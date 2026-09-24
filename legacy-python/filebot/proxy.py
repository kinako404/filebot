"""本地 HTTP CONNECT 代理：把电报流量经由 ss / socks5 / http 上游转发。

``http.client`` 只认 HTTP 代理，所以无论配置哪种上游，这里都在 127.0.0.1
上起一个同时支持 CONNECT 与绝对 URI 请求的小代理，出口由 :class:`Upstream`
决定。这样 Telegram 客户端只需 ``set_tunnel()``。
"""

from __future__ import annotations

import base64
import logging
import socket
import socketserver
import threading
from urllib.parse import unquote, urlsplit, urlunsplit

from .ssproto import SsClientStream, SsServer, pack_target_address, parse_ss_url, read_exact

log = logging.getLogger(__name__)

DEFAULT_PORTS = {"http": 80, "https": 443, "socks5": 1080, "socks5h": 1080, "socks": 1080}


class ProxyError(Exception):
    """代理配置或连接错误。"""


def _split_authority(target: str, default_port: int) -> tuple[str, int]:
    if target.startswith("["):
        host, _, rest = target[1:].partition("]")
        port_text = rest.lstrip(":")
    else:
        host, _, port_text = target.rpartition(":")
        if not host:
            host, port_text = target, ""
    try:
        port = int(port_text) if port_text else default_port
    except ValueError as exc:
        raise ProxyError(f"端口非法：{port_text!r}") from exc
    if not host:
        raise ProxyError(f"缺少主机名：{target!r}")
    return host, port


class _TunnelSocket:
    """把 :class:`SsClientStream` 包装成 socket 风格对象，供转发循环使用。"""

    def __init__(self, stream: SsClientStream) -> None:
        self._stream = stream

    def sendall(self, data: bytes) -> None:
        self._stream.sendall(data)

    def recv(self, size: int) -> bytes:
        return self._stream.recv(size)

    def settimeout(self, timeout: float | None) -> None:
        self._stream.settimeout(timeout)

    def shutdown(self, how: int) -> None:  # 半关闭无法在 SS 隧道里表达，忽略
        return None

    def close(self) -> None:
        self._stream.close()


class DirectUpstream:
    name = "direct"

    def connect(self, host: str, port: int, timeout: float = 20.0):
        return socket.create_connection((host, port), timeout=timeout)


class ShadowSocksUpstream:
    def __init__(self, server: SsServer, timeout: float = 20.0) -> None:
        self._server = server
        self._timeout = timeout
        self.name = f"shadowsocks {server}"

    def connect(self, host: str, port: int, timeout: float | None = None):
        sock = socket.create_connection(
            (self._server.host, self._server.port), timeout=timeout or self._timeout
        )
        sock.settimeout(None)
        stream = SsClientStream(sock, self._server.method, self._server.password)
        try:
            stream.connect_target(host, port)
        except Exception:
            stream.close()
            raise
        return _TunnelSocket(stream)


class Socks5Upstream:
    """标准 SOCKS5 客户端（支持无认证与用户名/密码认证）。"""

    def __init__(self, host: str, port: int, username: str = "", password: str = "") -> None:
        self._host = host
        self._port = port
        self._username = username
        self._password = password
        self.name = f"socks5 {host}:{port}"

    def connect(self, host: str, port: int, timeout: float = 20.0):
        sock = socket.create_connection((self._host, self._port), timeout=timeout)
        sock.settimeout(timeout)
        try:
            self._negotiate(sock)
            sock.sendall(b"\x05\x01\x00" + pack_target_address(host, port))
            self._read_reply(sock)
        except Exception:
            sock.close()
            raise
        sock.settimeout(None)
        return sock

    def _negotiate(self, sock: socket.socket) -> None:
        methods = b"\x00"
        if self._username:
            methods += b"\x02"
        sock.sendall(bytes([0x05, len(methods)]) + methods)
        reply = read_exact(sock, 2)
        if len(reply) != 2 or reply[0] != 0x05:
            raise ProxyError("SOCKS5 握手失败")
        method = reply[1]
        if method == 0xFF:
            raise ProxyError("SOCKS5 服务器拒绝了所有认证方式")
        if method == 0x02:
            user = self._username.encode()
            password = self._password.encode()
            sock.sendall(bytes([0x01, len(user)]) + user + bytes([len(password)]) + password)
            auth = read_exact(sock, 2)
            if len(auth) != 2 or auth[1] != 0x00:
                raise ProxyError("SOCKS5 用户名/密码认证失败")

    @staticmethod
    def _read_reply(sock: socket.socket) -> None:
        head = read_exact(sock, 4)
        if len(head) != 4 or head[0] != 0x05:
            raise ProxyError("SOCKS5 应答异常")
        if head[1] != 0x00:
            raise ProxyError(f"SOCKS5 连接被拒绝（reply=0x{head[1]:02x}）")
        atyp = head[3]
        if atyp == 0x01:
            read_exact(sock, 4 + 2)
        elif atyp == 0x04:
            read_exact(sock, 16 + 2)
        elif atyp == 0x03:
            length = read_exact(sock, 1)
            read_exact(sock, length[0] + 2)
        else:
            raise ProxyError(f"SOCKS5 应答地址类型未知：0x{atyp:02x}")


