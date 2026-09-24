"""端到端：真实目录 → 监控 → 分类/压缩 → Bot API（假服务端），并覆盖走 ss 代理的链路。"""

from __future__ import annotations

import os
import shutil
import tempfile
import unittest

from filebot.app import FileBot, human_size, render_caption
from filebot.config import load
from tests.helpers import FakeShadowsocksServer, FakeTelegram
from tests.test_media import make_stub_tool


class TestHelpers(unittest.TestCase):
    def test_human_size(self):
        self.assertEqual(human_size(512), "512 B")
        self.assertEqual(human_size(2048), "2.0 KB")
        self.assertEqual(human_size(5 * 1024 * 1024), "5.0 MB")

    def test_render_caption(self):
        caption = render_caption("{name} | {size} | {kind}", "/data/a.mp4", 2048, "video")
        self.assertEqual(caption, "a.mp4 | 2.0 KB | video")

    def test_render_caption_falls_back_on_bad_template(self):
        self.assertEqual(render_caption("{nope}", "/data/a.mp4", 1, "video"), "a.mp4")


class E2ETestBase(unittest.TestCase):
    def setUp(self):
        self.fake = FakeTelegram().start()
        self.addCleanup(self.fake.stop)
        self.work = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.work, True)
        self.media_dir = os.path.join(self.work, "media")
        self.sub_dir = os.path.join(self.media_dir, "sub")
        os.makedirs(self.sub_dir)
        self.state_file = os.path.join(self.work, "state.json")

    def write(self, relative: str, size: int, payload: bytes | None = None) -> str:
        path = os.path.join(self.media_dir, relative)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "wb") as handle:
            handle.write(payload if payload is not None else bytes(range(256)) * (size // 256))
        return path

    def build_config(self, **overrides):
        options = {
            "token": "test:token",
            "chat_id": "-100999",
            "api_base": self.fake.url,
            "dirs": [self.media_dir],
            "state_file": self.state_file,
            "min_size": 0,
        }
        options.update(overrides)
        return load(None, options)

    def run_bot(self, config) -> FileBot:
        bot = FileBot(config)
        self.addCleanup(bot.close)
        bot.run_once()
        return bot


class TestEndToEndDirect(E2ETestBase):
    def test_sends_original_and_inline_variants(self):
        jpeg = self.write("pic.jpg", 2048)
        video = self.write("sub/clip.mp4", 4096)
        with open(jpeg, "rb") as handle:
            jpeg_bytes = handle.read()

        self.run_bot(self.build_config())

        self.assertEqual(self.fake.methods().count("sendDocument"), 2)
        self.assertEqual(self.fake.methods().count("sendPhoto"), 1)
        self.assertEqual(self.fake.methods().count("sendVideo"), 1)

        document_call = [
            call for call in self.fake.calls_of("sendDocument")
            if call["files"]["document"][0] == "pic.jpg"
        ][0]
        self.assertEqual(document_call["files"]["document"][1], jpeg_bytes)
        self.assertEqual(document_call["fields"]["chat_id"], "-100999")
        self.assertIn("pic.jpg", document_call["fields"]["caption"])

        photo_call = self.fake.calls_of("sendPhoto")[0]
        self.assertEqual(photo_call["files"]["photo"], ("pic.jpg", jpeg_bytes))

        video_call = self.fake.calls_of("sendVideo")[0]
        self.assertEqual(video_call["fields"]["supports_streaming"], "true")
        self.assertEqual(video_call["files"]["video"][0], "clip.mp4")
        # 原文件已经在 sendDocument 里发过，压缩版不再重复贴 caption
        self.assertNotIn("caption", photo_call["fields"])
        self.assertNotIn("caption", video_call["fields"])

    def test_non_media_files_are_ignored(self):
        self.write("notes.txt", 2048)
        self.run_bot(self.build_config())
        self.assertEqual(self.fake.calls, [])

    def test_state_file_prevents_resending(self):
        self.write("pic.jpg", 2048)
        self.run_bot(self.build_config())
        first_round = len(self.fake.calls)

        self.run_bot(self.build_config())
        self.assertEqual(len(self.fake.calls), first_round)
        self.assertTrue(os.path.exists(self.state_file))

    def test_modified_file_is_resent(self):
        path = self.write("pic.jpg", 2048)
        self.run_bot(self.build_config())
        before = len(self.fake.calls)

        with open(path, "ab") as handle:
            handle.write(b"tail")
        self.run_bot(self.build_config())
        self.assertGreater(len(self.fake.calls), before)

    def test_document_skipped_when_over_limit_but_inline_still_sent(self):
        self.write("big.jpg", 5000)
        self.run_bot(self.build_config(max_upload_mb=0.001))
        self.assertEqual(self.fake.methods().count("sendDocument"), 0)
        self.assertEqual(self.fake.methods().count("sendPhoto"), 1)

    def test_no_compressed_flag(self):
        self.write("pic.jpg", 2048)
        self.run_bot(self.build_config(send_compressed=False))
        self.assertEqual(self.fake.methods().count("sendDocument"), 1)
        self.assertEqual(self.fake.methods().count("sendPhoto"), 0)

    def test_no_original_flag_puts_caption_on_inline_message(self):
        self.write("pic.jpg", 2048)
        self.run_bot(self.build_config(send_original=False))
        self.assertEqual(self.fake.methods().count("sendDocument"), 0)
        photo_call = self.fake.calls_of("sendPhoto")[0]
        self.assertIn("pic.jpg", photo_call["fields"]["caption"])

    def test_converted_image_is_sent_and_temp_file_removed(self):
        stub_dir = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, stub_dir, True)
        make_stub_tool(stub_dir)
        original_path = os.environ.get("PATH", "")
        os.environ["PATH"] = stub_dir + os.pathsep + original_path
        self.addCleanup(os.environ.__setitem__, "PATH", original_path)

        bmp = self.write("scan.bmp", 3000)
        with open(bmp, "rb") as handle:
            bmp_bytes = handle.read()

        bot = FileBot(self.build_config())
        self.addCleanup(bot.close)
        workdir = bot._preparer.workdir  # noqa: SLF001 - 测试用
        bot.run_once()

        self.assertEqual(self.fake.methods().count("sendDocument"), 1)
        self.assertEqual(self.fake.methods().count("sendPhoto"), 1)
        self.assertEqual(self.fake.calls_of("sendPhoto")[0]["files"]["photo"][1], bmp_bytes)
        # 转换产生的临时文件发送后应被清理
        leftovers = [] if not os.path.isdir(workdir) else os.listdir(workdir)
        self.assertEqual(leftovers, [])


class TestLiveRunMode(E2ETestBase):
    """常驻模式：run() 在后台线程里跑，新文件要能实时发出去并优雅退出。"""

    def wait_for(self, predicate, timeout: float = 30.0) -> bool:
        import time

        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if predicate():
                return True
            time.sleep(0.1)
        return predicate()

    def test_files_are_sent_while_running(self):
        import threading

        self.write("first.jpg", 2048)
        config = self.build_config(poll_interval=0.05, settle_seconds=0.1, scan_existing=True)
        bot = FileBot(config)
        self.addCleanup(bot.close)
        thread = threading.Thread(target=bot.run, daemon=True)
        thread.start()
        try:
            self.assertTrue(
                self.wait_for(lambda: len(self.fake.calls_of("sendPhoto")) >= 1),
                "启动后已有文件没有被发送",
            )
            # 运行中新增的文件同样要被发现
            self.write("second.jpg", 3000)
            self.assertTrue(
                self.wait_for(lambda: len(self.fake.calls_of("sendPhoto")) >= 2),
                "运行中新增的文件没有被发送",
            )
        finally:
            bot.stop()
            thread.join(timeout=30)
        self.assertFalse(thread.is_alive(), "stop() 之后 run() 应该退出")
        self.assertTrue(os.path.exists(self.state_file))

    def test_worker_survives_telegram_error(self):
        import threading

        self.write("bad.jpg", 2048)
        self.fake.method_scripts = {
            "sendDocument": [
                (400, {"ok": False, "error_code": 400, "description": "chat not found"}),
            ]
        }
        config = self.build_config(poll_interval=0.05, settle_seconds=0.1, scan_existing=True)
        bot = FileBot(config)
        self.addCleanup(bot.close)
        thread = threading.Thread(target=bot.run, daemon=True)
        thread.start()
        try:
            # 第一次 sendDocument 失败 → 重试第二次，worker 不会因此死掉
            self.assertTrue(
                self.wait_for(lambda: len(self.fake.calls_of("sendDocument")) >= 2),
                "失败后的重试没有发生",
            )
            self.assertTrue(thread.is_alive())
        finally:
            bot.stop()
            thread.join(timeout=30)
        self.assertFalse(thread.is_alive())


class TestEndToEndThroughShadowsocks(E2ETestBase):
    def test_traffic_goes_through_ss_proxy(self):
        ss = FakeShadowsocksServer(method="aes-256-gcm", password="pw").start()
        self.addCleanup(ss.stop)
        proxy = f"ss://aes-256-gcm:pw@{ss.host}:{ss.port}"

        self.write("pic.jpg", 2048)
        self.write("sub/clip.mp4", 4096)

        self.run_bot(self.build_config(proxy=proxy))

        self.assertEqual(self.fake.methods().count("sendDocument"), 2)
        self.assertEqual(self.fake.methods().count("sendPhoto"), 1)
        self.assertEqual(self.fake.methods().count("sendVideo"), 1)

        tg_host = self.fake.url.split("//")[1].split(":")
        self.assertIn(("127.0.0.1", int(tg_host[1])), ss.targets)

    def test_socks5_proxy_works_too(self):
        from tests.helpers import FakeSocks5Server

        socks = FakeSocks5Server().start()
        self.addCleanup(socks.stop)
        self.write("pic.jpg", 2048)

        self.run_bot(self.build_config(proxy=f"socks5://{socks.host}:{socks.port}"))

        self.assertEqual(self.fake.methods().count("sendDocument"), 1)
        self.assertTrue(socks.targets)


if __name__ == "__main__":
    unittest.main()
