"""测试辅助：假的 Telegram API 服务端、假的 Shadowsocks 服务端、假 HTTP 服务端。"""

from __future__ import annotations

import http.server
import json
import os
import socket
import socketserver
import struct
import threading

from filebot.ssproto import (
    KEY_LEN,
    MAX_PAYLOAD,
    derive_key,
    hkdf,
    make_cipher,
    parse_target_address,
    read_exact,
)

# --------------------------------------------------------------------------- #
# multipart 解析（用于校验客户端发出去的请求体）
# --------------------------------------------------------------------------- #


def parse_multipart(content_type: str, body: bytes) -> tuple[dict, dict]:
    boundary = content_type.split("boundary=", 1)[1].strip().strip('"').encode()
    delimiter = b"--" + boundary
    fields: dict[str, str] = {}
    files: dict[str, tuple[str, bytes]] = {}
    for chunk in body.split(delimiter)[1:]:
        if chunk.startswith(b"--"):
            break
        if chunk.startswith(b"\r\n"):
            chunk = chunk[2:]
        head, separator, data = chunk.partition(b"\r\n\r\n")
        if not separator:
            continue
        if data.endswith(b"\r\n"):
            data = data[:-2]
        name = filename = ""
        for line in head.split(b"\r\n"):
            if not line.lower().startswith(b"content-disposition:"):
                continue
            for item in line.decode("utf-8", "replace").split(";"):
                item = item.strip()
                if item.startswith("name="):
                    name = item[5:].strip('"')
                elif item.startswith("filename="):
                    filename = item[9:].strip('"')
        if filename:
            files[name] = (filename, data)
        else:
            fields[name] = data.decode("utf-8", "replace")
    return fields, files


class _FakeTelegramHandler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):  # 静音
        pass

    def do_POST(self):  # noqa: N802 - http.server 接口
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        method = self.path.rsplit("/", 1)[-1]
        fields, files = parse_multipart(self.headers.get("Content-Type", ""), body)
        server: "FakeTelegram" = self.server.fake  # type: ignore[attr-defined]

        scripted = server.script.pop(0) if server.script else None
        if scripted is None and server.method_scripts.get(method):
            scripted = server.method_scripts[method].pop(0)
        if scripted is not None:
            status, payload = scripted
            server.calls.append(
                {"method": method, "fields": fields, "files": files, "path": self.path}
            )
            raw = json.dumps(payload).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)
            return

        server.calls.append({"method": method, "fields": fields, "files": files, "path": self.path})
        result = {"message_id": len(server.calls)}
        if method == "getMe":
            result = {"id": 1, "username": "filebot_test", "is_bot": True}
        raw = json.dumps({"ok": True, "result": result}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)


class FakeTelegram:
    """记录所有调用的假 Telegram Bot API。"""

    def __init__(self) -> None:
        self._server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), _FakeTelegramHandler)
        self._server.fake = self  # type: ignore[attr-defined]
        self.calls: list[dict] = []
        self.script: list[tuple[int, dict]] = []
        self.method_scripts: dict[str, list[tuple[int, dict]]] = {}
        self._thread: threading.Thread | None = None

    @property
    def url(self) -> str:
        host, port = self._server.server_address[:2]
        return f"http://{host}:{port}"

    def start(self) -> "FakeTelegram":
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)
        self._thread.start()
        return self

    def stop(self) -> None:
        self._server.shutdown()
        self._server.server_close()

    def methods(self) -> list[str]:
        return [call["method"] for call in self.calls]

    def calls_of(self, method: str) -> list[dict]:
        return [call for call in self.calls if call["method"] == method]


# --------------------------------------------------------------------------- #
# 假 HTTP 服务端（作为隧道目标）
# --------------------------------------------------------------------------- #

class _EchoHandler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def do_GET(self):  # noqa: N802
        body = b"hello-through-tunnel"
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


class FakeHttpServer:
    def __init__(self) -> None:
        self._server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), _EchoHandler)
        self._thread: threading.Thread | None = None

    @property
    def host(self) -> str:
        return "127.0.0.1"

    @property
    def port(self) -> int:
        return self._server.server_address[1]

    def start(self) -> "FakeHttpServer":
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)
        self._thread.start()
        return self

    def stop(self) -> None:
        self._server.shutdown()
        self._server.server_close()


# --------------------------------------------------------------------------- #
# 假 Shadowsocks 服务端（AEAD-2017，服务端方向独立实现，用于交叉验证封帧）
# --------------------------------------------------------------------------- #

class _NonceStream:
    """独立写法的 AEAD 计数器（与被测代码 Direction 相互印证）。"""

    def __init__(self, method: str, subkey: bytes) -> None:
        self._cipher = make_cipher(method, subkey)
        self._counter = 0

    def _nonce(self) -> bytes:
        nonce = self._counter.to_bytes(12, "little")
        self._counter += 1
        return nonce

    def seal(self, payload: bytes) -> bytes:
        return self._cipher.encrypt(self._nonce(), struct.pack(">H", len(payload)), None) + \
            self._cipher.encrypt(self._nonce(), payload, None)

    def open(self, blob: bytes) -> bytes:
        return self._cipher.decrypt(self._nonce(), blob, None)

    def open_length(self, blob: bytes) -> int:
        return struct.unpack(">H", self._cipher.decrypt(self._nonce(), blob, None))[0]


def _read_chunk(sock: socket.socket, stream: _NonceStream) -> bytes:
    length_block = read_exact(sock, 18)
    if len(length_block) != 18:
        return b""
    length = stream.open_length(length_block)
    if length == 0 or length > MAX_PAYLOAD:
        raise ValueError(f"非法分块长度 {length}")
    data = read_exact(sock, length + 16)
    if len(data) != length + 16:
        raise ValueError("分块不完整")
    return stream.open(data)