class HttpProxyUpstream:
    """上游本身是 HTTP 代理（CONNECT 隧道）。"""

    def __init__(self, host: str, port: int, auth_header: str = "") -> None:
        self._host = host
        self._port = port
        self._auth = auth_header
        self.name = f"http {host}:{port}"

    def connect(self, host: str, port: int, timeout: float = 20.0):
        sock = socket.create_connection((self._host, self._port), timeout=timeout)
        sock.settimeout(timeout)
        request = [f"CONNECT {host}:{port} HTTP/1.1", f"Host: {host}:{port}"]
        if self._auth:
            request.append(f"Proxy-Authorization: {self._auth}")
        request.append("")
        request.append("")
        try:
            sock.sendall("\r\n".join(request).encode())
            response = b""
            while b"\r\n\r\n" not in response:
                chunk = sock.recv(4096)
                if not chunk:
                    raise ProxyError("上游 HTTP 代理未响应 CONNECT")
                response += chunk
                if len(response) > 65536:
                    raise ProxyError("上游 HTTP 代理响应过大")
        except Exception:
            sock.close()
            raise
        status_line = response.split(b"\r\n", 1)[0].split(b" ", 2)
        if len(status_line) < 2 or not status_line[1].startswith(b"2"):
            sock.close()
            raise ProxyError(f"上游 HTTP 代理拒绝 CONNECT：{status_line!r}")
        sock.settimeout(None)
        return sock


def _upstream_from_url(url: str) -> object:
    parts = urlsplit(url)
    scheme = parts.scheme.lower()
    if scheme == "ss":
        return ShadowSocksUpstream(parse_ss_url(url))
    if scheme in ("socks5", "socks5h", "socks"):
        host = parts.hostname or ""
        if not host:
            raise ProxyError(f"socks5 代理缺少主机名：{url}")
        return Socks5Upstream(
            host,
            parts.port or DEFAULT_PORTS[scheme],
            unquote(parts.username or ""),
            unquote(parts.password or ""),
        )
    if scheme in ("http", "https"):
        host = parts.hostname or ""
        if not host:
            raise ProxyError(f"http 代理缺少主机名：{url}")
        auth = ""
        if parts.username:
            import base64

            raw = f"{unquote(parts.username)}:{unquote(parts.password or '')}".encode()
            auth = "Basic " + base64.b64encode(raw).decode()
        return HttpProxyUpstream(host, parts.port or DEFAULT_PORTS[scheme], auth)
    if scheme == "direct" or url in ("", "none"):
        return DirectUpstream()
    raise ProxyError(f"不支持的代理协议 {scheme!r}：只支持 ss:// / socks5:// / http:// / 留空直连")


class LocalConnectProxy:
    """127.0.0.1 上的小代理，把请求转交给 upstream。"""

    def __init__(self, upstream, host: str = "127.0.0.1", port: int = 0) -> None:
        self._upstream = upstream
        self._server = socketserver.ThreadingTCPServer(
            (host, port), _ConnectProxyHandler, bind_and_activate=False
        )
        self._server.allow_reuse_address = True
        self._server.daemon_threads = True
        self._server.upstream = upstream
        self._server.handshake_timeout = 15.0
        self._server.server_bind()
        self._server.server_activate()
        self._thread: threading.Thread | None = None

    @property
    def host(self) -> str:
        return self._server.server_address[0]

    @property
    def port(self) -> int:
        return self._server.server_address[1]

    @property
    def url(self) -> str:
        return f"http://{self.host}:{self.port}"

    def start(self) -> "LocalConnectProxy":
        self._thread = threading.Thread(
            target=self._server.serve_forever, name="filebot-proxy", daemon=True
        )
        self._thread.start()
        log.info("本地代理已启动：%s → %s", self.url, getattr(self._upstream, "name", "?"))
        return self

    def stop(self) -> None:
        self._server.shutdown()
        self._server.server_close()
        if self._thread:
            self._thread.join(timeout=5)


