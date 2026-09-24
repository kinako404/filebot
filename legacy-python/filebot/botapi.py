"""Telegram Bot API 客户端。

只用标准库 ``http.client``：手写 multipart/form-data（支持大文件流式上传，
不把整个文件读进内存），支持 HTTP(S) 代理（``set_tunnel``），带 429/5xx 重试。
"""

from __future__ import annotations

import http.client
import json
import logging
import mimetypes
import os
import ssl
import threading
import time
from dataclasses import dataclass
from urllib.parse import urlsplit

log = logging.getLogger(__name__)

CAPTION_LIMIT = 1024
DEFAULT_TIMEOUT = 120.0


class TelegramError(Exception):
    """Bot API 返回的业务错误（不重试）。"""

    def __init__(self, message: str, error_code: int | None = None, method: str = "") -> None:
        super().__init__(message)
        self.error_code = error_code
        self.method = method


class _Retryable(Exception):
    """可重试的临时故障。"""

    def __init__(self, message: str, delay: float | None = None) -> None:
        super().__init__(message)
        self.delay = delay


@dataclass
class _FilePart:
    field: str
    path: str
    filename: str
    content_type: str


def _escape_filename(name: str) -> str:
    cleaned = "".join(ch for ch in name if ch >= " " and ch != "\x7f")
    return cleaned.replace("\\", "%5C").replace('"', "%22")


def _format_field(boundary: bytes, name: str, value: str) -> bytes:
    return (
        b"--" + boundary + b"\r\n"
        b'Content-Disposition: form-data; name="' + name.encode() + b'"\r\n\r\n'
        + value.encode() + b"\r\n"
    )


def _format_file_header(part: _FilePart, boundary: bytes) -> bytes:
    return (
        b"--" + boundary + b"\r\n"
        b'Content-Disposition: form-data; name="' + part.field.encode() + b'"; filename="'
        + _escape_filename(part.filename).encode("utf-8") + b'"\r\n'
        b"Content-Type: " + part.content_type.encode() + b"\r\n\r\n"
    )


class _MultipartBody:
    """可迭代的 multipart/form-data 请求体（惰性读取文件）。"""

    def __init__(self, boundary: str, fields: dict[str, str], files: list[_FilePart]) -> None:
        self.boundary = boundary.encode()
        self.parts = [_format_field(self.boundary, key, value) for key, value in fields.items()]
        self.files = files
        self._fixed_length = sum(len(part) for part in self.parts)
        self._file_headers = []
        for part in files:
            header = _format_file_header(part, self.boundary)
            self._file_headers.append(header)
        for part, header in zip(files, self._file_headers):
            self._fixed_length += len(header) + 2  # 文件后的 \r\n
        self._fixed_length += len(b"--" + self.boundary + b"--\r\n")

    @property
    def content_type(self) -> str:
        return f"multipart/form-data; boundary={self.boundary.decode()}"

    @property
    def content_length(self) -> int:
        total = self._fixed_length
        for part in self.files:
            total += os.path.getsize(part.path)
        return total

    def __iter__(self):
        for part in self.parts:
            yield part
        for part, header in zip(self.files, self._file_headers):
            yield header
            with open(part.path, "rb") as handle:
                while True:
                    chunk = handle.read(256 * 1024)
                    if not chunk:
                        break
                    yield chunk
            yield b"\r\n"
        yield b"--" + self.boundary + b"--\r\n"


