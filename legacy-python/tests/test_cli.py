"""真正跑一遍命令行入口（子进程），确保用户实际使用的方式可用。"""

from __future__ import annotations

import os
import subprocess
import sys
import tempfile
import unittest

from tests.helpers import FakeTelegram

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


class TestCli(unittest.TestCase):
    def setUp(self):
        self.fake = FakeTelegram().start()
        self.addCleanup(self.fake.stop)
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.media = os.path.join(self.tmp.name, "media")
        os.makedirs(self.media)
        self.state = os.path.join(self.tmp.name, "state.json")

    def write_media(self, name: str, size: int) -> str:
        path = os.path.join(self.media, name)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "wb") as handle:
            handle.write(b"m" * size)
        return path

    def write_config(self, extra: str = "") -> str:
        path = os.path.join(self.tmp.name, "config.toml")
        with open(path, "w", encoding="utf-8") as handle:
            handle.write(
                f"""
[telegram]
token = "cli:token"
chat_id = "-100777"
api_base = "{self.fake.url}"

[watch]
dirs = ["{self.media}"]
state_file = "{self.state}"
min_size = 0
scan_existing = true

[upload]
caption_template = "{{name}}"
{extra}
"""
            )
        return path

    def run_cli(self, *args: str) -> subprocess.CompletedProcess:
        env = dict(os.environ)
        env.pop("FILEBOT_TG_TOKEN", None)
        env.pop("FILEBOT_WATCH_DIRS", None)
        return subprocess.run(
            [sys.executable, os.path.join(ROOT, "bot.py"), *args],
            cwd=ROOT, env=env, capture_output=True, text=True, timeout=120,
        )

    def test_once_sends_files_and_exits(self):
        self.write_media("photo.jpg", 2048)
        self.write_media("sub/clip.mp4", 4096)

        result = self.run_cli("-c", self.write_config(), "--once")

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.fake.methods().count("sendDocument"), 2)
        self.assertEqual(self.fake.methods().count("sendPhoto"), 1)
        self.assertEqual(self.fake.methods().count("sendVideo"), 1)
        self.assertEqual(self.fake.calls_of("sendDocument")[0]["fields"]["chat_id"], "-100777")
        self.assertTrue(os.path.exists(self.state))

    def test_check_only_calls_get_me(self):
        result = self.run_cli("-c", self.write_config(), "--check")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.fake.methods(), ["getMe"])
        self.assertIn("filebot_test", result.stderr)

    def test_missing_token_exits_with_config_error(self):
        config = self.write_config()
        with open(config, "r", encoding="utf-8") as handle:
            text = handle.read().replace('token = "cli:token"', 'token = ""')
        with open(config, "w", encoding="utf-8") as handle:
            handle.write(text)

        env = dict(os.environ)
        env.pop("FILEBOT_TG_TOKEN", None)
        result = subprocess.run(
            [sys.executable, os.path.join(ROOT, "bot.py"), "-c", config, "--once"],
            cwd=ROOT, env=env, capture_output=True, text=True, timeout=60,
        )
        self.assertEqual(result.returncode, 2)
        self.assertIn("配置错误", result.stderr)

    def test_env_only_invocation(self):
        self.write_media("photo.jpg", 2048)
        env = dict(os.environ)
        env.update(
            {
                "FILEBOT_TG_TOKEN": "env:token",
                "FILEBOT_TG_CHAT_ID": "-100888",
                "FILEBOT_TG_API_BASE": self.fake.url,
                "FILEBOT_WATCH_DIRS": self.media,
                "FILEBOT_STATE_FILE": os.path.join(self.tmp.name, "state-env.json"),
            }
        )
        result = subprocess.run(
            [sys.executable, os.path.join(ROOT, "bot.py"), "--once", "--min-size", "0"],
            cwd=ROOT, env=env, capture_output=True, text=True, timeout=120,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.fake.methods().count("sendPhoto"), 1)
        self.assertEqual(self.fake.calls_of("sendPhoto")[0]["fields"]["chat_id"], "-100888")

    def test_no_compressed_flag_via_cli(self):
        self.write_media("photo.jpg", 2048)
        result = self.run_cli("-c", self.write_config(), "--once", "--no-compressed")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.fake.methods(), ["sendDocument"])

    def test_verbose_logs_show_progress(self):
        self.write_media("photo.jpg", 2048)
        result = self.run_cli("-c", self.write_config(), "--once", "--log-level", "DEBUG")
        self.assertIn("已发送原文件", result.stderr)

    def test_daemon_mode_sends_new_file_and_exits_on_sigterm(self):
        import signal
        import time

        env = dict(os.environ)
        env.pop("FILEBOT_TG_TOKEN", None)
        env.pop("FILEBOT_WATCH_DIRS", None)
        config = self.write_config()
        proc = subprocess.Popen(
            [
                sys.executable, os.path.join(ROOT, "bot.py"), "-c", config,
                "--poll-interval", "0.2", "--settle-seconds", "0.2",
            ],
            cwd=ROOT, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
        )
        try:
            deadline = time.monotonic() + 40
            while time.monotonic() < deadline and not self.fake.calls:
                self.assertIsNone(proc.poll(), "常驻进程意外退出")
                time.sleep(0.3)
            self.assertTrue(self.fake.calls, "常驻模式下新文件没有被发送")

            proc.send_signal(signal.SIGTERM)
            stdout, stderr = proc.communicate(timeout=40)
        finally:
            if proc.poll() is None:
                proc.kill()
                proc.communicate()
        self.assertEqual(proc.returncode, 0, stderr)
        self.assertIn("已退出", stderr)


if __name__ == "__main__":
    unittest.main()