class _ConnectProxyHandler(socketserver.BaseRequestHandler):
    server: LocalConnectProxy  # type: ignore[assignment]

    def handle(self) -> None:
        client: socket.socket = self.request
        client.settimeout(self.server.handshake_timeout)
        try:
            head, rest = self._read_head(client)
        except (OSError, ProxyError) as exc:
            log.debug("读取代理请求失败：%s", exc)
            return
        if not head:
            return

        lines = head.split(b"\r\n")
        parts = lines[0].split(b" ")
        if len(parts) != 3:
            self._fail(client, 400, "Bad Request")
            return
        method, target, version = parts
        try:
            if method.upper() == b"CONNECT":
                self._handle_connect(client, target, rest)
            else:
                self._handle_forward(client, method, target, version, lines[1:], rest)
        except (OSError, ProxyError) as exc:
            log.warning("代理转发失败：%s", exc)

    @staticmethod
    def _read_head(client: socket.socket) -> tuple[bytes, bytes]:
        buffer = b""
        while b"\r\n\r\n" not in buffer:
            chunk = client.recv(65536)
            if not chunk:
                return b"", b""
            buffer += chunk
            if len(buffer) > 65536:
                raise ProxyError("请求头过大")
        head, _, rest = buffer.partition(b"\r\n\r\n")
        return head, rest

    def _handle_connect(self, client: socket.socket, target: bytes, rest: bytes) -> None:
        host, port = _split_authority(target.decode("latin-1"), 443)
        remote = self._open_upstream(client, host, port)
        if remote is None:
            return
        try:
            client.sendall(b"HTTP/1.1 200 Connection Established\r\n\r\n")
            if rest:
                remote.sendall(rest)
            client.settimeout(None)
            self._relay(client, remote)
        finally:
            _safe_close(remote)

    def _open_upstream(self, client: socket.socket, host: str, port: int):
        try:
            return self.server.upstream.connect(host, port)
        except Exception as exc:  # noqa: BLE001 - 上游失败要明确回给客户端
            log.warning("连接上游 %s:%s 失败：%s", host, port, exc)
            self._fail(client, 502, "Bad Gateway")
            return None

    def _handle_forward(
        self,
        client: socket.socket,
        method: bytes,
        target: bytes,
        version: bytes,
        header_lines: list[bytes],
        rest: bytes,
    ) -> None:
        url = target.decode("latin-1")
        if not url.lower().startswith("http://"):
            self._fail(client, 501, "Not Implemented")
            return
        parts = urlsplit(url)
        host = parts.hostname or ""
        if not host:
            self._fail(client, 400, "Bad Request")
            return
        port = parts.port or 80
        path = urlunsplit(("", "", parts.path or "/", parts.query, ""))
        forwarded = [b" ".join([method, path.encode("latin-1"), version])]
        for line in header_lines:
            if line.split(b":", 1)[0].strip().lower() in (b"proxy-connection",):
                continue
            forwarded.append(line)
        remote = self._open_upstream(client, host, port)
        if remote is None:
            return
        try:
            remote.sendall(b"\r\n".join(forwarded) + b"\r\n\r\n" + rest)
            client.settimeout(None)
            self._relay(client, remote)
        finally:
            _safe_close(remote)

    @staticmethod
    def _relay(client: socket.socket, remote) -> None:
        thread = threading.Thread(
            target=_pump, args=(remote, client), name="filebot-proxy-up", daemon=True
        )
        thread.start()
        _pump(client, remote)
        thread.join(timeout=30)
        log.debug("代理连接结束")

    @staticmethod
    def _fail(client: socket.socket, code: int, reason: str) -> None:
        try:
            client.sendall(
                f"HTTP/1.1 {code} {reason}\r\nContent-Length: 0\r\nConnection: close\r\n\r\n".encode()
            )
        except OSError:
            pass


def _pump(src, dst) -> None:
    try:
        while True:
            data = src.recv(65536)
            if not data:
                break
            dst.sendall(data)
    except Exception as exc:  # noqa: BLE001 - 隧道任意一端出错都只结束本次连接
        log.debug("隧道转发结束：%s", exc)
    finally:
        try:
            dst.shutdown(socket.SHUT_WR)
        except (OSError, AttributeError):
            pass


def _safe_close(sock) -> None:
    try:
        sock.close()
    except OSError:
        pass


def build_proxy(url: str | None) -> tuple[str | None, LocalConnectProxy | None]:
    """把配置里的代理链接变成 ``http.client`` 可用的代理地址。

    返回 ``(proxy_url, server)``；``server`` 需要在退出时 ``stop()``。
    直连时返回 ``(None, None)``。
    """
    if not url or url.strip().lower() in ("", "none", "direct"):
        return None, None
    upstream = _upstream_from_url(url.strip())
    if isinstance(upstream, DirectUpstream):
        return None, None
    proxy = LocalConnectProxy(upstream).start()
    return proxy.url, proxy


__all__ = [
    "DirectUpstream",
    "HttpProxyUpstream",
    "LocalConnectProxy",
    "ProxyError",
    "ShadowSocksUpstream",
    "Socks5Upstream",
    "build_proxy",
]
