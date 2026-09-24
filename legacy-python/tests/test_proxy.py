"""本地 CONNECT 代理：ss:// / socks5:// 上游转发链路。"""

from __future__ import annotations

import socket
import unittest
from urllib.parse import urlsplit

from filebot.proxy import ProxyError, build_proxy
from tests.helpers import FakeHttpServer, FakeShadowsocksServer, FakeSocks5Server, read_exact


def read_head(sock: socket.socket) -> bytes:
    buffer = b""
    while b"\r\n\r\n" not in buffer:
        chunk = sock.recv(4096)
        if not chunk:
            break
        buffer += chunk
    return buffer


def read_all(sock: socket.socket) -> bytes:
    buffer = b""
    while True:
        chunk = sock.recv(65536)
        if not chunk:
            break
        buffer += chunk
    return buffer


def _find_free_port() -> int:
    """找一个当前没人监听的端口（用于制造连接失败）。"""
    probe = socket.socket()
    probe.bind(("127.0.0.1", 0))
    port = probe.getsockname()[1]
    probe.close()
    return port


def request_via_connect(proxy_url: str, host: str, port: int) -> bytes:
    parts = urlsplit(proxy_url)
    sock = socket.create_connection((parts.hostname, parts.port), timeout=10)
    try:
        sock.sendall(f"CONNECT {host}:{port} HTTP/1.1\r\nHost: {host}:{port}\r\n\r\n".encode())
        head = read_head(sock)
        assert head.startswith(b"HTTP/1.1 200"), head
        assert b"\r\n\r\n" in head
        leftover = head.split(b"\r\n\r\n", 1)[1]
        sock.sendall(
            f"GET / HTTP/1.1\r\nHost: {host}:{port}\r\nConnection: close\r\n\r\n".encode()
        )
        return leftover + read_all(sock)
    finally:
        sock.close()


def request_absolute_form(proxy_url: str, host: str, port: int) -> bytes:
    parts = urlsplit(proxy_url)
    sock = socket.create_connection((parts.hostname, parts.port), timeout=10)
    try:
        sock.sendall(
            f"GET http://{host}:{port}/ HTTP/1.1\r\nHost: {host}:{port}\r\n"
            f"Proxy-Connection: keep-alive\r\nConnection: close\r\n\r\n".encode()
        )
        return read_all(sock)
    finally:
        sock.close()


class TestBuildProxy(unittest.TestCase):
    def test_empty_means_direct(self):
        for value in (None, "", "   ", "direct", "none"):
            self.assertEqual(build_proxy(value), (None, None))

    def test_unknown_scheme_rejected(self):
        with self.assertRaises(ProxyError):
            build_proxy("ftp://1.2.3.4:21")

    def test_bad_ss_link_rejected(self):
        with self.assertRaises(Exception):
            build_proxy("ss://not-base64@")


class TestShadowsocksProxyChain(unittest.TestCase):
    def setUp(self):
        self.http = FakeHttpServer().start()
        self.ss = FakeShadowsocksServer(method="aes-256-gcm", password="pw").start()
        self.url = f"ss://aes-256-gcm:pw@{self.ss.host}:{self.ss.port}"
        self.proxy_url, self.proxy = build_proxy(self.url)

    def tearDown(self):
        if self.proxy:
            self.proxy.stop()
        self.ss.stop()
        self.http.stop()

    def test_connect_tunnel(self):
        response = request_via_connect(self.proxy_url, self.http.host, self.http.port)
        self.assertIn(b"200 OK", response)
        self.assertIn(b"hello-through-tunnel", response)
        self.assertEqual(self.ss.targets[0], (self.http.host, self.http.port))

    def test_absolute_uri_forwarding(self):
        response = request_absolute_form(self.proxy_url, self.http.host, self.http.port)
        self.assertIn(b"hello-through-tunnel", response)

    def test_proxy_listens_on_loopback_only(self):
        parts = urlsplit(self.proxy_url)
        self.assertEqual(parts.hostname, "127.0.0.1")

    def test_upstream_failure_is_reported_to_client(self):
        # socks5 上游连不上目标时，本地代理应回 502 而不是假装成功
        socks = FakeSocks5Server().start()
        proxy_url, proxy = build_proxy(f"socks5://{socks.host}:{socks.port}")
        dead = _find_free_port()
        try:
            parts = urlsplit(proxy_url)
            sock = socket.create_connection((parts.hostname, parts.port), timeout=10)
            try:
                sock.sendall(f"CONNECT 127.0.0.1:{dead} HTTP/1.1\r\n\r\n".encode())
                head = read_head(sock)
                self.assertIn(b"502", head)
            finally:
                sock.close()
        finally:
            if proxy:
                proxy.stop()
            socks.stop()

    def test_ss_upstream_failure_closes_tunnel(self):
        # ss 上游无法感知目标是否可达：本地代理先回 200，随后隧道被关闭
        dead = _find_free_port()
        parts = urlsplit(self.proxy_url)
        sock = socket.create_connection((parts.hostname, parts.port), timeout=10)
        try:
            sock.sendall(f"CONNECT 127.0.0.1:{dead} HTTP/1.1\r\n\r\n".encode())
            head = read_head(sock)
            self.assertIn(b"200 Connection Established", head)
            self.assertEqual(sock.recv(64), b"")
        finally:
            sock.close()


class TestSocks5ProxyChain(unittest.TestCase):
    def test_socks5_upstream(self):
        http = FakeHttpServer().start()
        socks = FakeSocks5Server().start()
        proxy_url, proxy = build_proxy(f"socks5://{socks.host}:{socks.port}")
        try:
            self.assertIsNotNone(proxy)
            response = request_via_connect(proxy_url, http.host, http.port)
            self.assertIn(b"hello-through-tunnel", response)
            self.assertEqual(socks.targets[0], (http.host, http.port))
            self.assertIn(0x00, socks.methods_offered[0])
        finally:
            if proxy:
                proxy.stop()
            socks.stop()
            http.stop()


class HelpersSanityTest(unittest.TestCase):
    def test_read_exact(self):
        left, right = socket.socketpair()
        try:
            right.sendall(b"abcd")
            self.assertEqual(read_exact(left, 4), b"abcd")
        finally:
            left.close()
            right.close()


if __name__ == "__main__":
    unittest.main()