class TelegramClient:
    def __init__(
        self,
        token: str,
        api_base: str = "https://api.telegram.org",
        proxy: str | None = None,
        timeout: float = DEFAULT_TIMEOUT,
        max_retries: int = 3,
        min_send_interval: float = 1.0,
        insecure: bool = False,
    ) -> None:
        if not token:
            raise ValueError("缺少 Bot token")
        self._token = token
        self._api_base = api_base.rstrip("/")
        parts = urlsplit(self._api_base)
        if parts.scheme not in ("http", "https") or not parts.hostname:
            raise ValueError(f"api_base 非法：{api_base}")
        self._scheme = parts.scheme
        self._host = parts.hostname
        self._port = parts.port or (443 if parts.scheme == "https" else 80)
        self._base_path = parts.path.rstrip("/")
        self._proxy = proxy
        self._proxy_parts = urlsplit(proxy) if proxy else None
        self._timeout = timeout
        self._max_retries = max(1, max_retries)
        self._min_interval = max(0.0, min_send_interval)
        self._lock = threading.Lock()
        self._last_send = 0.0
        self._context = (
            ssl._create_unverified_context() if insecure else ssl.create_default_context()
        )

    # ------------------------------------------------------------------ #
    # HTTP 层
    # ------------------------------------------------------------------ #
    def _new_connection(self):
        if self._proxy_parts is not None:
            proxy_host = self._proxy_parts.hostname or ""
            if not proxy_host:
                raise TelegramError(f"代理地址非法：{self._proxy}")
            proxy_port = self._proxy_parts.port or (
                443 if self._proxy_parts.scheme == "https" else 80
            )
            conn = self._make_connection(proxy_host, proxy_port)
            conn.set_tunnel(self._host, self._port)
            return conn
        return self._make_connection(self._host, self._port)

    def _make_connection(self, host: str, port: int):
        if self._scheme == "https":
            return http.client.HTTPSConnection(
                host, port, timeout=self._timeout, context=self._context
            )
        return http.client.HTTPConnection(host, port, timeout=self._timeout)

    def _throttle(self) -> None:
        if self._min_interval <= 0:
            return
        with self._lock:
            wait = self._last_send + self._min_interval - time.monotonic()
            if wait > 0:
                time.sleep(wait)
            self._last_send = time.monotonic()

    def call(
        self,
        method: str,
        fields: dict[str, str] | None = None,
        files: list[_FilePart] | None = None,
    ) -> dict:
        """调用 Bot API，返回 ``result`` 字段。"""
        body = _MultipartBody(
            boundary=f"filebot{int(time.time() * 1000):x}{os.getpid():x}",
            fields={k: str(v) for k, v in (fields or {}).items()},
            files=files or [],
        )
        path = f"{self._base_path}/bot{self._token}/{method}"
        headers = {
            "Host": self._host if self._port in (80, 443) else f"{self._host}:{self._port}",
            "Content-Type": body.content_type,
            "Content-Length": str(body.content_length),
            "Connection": "close",
            "User-Agent": "filebot/1.0",
        }

        last_error: Exception | None = None
        for attempt in range(1, self._max_retries + 1):
            self._throttle()
            try:
                return self._attempt(method, path, body, headers)
            except _Retryable as exc:
                last_error = exc
                if attempt == self._max_retries:
                    break
                delay = exc.delay if exc.delay is not None else min(2 ** attempt, 30)
                log.warning("%s 失败（%s），%.1fs 后重试（%d/%d）", method, exc, delay, attempt, self._max_retries)
                time.sleep(delay)
        raise TelegramError(f"{method} 多次重试后仍失败：{last_error}", method=method)

    def _attempt(self, method: str, path: str, body: _MultipartBody, headers: dict) -> dict:
        conn = None
        try:
            conn = self._new_connection()
            conn.request("POST", path, body=iter(body), headers=headers)
            response = conn.getresponse()
            status = response.status
            headers_in = dict(response.getheaders())
            raw = response.read()
        except (OSError, http.client.HTTPException, ssl.SSLError) as exc:
            raise _Retryable(f"{type(exc).__name__}: {exc}") from exc
        finally:
            if conn is not None:
                try:
                    conn.close()
                except Exception:  # noqa: BLE001 - 关闭失败不影响结果
                    pass

        try:
            payload = json.loads(raw.decode("utf-8", "replace"))
        except json.JSONDecodeError:
            raise _Retryable(f"响应不是 JSON（HTTP {status}）：{raw[:200]!r}") from None

        if status == 429 or payload.get("error_code") == 429:
            retry_after = None
            parameters = payload.get("parameters") or {}
            retry_after = parameters.get("retry_after")
            if retry_after is None and headers_in.get("Retry-After"):
                try:
                    retry_after = float(headers_in["Retry-After"])
                except ValueError:
                    retry_after = None
            delay = (retry_after + 0.5) if retry_after is not None else 3.5
            raise _Retryable(
                f"触发限流：{payload.get('description')}", delay=delay
            )

        if status >= 500:
            raise _Retryable(f"服务端错误 HTTP {status}：{payload.get('description')}")

        if not payload.get("ok"):
            raise TelegramError(
                payload.get("description") or f"HTTP {status}",
                error_code=payload.get("error_code", status),
                method=method,
            )
        return payload.get("result") or {}

    # ------------------------------------------------------------------ #
    # 具体方法
    # ------------------------------------------------------------------ #
    def get_me(self) -> dict:
        return self.call("getMe")

    def send_message(self, chat_id: str, text: str) -> dict:
        return self.call("sendMessage", {"chat_id": chat_id, "text": text[:4096]})

    def _media_call(self, method: str, field: str, chat_id: str, path: str, caption: str | None, extra: dict | None = None) -> dict:
        filename = os.path.basename(path)
        content_type = mimetypes.guess_type(filename)[0] or "application/octet-stream"
        fields = {"chat_id": chat_id}
        if caption:
            fields["caption"] = caption[:CAPTION_LIMIT]
        fields.update(extra or {})
        return self.call(
            method,
            fields,
            [_FilePart(field=field, path=path, filename=filename, content_type=content_type)],
        )

    def send_document(self, chat_id: str, path: str, caption: str | None = None) -> dict:
        return self._media_call("sendDocument", "document", chat_id, path, caption)

    def send_photo(self, chat_id: str, path: str, caption: str | None = None) -> dict:
        return self._media_call("sendPhoto", "photo", chat_id, path, caption)

    def send_animation(self, chat_id: str, path: str, caption: str | None = None) -> dict:
        return self._media_call("sendAnimation", "animation", chat_id, path, caption)

    def send_video(self, chat_id: str, path: str, caption: str | None = None, **extra) -> dict:
        fields = {"supports_streaming": "true"}
        fields.update({k: v for k, v in extra.items() if v is not None})
        return self._media_call("sendVideo", "video", chat_id, path, caption, fields)
