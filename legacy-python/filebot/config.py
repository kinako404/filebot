"""配置读取：TOML 文件 + 命令行覆盖 + 环境变量覆盖。

优先级：命令行 > 环境变量 > 配置文件 > 默认值。
"""

from __future__ import annotations

import os
import tomllib
from dataclasses import dataclass, field

DEFAULT_API_BASE = "https://api.telegram.org"
DEFAULT_STATE_FILE = "~/.local/state/filebot/state.json"
DEFAULT_POLL_INTERVAL = 2.0
DEFAULT_SETTLE_SECONDS = 3.0
DEFAULT_MIN_SIZE = 1024
DEFAULT_PHOTO_MAX_MB = 10.0
DEFAULT_VIDEO_MAX_MB = 50.0
DEFAULT_MAX_UPLOAD_MB = 50.0
DEFAULT_CAPTION = "{name}\n{size} · {path}"
DEFAULT_EXCLUDES = ["*.part", "*.tmp", "*.crdownload", "*.download", "*.partial"]

WATCH_ALIASES = {"exclude": "excludes"}


class ConfigError(Exception):
    """配置缺失或非法。"""


@dataclass
class TelegramConfig:
    token: str = ""
    chat_id: str = ""
    api_base: str = DEFAULT_API_BASE
    timeout: float = 120.0
    max_retries: int = 3
    min_send_interval: float = 1.0
    insecure: bool = False


@dataclass
class WatchConfig:
    dirs: list[str] = field(default_factory=list)
    poll_interval: float = DEFAULT_POLL_INTERVAL
    settle_seconds: float = DEFAULT_SETTLE_SECONDS
    min_size: int = DEFAULT_MIN_SIZE
    excludes: list[str] = field(default_factory=lambda: list(DEFAULT_EXCLUDES))
    scan_existing: bool = False
    state_file: str = DEFAULT_STATE_FILE


@dataclass
class UploadConfig:
    send_original: bool = True
    send_compressed: bool = True
    max_upload_mb: float = DEFAULT_MAX_UPLOAD_MB
    photo_max_mb: float = DEFAULT_PHOTO_MAX_MB
    video_max_mb: float = DEFAULT_VIDEO_MAX_MB
    caption_template: str = DEFAULT_CAPTION
    notify_skipped: bool = False

    @property
    def max_upload_bytes(self) -> int:
        return int(self.max_upload_mb * 1024 * 1024)

    @property
    def photo_max_bytes(self) -> int:
        return int(self.photo_max_mb * 1024 * 1024)

    @property
    def video_max_bytes(self) -> int:
        return int(self.video_max_mb * 1024 * 1024)


@dataclass
class Config:
    telegram: TelegramConfig = field(default_factory=TelegramConfig)
    watch: WatchConfig = field(default_factory=WatchConfig)
    upload: UploadConfig = field(default_factory=UploadConfig)
    proxy: str = ""
    log_level: str = "INFO"
    workers: int = 1

    def validate(self) -> "Config":
        problems = []
        if not self.telegram.token:
            problems.append("缺少 telegram.token（或环境变量 FILEBOT_TG_TOKEN）")
        if not self.telegram.chat_id:
            problems.append("缺少 telegram.chat_id（或环境变量 FILEBOT_TG_CHAT_ID）")
        if not self.watch.dirs:
            problems.append("缺少 watch.dirs（或环境变量 FILEBOT_WATCH_DIRS）")
        if self.upload.max_upload_mb <= 0:
            problems.append("upload.max_upload_mb 必须大于 0")
        if problems:
            raise ConfigError("；".join(problems))
        return self

    def missing_dirs(self) -> list[str]:
        return [d for d in self.watch.dirs if not os.path.isdir(os.path.expanduser(d))]


def _as_bool_env(value: str) -> bool:
    return value.strip().lower() in ("1", "true", "yes", "on")