class _ShadowsocksHandler(socketserver.BaseRequestHandler):
    def handle(self) -> None:
        server: "FakeShadowsocksServer" = self.server  # type: ignore[assignment]
        sock: socket.socket = self.request
        sock.settimeout(20)
        key_len = KEY_LEN[server.method]
        master = derive_key(server.password, key_len)

        client_salt = read_exact(sock, key_len)
        if len(client_salt) != key_len:
            return
        inbound = _NonceStream(server.method, hkdf(master, client_salt, b"ss-subkey", key_len))
        first = _read_chunk(sock, inbound)
        if not first:
            return
        host, port, consumed = parse_target_address(first)
        leftover = first[consumed:]
        server.targets.append((host, port))

        try:
            remote = socket.create_connection((host, port), timeout=10)
        except OSError:
            return
        remote.settimeout(20)

        out_salt = os.urandom(key_len)
        outbound = _NonceStream(server.method, hkdf(master, out_salt, b"ss-subkey", key_len))
        sock.sendall(out_salt)
        if leftover:
            remote.sendall(leftover)

        def to_remote() -> None:
            try:
                while True:
                    data = _read_chunk(sock, inbound)
                    if not data:
                        break
                    remote.sendall(data)
            except (OSError, ValueError):
                pass
            finally:
                try:
                    remote.shutdown(socket.SHUT_WR)
                except OSError:
                    pass

        forwarder = threading.Thread(target=to_remote, daemon=True)
        forwarder.start()
        try:
            while True:
                data = remote.recv(65536)
                if not data:
                    break
                for offset in range(0, len(data), MAX_PAYLOAD):
                    sock.sendall(outbound.seal(data[offset : offset + MAX_PAYLOAD]))
        except (OSError, ValueError):
            pass
        finally:
            forwarder.join(timeout=5)
            try:
                remote.close()
            except OSError:
                pass


class FakeShadowsocksServer(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True

    def __init__(self, method: str = "aes-256-gcm", password: str = "test-password") -> None:
        super().__init__(("127.0.0.1", 0), _ShadowsocksHandler)
        self.method = method
        self.password = password
        self.targets: list[tuple[str, int]] = []
        self._thread: threading.Thread | None = None

    @property
    def host(self) -> str:
        return "127.0.0.1"

    @property
    def port(self) -> int:
        return self.server_address[1]

    def start(self) -> "FakeShadowsocksServer":
        self._thread = threading.Thread(target=self.serve_forever, daemon=True)
        self._thread.start()
        return self

    def stop(self) -> None:
        self.shutdown()
        self.server_close()


# --------------------------------------------------------------------------- #
# 假 SOCKS5 服务端
# --------------------------------------------------------------------------- #

class _Socks5Handler(socketserver.BaseRequestHandler):
    def handle(self) -> None:
        server: "FakeSocks5Server" = self.server  # type: ignore[assignment]
        sock: socket.socket = self.request
        sock.settimeout(20)
        head = read_exact(sock, 2)
        if len(head) != 2:
            return
        methods = read_exact(sock, head[1])
        server.methods_offered.append(list(methods))
        sock.sendall(b"\x05\x00")
        request = read_exact(sock, 4)
        if len(request) != 4:
            return
        host, port, rest = self._read_target(sock, request[3])
        if rest:
            return
        server.targets.append((host, port))
        try:
            remote = socket.create_connection((host, port), timeout=10)
        except OSError:
            sock.sendall(b"\x05\x05\x00\x01" + b"\x00" * 6)
            return
        sock.sendall(b"\x05\x00\x00\x01" + b"\x00" * 6)
        self._relay(sock, remote)

    @staticmethod
    def _read_target(sock: socket.socket, atyp: int):
        if atyp == 0x01:
            raw = read_exact(sock, 4)
            host = socket.inet_ntoa(raw)
        elif atyp == 0x04:
            host = socket.inet_ntop(socket.AF_INET6, read_exact(sock, 16))
        elif atyp == 0x03:
            length = read_exact(sock, 1)[0]
            host = read_exact(sock, length).decode()
        else:
            return "", 0, True
        port = struct.unpack(">H", read_exact(sock, 2))[0]
        return host, port, False

    @staticmethod
    def _relay(client: socket.socket, remote: socket.socket) -> None:
        def pump(src, dst) -> None:
            try:
                while True:
                    data = src.recv(65536)
                    if not data:
                        break
                    dst.sendall(data)
            except OSError:
                pass
            finally:
                try:
                    dst.shutdown(socket.SHUT_WR)
                except OSError:
                    pass

        thread = threading.Thread(target=pump, args=(client, remote), daemon=True)
        thread.start()
        pump(remote, client)
        thread.join(timeout=5)
        remote.close()


class FakeSocks5Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True

    def __init__(self) -> None:
        super().__init__(("127.0.0.1", 0), _Socks5Handler)
        self.targets: list[tuple[str, int]] = []
        self.methods_offered: list[list[int]] = []
        self._thread: threading.Thread | None = None

    @property
    def host(self) -> str:
        return "127.0.0.1"

    @property
    def port(self) -> int:
        return self.server_address[1]

    def start(self) -> "FakeSocks5Server":
        self._thread = threading.Thread(target=self.serve_forever, daemon=True)
        self._thread.start()
        return self

    def stop(self) -> None:
        self.shutdown()
        self.server_close()
