"""配置读取：TOML、环境变量、命令行覆盖的优先级与校验。"""

from __future__ import annotations

import os
import tempfile
import unittest

from filebot.config import ConfigError, load

SAMPLE = """
[telegram]
token = "123:ABC"
chat_id = "@mychannel"
api_base = "https://api.telegram.org"
min_send_interval = 2.5

[proxy]
url = "ss://aes-256-gcm:pw@1.2.3.4:8388#node"

[watch]
dirs = ["/data/a", "/data/b"]
poll_interval = 5
settle_seconds = 1.5
min_size = 4096
exclude = ["*.skip"]
scan_existing = true

[upload]
send_original = true
send_compressed = false
max_upload_mb = 2000
caption_template = "{name} ({size})"
"""


class ConfigTestBase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.path = os.path.join(self.tmp.name, "config.toml")
        self._env_backup = {
            key: os.environ.pop(key)
            for key in list(os.environ)
            if key.startswith("FILEBOT_")
        }

        def restore():
            for key in list(os.environ):
                if key.startswith("FILEBOT_"):
                    del os.environ[key]
            os.environ.update(self._env_backup)

        self.addCleanup(restore)

    def write_config(self, text: str) -> str:
        with open(self.path, "w", encoding="utf-8") as handle:
            handle.write(text)
        return self.path


class TestTomlLoading(ConfigTestBase):
    def test_full_config(self):
        config = load(self.write_config(SAMPLE))
        self.assertEqual(config.telegram.token, "123:ABC")
        self.assertEqual(config.telegram.chat_id, "@mychannel")
        self.assertEqual(config.telegram.min_send_interval, 2.5)
        self.assertEqual(config.proxy, "ss://aes-256-gcm:pw@1.2.3.4:8388#node")
        self.assertEqual(config.watch.dirs, ["/data/a", "/data/b"])
        self.assertEqual(config.watch.poll_interval, 5)
        self.assertEqual(config.watch.min_size, 4096)
        self.assertTrue(config.watch.scan_existing)
        self.assertFalse(config.upload.send_compressed)
        self.assertEqual(config.upload.max_upload_mb, 2000)
        self.assertEqual(config.upload.max_upload_bytes, 2000 * 1024 * 1024)

    def test_validate_reports_missing_fields(self):
        with self.assertRaises(ConfigError) as ctx:
            load().validate()
        message = str(ctx.exception)
        self.assertIn("telegram.token", message)
        self.assertIn("telegram.chat_id", message)
        self.assertIn("watch.dirs", message)

    def test_unknown_key_is_rejected(self):
        with self.assertRaises(ConfigError):
            load(self.write_config("[telegram]\ntoken = \"x\"\nwat = 1\n"))

    def test_unknown_watch_key_is_rejected(self):
        with self.assertRaises(ConfigError):
            load(self.write_config("[watch]\ndir = [\"/tmp\"]\n"))

    def test_missing_file(self):
        with self.assertRaises(ConfigError):
            load("/definitely/missing.toml")

    def test_broken_toml(self):
        with self.assertRaises(ConfigError):
            load(self.write_config("[telegram\ntoken=\n"))

    def test_proxy_as_plain_string(self):
        config = load(self.write_config('proxy = "socks5://127.0.0.1:1080"\n'))
        self.assertEqual(config.proxy, "socks5://127.0.0.1:1080")

    def test_tilde_expansion(self):
        config = load(self.write_config('[watch]\ndirs = ["~/media"]\n'))
        self.assertTrue(config.watch.dirs[0].startswith(os.path.expanduser("~")))


class TestOverrides(ConfigTestBase):
    def test_env_overrides_file(self):
        os.environ["FILEBOT_TG_TOKEN"] = "env:TOKEN"
        os.environ["FILEBOT_PROXY"] = "ss://env"
        os.environ["FILEBOT_WATCH_DIRS"] = os.pathsep.join(["/env/one", "/env/two"])
        config = load(self.write_config(SAMPLE))
        self.assertEqual(config.telegram.token, "env:TOKEN")
        self.assertEqual(config.proxy, "ss://env")
        self.assertEqual(config.watch.dirs, ["/env/one", "/env/two"])

    def test_cli_overrides_env_and_file(self):
        os.environ["FILEBOT_TG_TOKEN"] = "env:TOKEN"
        config = load(
            self.write_config(SAMPLE),
            {"token": "cli:TOKEN", "chat_id": "-42", "dirs": ["/cli"], "none": None},
        )
        self.assertEqual(config.telegram.token, "cli:TOKEN")
        self.assertEqual(config.telegram.chat_id, "-42")
        self.assertEqual(config.watch.dirs, ["/cli"])

    def test_unknown_override_key(self):
        with self.assertRaises(ConfigError):
            load(None, {"nope": 1})

    def test_env_only_configuration(self):
        os.environ["FILEBOT_TG_TOKEN"] = "t"
        os.environ["FILEBOT_TG_CHAT_ID"] = "1"
        os.environ["FILEBOT_WATCH_DIRS"] = "/data"
        config = load().validate()
        self.assertEqual(config.watch.dirs, ["/data"])
        self.assertEqual(config.telegram.api_base, "https://api.telegram.org")

    def test_missing_dirs_helper(self):
        config = load(None, {"dirs": ["/no/such/dir"]})
        self.assertEqual(config.missing_dirs(), ["/no/such/dir"])


class TestDefaults(unittest.TestCase):
    def test_defaults_are_sane(self):
        config = load(None, {"dirs": ["/tmp"]})
        self.assertEqual(config.upload.max_upload_mb, 50.0)
        self.assertEqual(config.upload.photo_max_mb, 10.0)
        self.assertEqual(config.watch.poll_interval, 2.0)
        self.assertEqual(config.watch.settle_seconds, 3.0)
        self.assertTrue(config.upload.send_original)
        self.assertTrue(config.upload.send_compressed)
        self.assertEqual(config.workers, 1)
        self.assertIn("{name}", config.upload.caption_template)


if __name__ == "__main__":
    unittest.main()
