"""命令行入口。"""

from __future__ import annotations

import argparse
import logging
import os
import sys

from . import __version__
from .app import FileBot
from .config import ConfigError, load

EXAMPLES = """示例：
  # 用配置文件运行
  python3 bot.py -c config.toml

  # 只用环境变量 / 参数
  FILEBOT_TG_TOKEN=123:abc FILEBOT_TG_CHAT_ID=@mychannel \\
  FILEBOT_WATCH_DIRS=/data/media python3 bot.py --proxy 'ss://...'

  # 自检（验证 token、代理、chat_id 是否可用）
  python3 bot.py -c config.toml --check

  # 补发目录里已有的文件后退出
  python3 bot.py -c config.toml --once
"""


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="bot.py",
        description="监控目录并把新增/更新的图片、视频发送到 Telegram（原文件 + 可点开的压缩版）",
        epilog=EXAMPLES,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument("-c", "--config", help="TOML 配置文件路径")
    parser.add_argument("--token", help="Telegram Bot token")
    parser.add_argument("--chat-id", dest="chat_id", help="接收方 chat_id 或 @channel")
    parser.add_argument("--api-base", dest="api_base", help="Bot API 地址（自建 server 可改）")
    parser.add_argument("--proxy", help="ss:// / socks5:// / http:// 代理链接，留空为直连")
    parser.add_argument("-d", "--dir", action="append", dest="dirs", help="监控目录，可重复")
    parser.add_argument("--state-file", dest="state_file", help="发送记录文件路径")
    parser.add_argument("--scan-existing", action="store_true", default=None,
                        help="启动时把目录里已有文件也发送一遍")
    parser.add_argument("--once", action="store_true", help="处理完现有文件后退出")
    parser.add_argument("--check", action="store_true", help="只做连接自检，然后退出")
    parser.add_argument("--poll-interval", type=float, help="扫描间隔秒数（默认 2）")
    parser.add_argument("--settle-seconds", type=float, help="文件稳定多少秒后才发送（默认 3）")
    parser.add_argument("--min-size", type=int, help="小于该字节数的文件忽略（默认 1024）")
    parser.add_argument("--max-upload-mb", type=float, help="原文件上传上限 MB（默认 50）")
    parser.add_argument("--photo-max-mb", type=float, help="图片内联上限 MB（默认 10）")
    parser.add_argument("--video-max-mb", type=float, help="视频内联上限 MB（默认 50）")
    parser.add_argument("--workers", type=int, help="发送线程数（默认 1）")
    parser.add_argument("--no-original", action="store_true", help="不发送原文件")
    parser.add_argument("--no-compressed", action="store_true", help="不发送压缩版")
    parser.add_argument("--log-level", help="DEBUG/INFO/WARNING/ERROR")
    parser.add_argument("--version", action="version", version=f"filebot {__version__}")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)-7s %(name)s: %(message)s",
        datefmt="%Y-%m-%d %H:%M:%S",
    )

    overrides = {
        "token": args.token,
        "chat_id": args.chat_id,
        "api_base": args.api_base,
        "proxy": args.proxy,
        "dirs": args.dirs,
        "state_file": args.state_file,
        "scan_existing": args.scan_existing,
        "poll_interval": args.poll_interval,
        "settle_seconds": args.settle_seconds,
        "min_size": args.min_size,
        "max_upload_mb": args.max_upload_mb,
        "photo_max_mb": args.photo_max_mb,
        "video_max_mb": args.video_max_mb,
        "workers": args.workers,
        "log_level": args.log_level,
    }
    if args.no_original:
        overrides["send_original"] = False
    if args.no_compressed:
        overrides["send_compressed"] = False

    try:
        config = load(args.config, overrides).validate()
    except ConfigError as exc:
        print(f"配置错误：{exc}", file=sys.stderr)
        return 2

    logging.getLogger().setLevel(getattr(logging, config.log_level, logging.INFO))

    for directory in config.watch.dirs:
        if not os.path.isdir(directory):
            logging.warning("监控目录不存在（挂载好之后会自动生效）：%s", directory)
    if config.watch.scan_existing:
        logging.info("scan_existing 已开启：启动时会把已有文件都发送一遍")

    bot = FileBot(config)
    try:
        if args.check:
            bot.setup()
            bot.check()
            bot.close()
            return 0
        if args.once:
            return bot.run_once()
        return bot.run()
    except KeyboardInterrupt:
        return 130