def _apply_env(config: Config) -> Config:
    telegram, watch, upload = config.telegram, config.watch, config.upload
    if os.environ.get("FILEBOT_TG_TOKEN"):
        telegram.token = os.environ["FILEBOT_TG_TOKEN"].strip()
    if os.environ.get("FILEBOT_TG_CHAT_ID"):
        telegram.chat_id = os.environ["FILEBOT_TG_CHAT_ID"].strip()
    if os.environ.get("FILEBOT_TG_API_BASE"):
        telegram.api_base = os.environ["FILEBOT_TG_API_BASE"].strip()
    if os.environ.get("FILEBOT_PROXY"):
        config.proxy = os.environ["FILEBOT_PROXY"].strip()
    if os.environ.get("FILEBOT_WATCH_DIRS"):
        watch.dirs = [
            item for item in os.environ["FILEBOT_WATCH_DIRS"].split(os.pathsep) if item.strip()
        ]
    if os.environ.get("FILEBOT_STATE_FILE"):
        watch.state_file = os.environ["FILEBOT_STATE_FILE"].strip()
    if os.environ.get("FILEBOT_SCAN_EXISTING"):
        watch.scan_existing = _as_bool_env(os.environ["FILEBOT_SCAN_EXISTING"])
    if os.environ.get("FILEBOT_LOG_LEVEL"):
        config.log_level = os.environ["FILEBOT_LOG_LEVEL"].strip().upper()
    return config


def load(path: str | None = None, overrides: dict | None = None) -> Config:
    """读取配置；``path`` 为空时只使用默认值 + 环境变量 + overrides。"""
    config = Config()
    if path:
        expanded = os.path.expanduser(path)
        if not os.path.exists(expanded):
            raise ConfigError(f"配置文件不存在：{expanded}")
        try:
            with open(expanded, "rb") as handle:
                raw = tomllib.load(handle)
        except (OSError, tomllib.TOMLDecodeError) as exc:
            raise ConfigError(f"读取配置文件失败：{exc}") from exc

        telegram = raw.get("telegram") or {}
        for key, value in telegram.items():
            if not hasattr(config.telegram, key):
                raise ConfigError(f"未知配置项 telegram.{key}")
            setattr(config.telegram, key, value)

        watch = raw.get("watch") or {}
        for key, value in watch.items():
            key = WATCH_ALIASES.get(key, key)
            if key == "dirs":
                config.watch.dirs = [str(item) for item in value]
            elif hasattr(config.watch, key):
                setattr(config.watch, key, value)
            else:
                raise ConfigError(f"未知配置项 watch.{key}")

        upload = raw.get("upload") or {}
        for key, value in upload.items():
            if not hasattr(config.upload, key):
                raise ConfigError(f"未知配置项 upload.{key}")
            setattr(config.upload, key, value)

        proxy = raw.get("proxy") or {}
        if isinstance(proxy, str):
            config.proxy = proxy
        else:
            config.proxy = str(proxy.get("url", "") or "")
            if proxy.get("url") is None and proxy:
                unknown = set(proxy) - {"url"}
                if unknown:
                    raise ConfigError(f"未知配置项 proxy.{sorted(unknown)[0]}")

        if "workers" in raw:
            config.workers = int(raw["workers"])
        if "log_level" in raw:
            config.log_level = str(raw["log_level"]).upper()

    _apply_env(config)

    for key, value in (overrides or {}).items():
        if value is None:
            continue
        if key in ("token", "chat_id", "api_base"):
            setattr(config.telegram, key, value)
        elif key == "proxy":
            config.proxy = value
        elif key == "dirs":
            config.watch.dirs = list(value)
        elif key == "state_file":
            config.watch.state_file = value
        elif key == "scan_existing":
            config.watch.scan_existing = bool(value)
        elif key == "log_level":
            config.log_level = str(value).upper()
        elif key == "workers":
            config.workers = int(value)
        elif hasattr(config.watch, key):
            setattr(config.watch, key, value)
        elif hasattr(config.upload, key):
            setattr(config.upload, key, value)
        else:
            raise ConfigError(f"未知的命令行配置：{key}")

    config.watch.dirs = [os.path.expanduser(d) for d in config.watch.dirs]
    config.watch.state_file = os.path.expanduser(config.watch.state_file)
    return config
