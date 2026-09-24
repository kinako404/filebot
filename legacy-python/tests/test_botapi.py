"""Bot API 客户端：multipart 组装、重试策略、错误处理。"""

from __future__ import annotations

import os
import tempfile
import unittest

from filebot.botapi import CAPTION_LIMIT, TelegramClient, TelegramError, _FilePart, _MultipartBody
from tests.helpers import FakeTelegram


class TestMultipartBody(unittest.TestCase):
    def test_content_length_matches_serialized_size(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "样本 文件.bin")
            with open(path, "wb") as handle:
                handle.write(os.urandom(5000))
            body = _MultipartBody(
                boundary="test-boundary",
                fields={"chat_id": "123", "caption": "hello"},
                files=[_FilePart("document", path, "样本 文件.bin", "application/octet-stream")],
            )
            serialized = b"".join(body)
            self.assertEqual(len(serialized), body.content_length)
            self.assertIn(b'name="chat_id"', serialized)
            self.assertIn("样本 文件.bin".encode(), serialized)
            self.assertTrue(serialized.endswith(b"--test-boundary--\r\n"))

    def test_filename_escaping(self):
        body = _MultipartBody(
            boundary="b",
            fields={},
            files=[_FilePart("document", __file__, 'we"ird\\name.jpg', "image/jpeg")],
        )
        serialized = b"".join(body)
        self.assertIn(b'filename="we%22ird%5Cname.jpg"', serialized)


class TelegramClientTestBase(unittest.TestCase):
    def setUp(self):
        self.fake = FakeTelegram().start()
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.addCleanup(self.fake.stop)

    def make_client(self, **kwargs) -> TelegramClient:
        options = {"token": "test:token", "api_base": self.fake.url, "min_send_interval": 0}
        options.update(kwargs)
        return TelegramClient(**options)

    def write_file(self, name: str = "clip.mp4", size: int = 2048) -> str:
        path = os.path.join(self.tmp.name, name)
        with open(path, "wb") as handle:
            handle.write(bytes(range(256)) * (size // 256) + os.urandom(size % 256))
        return path


class TestSendMethods(TelegramClientTestBase):
    def test_get_me(self):
        client = self.make_client()
        self.assertEqual(client.get_me()["username"], "filebot_test")

    def test_send_document_sends_exact_bytes(self):
        client = self.make_client()
        path = self.write_file("报告.pdf", 3000)
        with open(path, "rb") as handle:
            expected = handle.read()
        client.send_document("-100123", path, "标题")
        call = self.fake.calls_of("sendDocument")[0]
        self.assertEqual(call["fields"]["chat_id"], "-100123")
        self.assertEqual(call["fields"]["caption"], "标题")
        filename, payload = call["files"]["document"]
        self.assertEqual(filename, "报告.pdf")
        self.assertEqual(payload, expected)

    def test_send_photo_uses_photo_field(self):
        client = self.make_client()
        path = self.write_file("a.jpg", 1500)
        client.send_photo("@channel", path, None)
        call = self.fake.calls_of("sendPhoto")[0]
        self.assertEqual(call["fields"]["chat_id"], "@channel")
        self.assertNotIn("caption", call["fields"])
        self.assertIn("photo", call["files"])

    def test_send_video_enables_streaming(self):
        client = self.make_client()
        path = self.write_file("a.mp4", 1500)
        client.send_video("1", path, "cap")
        call = self.fake.calls_of("sendVideo")[0]
        self.assertEqual(call["fields"]["supports_streaming"], "true")
        self.assertIn("video", call["files"])

    def test_send_animation(self):
        client = self.make_client()
        path = self.write_file("a.gif", 1500)
        client.send_animation("1", path)
        self.assertIn("animation", self.fake.calls_of("sendAnimation")[0]["files"])

    def test_caption_is_truncated(self):
        client = self.make_client()
        path = self.write_file("a.jpg", 1200)
        client.send_photo("1", path, "x" * 5000)
        self.assertEqual(len(self.fake.calls_of("sendPhoto")[0]["fields"]["caption"]), CAPTION_LIMIT)


class TestRetryAndErrors(TelegramClientTestBase):
    def test_retries_on_429_then_succeeds(self):
        client = self.make_client()
        self.fake.script = [
            (429, {"ok": False, "error_code": 429, "description": "Too Many Requests",
                   "parameters": {"retry_after": 0}}),
        ]
        path = self.write_file("a.jpg", 1200)
        client.send_photo("1", path)
        self.assertEqual(self.fake.methods(), ["sendPhoto", "sendPhoto"])

    def test_retries_on_5xx(self):
        client = self.make_client()
        self.fake.script = [(500, {"ok": False, "description": "boom"})]
        path = self.write_file("a.jpg", 1200)
        client.send_photo("1", path)
        self.assertEqual(len(self.fake.calls), 2)

    def test_client_error_is_not_retried(self):
        client = self.make_client()
        self.fake.script = [
            (400, {"ok": False, "error_code": 400, "description": "Bad Request: chat not found"}),
        ]
        path = self.write_file("a.jpg", 1200)
        with self.assertRaises(TelegramError) as ctx:
            client.send_photo("1", path)
        self.assertIn("chat not found", str(ctx.exception))
        self.assertEqual(len(self.fake.calls), 1)

    def test_connection_error_raises_after_retries(self):
        client = self.make_client(api_base="http://127.0.0.1:1", max_retries=1)
        path = self.write_file("a.jpg", 1200)
        with self.assertRaises(TelegramError):
            client.send_photo("1", path)

    def test_invalid_api_base(self):
        with self.assertRaises(ValueError):
            TelegramClient(token="t", api_base="ftp://example.com")


if __name__ == "__main__":
    unittest.main()
