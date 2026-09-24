"""ssproto：加密原语（官方向量）、链接解析、隧道封帧。"""

from __future__ import annotations

import binascii
import socket
import unittest

from filebot.ssproto import (
    KEY_LEN,
    SsClientStream,
    SsError,
    derive_key,
    hkdf,
    parse_ss_url,
    parse_target_address,
    pack_target_address,
)
from tests.helpers import FakeHttpServer, FakeShadowsocksServer


class TestOfficialVectors(unittest.TestCase):
    def test_rfc8439_chacha20_poly1305(self):
        from filebot.ssproto import PureChaCha20Poly1305

        key = bytes(range(0x80, 0xA0))
        nonce = bytes.fromhex("070000004041424344454647")
        aad = bytes.fromhex("50515253c0c1c2c3c4c5c6c7")
        plaintext = (
            b"Ladies and Gentlemen of the class of '99: If I could offer you only "
            b"one tip for the future, sunscreen would be it."
        )
        expected = binascii.unhexlify(
            "d31a8d34648e60db7b86afbc53ef7ec2"
            "a4aded51296e08fea9e2b5a736ee62d6"
            "3dbea45e8ca9671282fafb69da92728b"
            "1a71de0a9e060b2905d6a5b67ecd3b36"
            "92ddbd7f2d778b8c9803aee328091b58"
            "fab324e4fad675945585808b4831d7bc"
            "3ff4def08e4b7a9de576d26586cec64b"
            "6116"
        ) + binascii.unhexlify("1ae10b594f09e26a7e902ecbd0600691")

        cipher = PureChaCha20Poly1305(key)
        self.assertEqual(cipher.encrypt(nonce, plaintext, aad), expected)
        self.assertEqual(cipher.decrypt(nonce, expected, aad), plaintext)

    def test_poly1305_rejects_tampered_tag(self):
        from filebot.ssproto import PureChaCha20Poly1305

        cipher = PureChaCha20Poly1305(b"k" * 32)
        blob = bytearray(cipher.encrypt(b"n" * 12, b"payload", b""))
        blob[-1] ^= 0x01
        with self.assertRaises(ValueError):
            cipher.decrypt(b"n" * 12, bytes(blob), b"")

    def test_rfc5869_hkdf_sha256_case1(self):
        okm = hkdf(
            bytes.fromhex("0b" * 22),
            bytes.fromhex("000102030405060708090a0b0c"),
            bytes.fromhex("f0f1f2f3f4f5f6f7f8f9"),
            42,
            "sha256",
        )
        self.assertEqual(
            okm.hex(),
            "3cb25f25faacd57a90434f64d0362f2a2d2d0a90cf1a5a4c5db02d56ecc4c5bf"
            "34007208d5b887185865",
        )

    def test_rfc5869_hkdf_sha1_case1(self):
        okm = hkdf(
            bytes.fromhex("0b" * 11),
            bytes.fromhex("000102030405060708090a0b0c"),
            bytes.fromhex("f0f1f2f3f4f5f6f7f8f9"),
            42,
            "sha1",
        )
        self.assertEqual(
            okm.hex(),
            "085a01ea1b10f36933068b56efa5ad81a4f14b822f5b091568a9cdd4f155fda2"
            "c22e422478d305f3f896",
        )


