"""把监控、压缩、上传串起来的主程序。"""

from __future__ import annotations

import logging
import os
import queue
import signal
import threading
import time

from . import botapi, media
from .config import Config
from .proxy import build_proxy
from .state import SendState
from .watcher import DirectoryWatcher, PendingFile

log = logging.getLogger(__name__)

SEND_ATTEMPTS = 2
RETRY_DELAY = 5.0


def human_size(size: int) -> str:
    value = float(size)
    for unit in ("B", "KB", "MB", "GB"):
        if value < 1024 or unit == "GB":
            return f"{value:.1f} {unit}" if unit != "B" else f"{int(value)} B"
        value /= 1024
    return f"{value:.1f} GB"


def render_caption(template: str, path: str, size: int, kind: str | None) -> str:
    values = {
        "name": os.path.basename(path),
        "path": path,
        "dir": os.path.dirname(path),
        "size": human_size(size),
        "kind": kind or "other",
    }
    try:
        text = template.format(**values)
    except (KeyError, IndexError, ValueError) as exc:
        log.warning("caption 模板 %r 无法套用（%s），改用文件名", template, exc)
        text = values["name"]
    return text.strip()


class FileBot:
    def __init__(self, config: Config) -> None:
        self.config = config
        self._queue: queue.Queue[PendingFile | None] = queue.Queue(maxsize=1000)
        self._stop = threading.Event()
        self._proxy = None
        self._proxy_url: str | None = None
        self._preparer = media.MediaPreparer(
            photo_max_bytes=config.upload.photo_max_bytes,
            video_max_bytes=config.upload.video_max_bytes,
        )
        self._state = SendState(config.watch.state_file)
        self._client: botapi.TelegramClient | None = None
        self._workers: list[threading.Thread] = []

    # ------------------------------------------------------------------ #
    def setup(self) -> None:
        proxy_url, self._proxy = build_proxy(self.config.proxy)
        self._proxy_url = proxy_url
        if proxy_url:
            scheme = self.config.proxy.split("://", 1)[0]
            log.info("已启用代理：%s → %s", scheme, proxy_url)
        else:
            log.info("未配置代理，直连 Telegram")
        self._client = botapi.TelegramClient(
            token=self.config.telegram.token,
            api_base=self.config.telegram.api_base,
            proxy=proxy_url,
            timeout=self.config.telegram.timeout,
            max_retries=self.config.telegram.max_retries,
            min_send_interval=self.config.telegram.min_send_interval,
            insecure=self.config.telegram.insecure,
        )
        self._state.load()
        log.info("可用的转换工具：%s", self._preparer.capabilities())

    def check(self) -> dict:
        """连接性自检：走一遍 getMe。"""
        assert self._client is not None
        me = self._client.get_me()
        log.info("Bot 已就绪：@%s（id=%s）", me.get("username"), me.get("id"))
        return me

    # ------------------------------------------------------------------ #
    def _enqueue(self, items: list[PendingFile]) -> None:
        for item in items:
            try:
                self._queue.put(item, timeout=30)
            except queue.Full:
                log.error("发送队列已满，丢弃：%s", item.path)

    def stop(self) -> None:
        """请求退出（等价于收到 SIGINT/SIGTERM）。"""
        self._stop.set()

    def _handle_file(self, pending: PendingFile, done: set[str]) -> bool:
        """发送单个文件；``done`` 记录已成功的部分，重试时不会重复发送。

        返回是否全部成功。
        """
        assert self._client is not None
        path = pending.path
        kind = media.classify(path)
        if kind is None:
            log.debug("不是图片/视频，忽略：%s", path)
            done.update({"original", "compressed"})
            return True

        caption = render_caption(
            self.config.upload.caption_template, path, pending.size, kind
        )
        upload = self.config.upload

        if upload.send_original and "original" not in done:
            if pending.size <= upload.max_upload_bytes:
                self._client.send_document(self.config.telegram.chat_id, path, caption)
                log.info("已发送原文件：%s（%s）", path, human_size(pending.size))
            else:
                log.warning(
                    "原文件超过 Bot API 上传上限（%s > %s），跳过原文件：%s",
                    human_size(pending.size), human_size(upload.max_upload_bytes), path,
                )
                if upload.notify_skipped:
                    self._client.send_message(
                        self.config.telegram.chat_id,
                        f"原文件超过 {human_size(upload.max_upload_bytes)} 上限，未发送：\n"
                        f"{os.path.basename(path)}（{human_size(pending.size)}）",
                    )
            done.add("original")

        if upload.send_compressed and "compressed" not in done:
            prepared = self._preparer.prepare(path, kind)
            if prepared is None:
                log.info("无法生成可内联的副本（缺少转换工具或超限）：%s", path)
                done.add("compressed")
            else:
                try:
                    inline_caption = None if upload.send_original else caption
                    if prepared.kind == "photo":
                        self._client.send_photo(
                            self.config.telegram.chat_id, prepared.path, inline_caption
                        )
                    elif prepared.kind == "animation":
                        self._client.send_animation(
                            self.config.telegram.chat_id, prepared.path, inline_caption
                        )
                    else:
                        self._client.send_video(
                            self.config.telegram.chat_id, prepared.path, inline_caption
                        )
                    log.info(
                        "已发送压缩版（%s%s）：%s",
                        prepared.kind,
                        f", {prepared.note}" if prepared.note else "",
                        path,
                    )
                    done.add("compressed")
                finally:
                    if prepared.temporary:
                        try:
                            os.unlink(prepared.path)
                        except OSError:
                            pass

        expected = set()
        if upload.send_original:
            expected.add("original")
        if upload.send_compressed:
            expected.add("compressed")
        return expected <= done

    def _worker(self) -> None:
        while True:
            item = self._queue.get()
            try:
                if item is None:
                    return
                done: set[str] = set()
                for attempt in range(1, SEND_ATTEMPTS + 1):
                    try:
                        if self._handle_file(item, done):
                            break
                        log.error("部分内容发送失败：%s", item.path)
                        break
                    except botapi.TelegramError as exc:
                        log.error(
                            "发送 %s 失败（第 %d/%d 次）：%s",
                            item.path, attempt, SEND_ATTEMPTS, exc,
                        )
                        if attempt < SEND_ATTEMPTS and not self._stop.is_set():
                            self._stop.wait(RETRY_DELAY)
                    except Exception:  # noqa: BLE001 - 单个文件异常不能拖垮 worker
                        log.exception("处理 %s 时出现未预期错误", item.path)
                        break
                if "original" in done or "compressed" in done:
                    self._state.mark_sent(item.path, item.mtime_ns, item.size)
                    self._state.save()
            finally:
                self._queue.task_done()

    # ------------------------------------------------------------------ #
    def _start_workers(self) -> None:
        for index in range(max(1, self.config.workers)):
            thread = threading.Thread(
                target=self._worker, name=f"filebot-worker-{index}", daemon=True
            )
            thread.start()
            self._workers.append(thread)

    def _stop_workers(self, timeout: float = 120.0) -> None:
        for _ in self._workers:
            self._queue.put(None)
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if self._queue.unfinished_tasks == 0:
                break
            time.sleep(0.2)
        for thread in self._workers:
            thread.join(timeout=5)
        self._workers.clear()

    def _install_signal_handlers(self) -> None:
        def handler(signum, _frame):
            log.info("收到信号 %s，准备退出…", signum)
            self._stop.set()

        for sig in (signal.SIGINT, signal.SIGTERM):
            try:
                signal.signal(sig, handler)
            except (ValueError, OSError):  # 非主线程时忽略
                pass

    def _build_watcher(self, scan_existing: bool, settle: float | None = None) -> DirectoryWatcher:
        return DirectoryWatcher(
            dirs=self.config.watch.dirs,
            poll_interval=self.config.watch.poll_interval,
            settle_seconds=self.config.watch.settle_seconds if settle is None else settle,
            min_size=self.config.watch.min_size,
            excludes=self.config.watch.excludes,
            scan_existing=scan_existing,
        )

    def run_once(self) -> int:
        """把目录里现有文件处理一遍然后退出（主要用于测试/补发）。"""
        self.setup()
        watcher = self._build_watcher(scan_existing=True, settle=0.0)
        ready = [item for item in watcher.poll() if not self._state.is_sent(
            item.path, item.mtime_ns, item.size)]
        log.info("扫描到 %d 个待发送文件", len(ready))
        if not ready:
            self.close()
            return 0
        self._start_workers()
        self._enqueue(ready)
        self._stop_workers()
        self.close()
        return 0

    def run(self) -> int:
        self.setup()
        self.check()
        self._install_signal_handlers()
        self._start_workers()
        watcher = self._build_watcher(scan_existing=self.config.watch.scan_existing)

        def on_files(items: list[PendingFile]) -> None:
            fresh = [
                item for item in items
                if not self._state.is_sent(item.path, item.mtime_ns, item.size)
            ]
            if fresh:
                log.info("发现 %d 个新文件", len(fresh))
                self._enqueue(fresh)

        try:
            watcher.run(on_files, self._stop)
        finally:
            self._stop.set()
            self._stop_workers()
            self.close()
        log.info("已退出")
        return 0

    def close(self) -> None:
        self._state.save()
        if self._proxy is not None:
            self._proxy.stop()
            self._proxy = None
        self._preparer.close()
