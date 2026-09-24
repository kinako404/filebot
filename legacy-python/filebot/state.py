"""已发送记录：重启后不重复推送同一个文件版本。"""

from __future__ import annotations

import json
import logging
import os
import tempfile

log = logging.getLogger(__name__)

MAX_ENTRIES = 20000
PRUNE_TO = 10000


class SendState:
    def __init__(self, path: str | None) -> None:
        self.path = os.path.expanduser(path) if path else None
        self._entries: dict[str, tuple[int, int]] = {}
        self._dirty = False

    def load(self) -> None:
        if not self.path or not os.path.exists(self.path):
            return
        try:
            with open(self.path, "r", encoding="utf-8") as handle:
                raw = json.load(handle)
            for key, value in (raw.get("files") or {}).items():
                self._entries[key] = (int(value[0]), int(value[1]))
            log.info("已载入发送记录 %d 条：%s", len(self._entries), self.path)
        except (OSError, ValueError, TypeError, IndexError) as exc:
            log.warning("读取状态文件失败（忽略）：%s", exc)

    def is_sent(self, path: str, mtime_ns: int, size: int) -> bool:
        return self._entries.get(path) == (mtime_ns, size)

    def mark_sent(self, path: str, mtime_ns: int, size: int) -> None:
        self._entries.pop(path, None)
        self._entries[path] = (mtime_ns, size)
        self._dirty = True
        if len(self._entries) > MAX_ENTRIES:
            for key in list(self._entries)[: len(self._entries) - PRUNE_TO]:
                del self._entries[key]

    def save(self) -> None:
        if not self.path or not self._dirty:
            return
        payload = {
            "version": 1,
            "files": {k: list(v) for k, v in self._entries.items()},
        }
        directory = os.path.dirname(self.path) or "."
        try:
            os.makedirs(directory, exist_ok=True)
            handle = tempfile.NamedTemporaryFile(
                "w", encoding="utf-8", dir=directory, prefix=".state-", delete=False
            )
            with handle:
                json.dump(payload, handle)
            os.replace(handle.name, self.path)
            self._dirty = False
        except OSError as exc:
            log.warning("写入状态文件失败：%s", exc)