class TestSsUrlParsing(unittest.TestCase):
    def test_base64_userinfo(self):
        import base64

        credential = base64.urlsafe_b64encode(b"aes-256-gcm:p@ss:word").decode().rstrip("=")
        server = parse_ss_url(f"ss://{credential}@1.2.3.4:8388#%E8%8A%82%E7%82%B9A")
        self.assertEqual(server.host, "1.2.3.4")
        self.assertEqual(server.port, 8388)
        self.assertEqual(server.method, "aes-256-gcm")
        self.assertEqual(server.password, "p@ss:word")
        self.assertEqual(server.tag, "节点A")

    def test_fully_base64(self):
        import base64

        blob = base64.urlsafe_b64encode(b"chacha20-ietf-poly1305:secret@ss.example.com:443").decode()
        server = parse_ss_url(f"ss://{blob}")
        self.assertEqual((server.host, server.port, server.method), ("ss.example.com", 443, "chacha20-ietf-poly1305"))
        self.assertEqual(server.password, "secret")

    def test_plain_form_and_ipv6(self):
        server = parse_ss_url("ss://aes-128-gcm:pw@[2001:db8::1]:8388")
        self.assertEqual((server.host, server.port, server.method), ("2001:db8::1", 8388, "aes-128-gcm"))

    def test_query_params_are_ignored(self):
        import base64

        credential = base64.urlsafe_b64encode(b"aes-256-gcm:pw").decode().rstrip("=")
        server = parse_ss_url(f"ss://{credential}@1.1.1.1:8388?plugin=obfs-local%3Bobfs%3Dhttp")
        self.assertEqual(server.port, 8388)

    def test_rejects_unsupported_cipher(self):
        with self.assertRaises(SsError) as ctx:
            parse_ss_url("ss://aes-256-cfb:pw@1.1.1.1:8388")
        self.assertIn("AEAD-2017", str(ctx.exception))

    def test_rejects_missing_port(self):
        with self.assertRaises(SsError):
            parse_ss_url("ss://aes-256-gcm:pw@1.1.1.1")


class TestAddressAndKeys(unittest.TestCase):
    def test_key_lengths(self):
        for method, length in KEY_LEN.items():
            self.assertEqual(len(derive_key("pw", length)), length)

    def test_evp_bytes_to_key_matches_md5_chain(self):
        import hashlib

        expected = hashlib.md5(b"secret").digest() + hashlib.md5(
            hashlib.md5(b"secret").digest() + b"secret"
        ).digest()
        self.assertEqual(derive_key("secret", 32), expected[:32])

    def test_address_roundtrip(self):
        for host, port in [("example.com", 443), ("1.2.3.4", 80), ("2001:db8::1", 8388)]:
            packed = pack_target_address(host, port)
            self.assertEqual(parse_target_address(packed), (host, port, len(packed)))

    def test_address_atyp(self):
        self.assertEqual(pack_target_address("1.2.3.4", 1)[0], 0x01)
        self.assertEqual(pack_target_address("::1", 1)[0], 0x04)
        self.assertEqual(pack_target_address("example.com", 1)[0], 0x03)


class TestSsStreamOverFakeServer(unittest.TestCase):
    def test_roundtrip_through_fake_shadowsocks(self):
        http = FakeHttpServer().start()
        ss = FakeShadowsocksServer(method="aes-256-gcm").start()
        try:
            sock = socket.create_connection((ss.host, ss.port), timeout=10)
            stream = SsClientStream(sock, "aes-256-gcm", ss.password)
            stream.connect_target(http.host, http.port)
            stream.sendall(
                f"GET / HTTP/1.1\r\nHost: {http.host}:{http.port}\r\nConnection: close\r\n\r\n".encode()
            )
            received = b""
            while b"hello-through-tunnel" not in received:
                chunk = stream.recv(65536)
                if not chunk:
                    break
                received += chunk
            stream.close()
        finally:
            ss.stop()
            http.stop()
        self.assertIn(b"200 OK", received)
        self.assertIn(b"hello-through-tunnel", received)
        self.assertEqual(ss.targets[0], (http.host, http.port))

    def test_chacha20_method_roundtrip(self):
        http = FakeHttpServer().start()
        ss = FakeShadowsocksServer(method="chacha20-ietf-poly1305", password="pw123").start()
        try:
            sock = socket.create_connection((ss.host, ss.port), timeout=10)
            stream = SsClientStream(sock, "chacha20-ietf-poly1305", "pw123")
            stream.connect_target(http.host, http.port)
            stream.sendall(b"GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
            received = b""
            while True:
                chunk = stream.recv(65536)
                if not chunk:
                    break
                received += chunk
            stream.close()
        finally:
            ss.stop()
            http.stop()
        self.assertIn(b"hello-through-tunnel", received)


if __name__ == "__main__":
    unittest.main()
