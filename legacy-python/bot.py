#!/usr/bin/env python3
"""入口脚本：python3 bot.py -c config.toml"""

import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from filebot.cli import main  # noqa: E402

if __name__ == "__main__":
    sys.exit(main())
