"""目录监控：递归轮询快照 + 去抖，避免把"正在拷贝"的文件当成新文件。

不依赖 inotify / watchdog：轮询对拷贝中的大文件天然友好（大小或 mtime 一变就
重新计时），也不受容器/网络文件系统限制。
"""

from __future__ import annotations

import fnmatch
import logging
import os
import stat
import time
from dataclasses import dataclass

log = logging.getLogger(__name__)

TEMP_SUFFIXES = (".part", ".tmp", ".crdownload", ".download", ".partial", ".filepart", ".swp")


@dataclass(frozen=True)
class PendingFile:
    path: str
    mtime_ns: int
    size: int


def is_hidden(name: str) -> bool:
    return name.startswith(".")


def should_skip(name: str) -> bool:
    return is_hidden(name) or name.endswith("~") or name.lower().endswith(TEMP_SUFFIXES)


def matches_any(path: str, patterns: list[str]) -> bool:
    name = os.path.basename(path)
    for pattern in patterns:
        if fnmatch.fnmatch(name, pattern) or fnmatch.fnmatch(path, pattern):
            return True
    return False


def scan(dirs: list[str], excludes: list[str] | None = None) -> dict[str, tuple[int, int]]:
    """递归扫描目录，返回 ``{路径: (mtime_ns, size)}``。"""
    excludes = excludes or []
    found: dict[str, tuple[int, int]] = {}
    for root_dir in dirs:
        for current, subdirs, filenames in os.walk(root_dir, followlinks=False):
            subdirs[:] = [
                d for d in subdirs if not is_hidden(d) and not should_skip(d)
            ]
            for name in filenames:
                if should_skip(name):
                    continue
                path = os.path.join(current, name)
                if matches_any(path, excludes):
                    continue
                try:
                    info = os.stat(path, follow_symlinks=True)
                except OSError:
                    continue
                if not stat.S_ISREG(info.st_mode):
                    continue
                found[path] = (info.st_mtime_ns, info.st_size)
    return found


class DirectoryWatcher:
    def __init__(
        self,
        dirs: list[str],
        poll_interval: float = 2.0,
        settle_seconds: float = 3.0,
        min_size: int = 1024,
        excludes: list[str] | None = None,
        scan_existing: bool = False,
    ) -> None:
        if not dirs:
            raise ValueError("至少要配置一个监控目录")
        self.dirs = [os.path.abspath(os.path.expanduser(d)) for d in dirs]
        self.poll_interval = max(0.1, poll_interval)
        self.settle_seconds = max(0.0, settle_seconds)
        self.min_size = max(0, min_size)
        self.excludes = excludes or []
        self.scan_existing = scan_existing
        self._known: dict[str, tuple[int, int]] | None = None
        self._pending: dict[str, tuple[tuple[int, int], float]] = {}

    def snapshot(self) -> dict[str, tuple[int, int]]:
        return scan(self.dirs, self.excludes)

    def poll(self, now: float | None = None) -> list[PendingFile]:
        """扫描一次，返回已经稳定下来、可以发送的文件。"""
        now = time.monotonic() if now is None else now
        current = self.snapshot()

        if self._known is None:
            self._known = {} if self.scan_existing else dict(current)
            if self.scan_existing:
                log.info("首次扫描：把 %d 个已存在的文件视为新文件", len(current))

        for path, signature in current.items():
            if self._known.get(path) != signature:
                self._pending[path] = (signature, now)

        self._known = current

        ready: list[PendingFile] = []
        for path, (signature, seen_at) in list(self._pending.items()):
            if path not in current:
                del self._pending[path]  # 被删除或改名
                continue
            if current[path] != signature:
                continue  # 已在上面重置计时
            if now - seen_at < self.settle_seconds:
                continue
            mtime_ns, size = signature
            del self._pending[path]
            if size < self.min_size:
                log.debug("跳过过小的文件：%s（%d 字节）", path, size)
                continue
            ready.append(PendingFile(path=path, mtime_ns=mtime_ns, size=size))
        return ready

    def run(self, on_files, stop_event, on_error=None) -> None:
        """轮询直到 stop_event 被设置；``on_files`` 收到一批 PendingFile。"""
        while not stop_event.is_set():
            try:
                ready = self.poll()
            except Exception as exc:  # noqa: BLE001 - 轮询失败不应终止进程
                log.exception("扫描目录失败：%s", exc)
                if on_error:
                    on_error(exc)
                ready = []
            if ready:
                on_files(ready)
            stop_event.wait(self.poll_interval)
