"""Shadowsocks AEAD-2017 协议原语。

包含：``ss://`` 链接解析、EVP_BytesToKey 主密钥派生、HKDF-SHA1 子密钥派生、
AEAD 分块封帧（客户端方向），以及地址头编解码。

协议参考 shadowsocks SIP004（AEAD ciphers）。
"""

from __future__ import annotations

import base64
import binascii
import hashlib
import hmac
import os
import socket
import struct
from dataclasses import dataclass
from urllib.parse import unquote

MAX_PAYLOAD = 0x3FFF
"""单个 AEAD 分块的最大明文长度（长度字段为 2 字节，且上限 0x3FFF）。"""

KEY_LEN = {
    "aes-128-gcm": 16,
    "aes-192-gcm": 24,
    "aes-256-gcm": 32,
    "chacha20-ietf-poly1305": 32,
}
"""支持的 AEAD-2017 加密方式及其密钥长度。"""

UNSUPPORTED_METHOD_HINT = (
    "当前仅支持 AEAD-2017 加密（aes-128-gcm / aes-192-gcm / aes-256-gcm / "
    "chacha20-ietf-poly1305）；不支持 2022-blake3-* 与流加密（aes-256-cfb 等）"
)


class SsError(Exception):
    """Shadowsocks 配置或协议错误。"""


@dataclass(frozen=True)
class SsServer:
    host: str
    port: int
    method: str
    password: str
    tag: str = ""

    def __str__(self) -> str:
        label = self.tag or self.host
        return f"{self.method}@{self.host}:{self.port} ({label})"


# --------------------------------------------------------------------------- #
# ss:// 链接解析
# --------------------------------------------------------------------------- #

def _b64_decode(data: str) -> bytes | None:
    """urlsafe base64 解码，容忍缺失的 padding；失败返回 None。"""
    text = data.strip().replace("-", "+").replace("_", "/")
    if not text:
        return None
    text += "=" * (-len(text) % 4)
    try:
        return base64.b64decode(text, validate=True)
    except (binascii.Error, ValueError):
        return None


def _split_host_port(text: str) -> tuple[str, int]:
    text = text.strip()
    if text.startswith("["):  # IPv6: [::1]:8388
        host, sep, rest = text[1:].partition("]")
        port_text = rest.lstrip(":") if sep else ""
    else:
        host, sep, port_text = text.rpartition(":")
        if not sep:
            host, port_text = text, ""
    if not host:
        raise SsError(f"无法从 {text!r} 解析出服务器地址")
    if not port_text:
        raise SsError(f"无法从 {text!r} 解析出端口")
    try:
        port = int(port_text)
    except ValueError as exc:
        raise SsError(f"端口 {port_text!r} 不是数字") from exc
    if not 0 < port < 65536:
        raise SsError(f"端口 {port} 超出范围")
    return host, port


def parse_ss_url(url: str) -> SsServer:
    """解析 ``ss://`` 链接。

    支持三种常见形态::

        ss://base64(method:password)@host:port#tag
        ss://base64(method:password@host:port)#tag
        ss://method:password@host:port#tag
    """
    raw = url.strip()
    if raw.lower().startswith("ss://"):
        raw = raw[5:]
    elif "://" in raw:
        raise SsError(f"不是 ss:// 链接：{url}")

    tag = ""
    if "#" in raw:
        raw, _, tag = raw.partition("#")
        tag = unquote(tag)
    raw = raw.split("?", 1)[0]  # 丢弃 plugin/obfs 参数
    raw = raw.strip().rstrip("/")
    if not raw:
        raise SsError("ss:// 链接为空")

    userinfo, at, hostpart = raw.rpartition("@")
    if at:
        decoded = _b64_decode(userinfo)
        if decoded and b":" in decoded:
            credential = decoded.decode("utf-8", "replace")
        else:
            credential = unquote(userinfo)
        method, sep, password = credential.partition(":")
        if not sep:
            raise SsError(f"ss:// 链接缺少 method:password：{url}")
        host, port = _split_host_port(hostpart)
    else:
        decoded = _b64_decode(raw)
        if decoded is None or b"@" not in decoded:
            raise SsError(f"ss:// 链接格式无法识别：{url}")
        credential, _, hostport = decoded.decode("utf-8", "replace").rpartition("@")
        method, sep, password = credential.partition(":")
        if not sep:
            raise SsError(f"ss:// 链接缺少 method:password：{url}")
        host, port = _split_host_port(hostport)

    method = method.strip().lower()
    if method not in KEY_LEN:
        raise SsError(f"不支持的加密方式 {method!r}。{UNSUPPORTED_METHOD_HINT}")
    if not password:
        raise SsError("ss:// 链接中的密码为空")
    return SsServer(host=host, port=port, method=method, password=password, tag=tag)


# --------------------------------------------------------------------------- #
# 密钥派生
# --------------------------------------------------------------------------- #

def derive_key(password: str, key_len: int) -> bytes:
    """EVP_BytesToKey 的 MD5 变体（shadowsocks 主密钥派生）。"""
    password_bytes = password.encode("utf-8")
    out = b""
    prev = b""
    while len(out) < key_len:
        prev = hashlib.md5(prev + password_bytes).digest()
        out += prev
    return out[:key_len]


def hkdf(ikm: bytes, salt: bytes, info: bytes, length: int, hash_name: str = "sha1") -> bytes:
    """RFC 5869 HKDF（shadowsocks 用 SHA-1、info=b"ss-subkey"）。"""
    digestmod = getattr(hashlib, hash_name)
    prk = hmac.new(salt, ikm, digestmod).digest()
    out = b""
    block = b""
    counter = 1
    while len(out) < length:
        block = hmac.new(prk, block + info + bytes([counter]), digestmod).digest()
        out += block
        counter += 1
        if counter > 255:
            raise SsError("HKDF 输出长度过大")
    return out[:length]


# --------------------------------------------------------------------------- #
# AEAD 实现（优先 cryptography，缺失时用纯 Python ChaCha20-Poly1305 兜底）
# --------------------------------------------------------------------------- #

try:  # pragma: no cover - 取决于运行环境
    from cryptography.hazmat.primitives.ciphers.aead import AESGCM as _AESGCM
    from cryptography.hazmat.primitives.ciphers.aead import (
        ChaCha20Poly1305 as _CryptographyChaCha20Poly1305,
    )

    HAVE_CRYPTOGRAPHY = True
except ImportError:  # pragma: no cover
    HAVE_CRYPTOGRAPHY = False


def _rotl32(value: int, count: int) -> int:
    return ((value << count) | (value >> (32 - count))) & 0xFFFFFFFF


def _chacha20_block(key: bytes, counter: int, nonce: bytes) -> bytes:
    constants = struct.unpack("<4I", b"expand 32-byte k")
    state = list(constants) + list(struct.unpack("<8I", key)) + [counter] + list(
        struct.unpack("<3I", nonce)
    )
    working = state[:]

    def quarter_round(a: int, b: int, c: int, d: int) -> None:
        working[a] = (working[a] + working[b]) & 0xFFFFFFFF
        working[d] = _rotl32(working[d] ^ working[a], 16)
        working[c] = (working[c] + working[d]) & 0xFFFFFFFF
        working[b] = _rotl32(working[b] ^ working[c], 12)
        working[a] = (working[a] + working[b]) & 0xFFFFFFFF
        working[d] = _rotl32(working[d] ^ working[a], 8)
        working[c] = (working[c] + working[d]) & 0xFFFFFFFF
        working[b] = _rotl32(working[b] ^ working[c], 7)

    for _ in range(10):
        quarter_round(0, 4, 8, 12)
        quarter_round(1, 5, 9, 13)
        quarter_round(2, 6, 10, 14)
        quarter_round(3, 7, 11, 15)
        quarter_round(0, 5, 10, 15)
        quarter_round(1, 6, 11, 12)
        quarter_round(2, 7, 8, 13)
        quarter_round(3, 4, 9, 14)

    return struct.pack("<16I", *((working[i] + state[i]) & 0xFFFFFFFF for i in range(16)))


def _chacha20_xor(key: bytes, counter: int, nonce: bytes, data: bytes) -> bytes:
    out = bytearray(len(data))
    for offset in range(0, len(data), 64):
        keystream = _chacha20_block(key, counter + offset // 64, nonce)
        block = data[offset : offset + 64]
        for i, byte in enumerate(block):
            out[offset + i] = byte ^ keystream[i]
    return bytes(out)


def _poly1305(msg: bytes, key: bytes) -> bytes:
    r = int.from_bytes(key[:16], "little") & 0x0FFFFFFC0FFFFFFC0FFFFFFC0FFFFFFF
    s = int.from_bytes(key[16:32], "little")
    modulus = (1 << 130) - 5
    accumulator = 0
    for offset in range(0, len(msg), 16):
        block = msg[offset : offset + 16]
        accumulator = ((accumulator + int.from_bytes(block + b"\x01", "little")) * r) % modulus
    return ((accumulator + s) & ((1 << 128) - 1)).to_bytes(16, "little")


def _pad16(data: bytes) -> bytes:
    return data + b"\x00" * (-len(data) % 16)


class PureChaCha20Poly1305:
    """RFC 8439 ChaCha20-Poly1305 AEAD（纯 Python）。

    接口与 ``cryptography`` 的 AEAD 一致：``encrypt(nonce, data, aad)`` /
    ``decrypt(nonce, data, aad)``。速度约为 cryptography 的百分之一，仅作为
    没有 cryptography 时的兜底，让 chacha20 节点仍可用。
    """

    def __init__(self, key: bytes) -> None:
        if len(key) != 32:
            raise ValueError("ChaCha20-Poly1305 需要 32 字节密钥")
        self._key = key

    def _tag(self, nonce: bytes, ciphertext: bytes, aad: bytes) -> bytes:
        poly_key = _chacha20_block(self._key, 0, nonce)[:32]
        mac_data = _pad16(aad) + _pad16(ciphertext) + struct.pack("<QQ", len(aad), len(ciphertext))
        return _poly1305(mac_data, poly_key)

    def encrypt(self, nonce: bytes, data: bytes, aad: bytes | None = None) -> bytes:
        ciphertext = _chacha20_xor(self._key, 1, nonce, data)
        return ciphertext + self._tag(nonce, ciphertext, aad or b"")

    def decrypt(self, nonce: bytes, data: bytes, aad: bytes | None = None) -> bytes:
        if len(data) < 16:
            raise ValueError("密文长度不足，缺少 Poly1305 tag")
        ciphertext, tag = data[:-16], data[-16:]
        expected = self._tag(nonce, ciphertext, aad or b"")
        if not hmac.compare_digest(tag, expected):
            raise ValueError("Poly1305 校验失败")
        return _chacha20_xor(self._key, 1, nonce, ciphertext)


def make_cipher(method: str, key: bytes):
    """按加密方式构造 AEAD 对象（cryptography 优先）。"""
    if method.startswith("aes-"):
        if not HAVE_CRYPTOGRAPHY:
            raise SsError(
                f"{method} 需要 cryptography 库：pip install cryptography"
                "（或改用 chacha20-ietf-poly1305 节点）"
            )
        return _AESGCM(key)
    if method.startswith("chacha20"):
        if HAVE_CRYPTOGRAPHY:
            return _CryptographyChaCha20Poly1305(key)
        return PureChaCha20Poly1305(key)
    raise SsError(f"不支持的加密方式 {method!r}。{UNSUPPORTED_METHOD_HINT}")


class Direction:
    """单向 AEAD 流：递增 nonce，长度块 + 数据块各自独立封帧。"""

    def __init__(self, cipher) -> None:
        self._cipher = cipher
        self._nonce = bytearray(12)

    def _next_nonce(self) -> bytes:
        nonce = bytes(self._nonce)
        for i in range(12):
            self._nonce[i] = (self._nonce[i] + 1) & 0xFF
            if self._nonce[i]:
                break
        return nonce

    def seal(self, payload: bytes) -> bytes:
        length_block = self._cipher.encrypt(self._next_nonce(), len(payload).to_bytes(2, "big"), None)
        data_block = self._cipher.encrypt(self._next_nonce(), payload, None)
        return length_block + data_block

    def open_length(self, encrypted: bytes) -> int:
        return int.from_bytes(self._cipher.decrypt(self._next_nonce(), encrypted, None), "big")

    def open_data(self, encrypted: bytes) -> bytes:
        return self._cipher.decrypt(self._next_nonce(), encrypted, None)


# --------------------------------------------------------------------------- #
# 地址头
# --------------------------------------------------------------------------- #

ATYP_IPV4 = 0x01
ATYP_DOMAIN = 0x03
ATYP_IPV6 = 0x04


def pack_target_address(host: str, port: int) -> bytes:
    """打包 shadowsocks 地址头（0x03 域名形式，除非 host 是 IP 字面量）。"""
    try:
        packed = socket.inet_pton(socket.AF_INET, host)
        return bytes([ATYP_IPV4]) + packed + struct.pack(">H", port)
    except OSError:
        pass
    try:
        packed = socket.inet_pton(socket.AF_INET6, host)
        return bytes([ATYP_IPV6]) + packed + struct.pack(">H", port)
    except OSError:
        pass
    encoded = host.encode("idna") if host.isascii() else host.encode("utf-8")
    if len(encoded) > 255:
        raise SsError(f"域名过长：{host}")
    return bytes([ATYP_DOMAIN, len(encoded)]) + encoded + struct.pack(">H", port)


def parse_target_address(data: bytes) -> tuple[str, int, int]:
    """解析地址头，返回 ``(host, port, 消耗的字节数)``。"""
    if not data:
        raise SsError("地址头为空")
    atyp = data[0]
    if atyp == ATYP_IPV4:
        if len(data) < 7:
            raise SsError("IPv4 地址头不完整")
        host = socket.inet_ntop(socket.AF_INET, data[1:5])
        offset = 5
    elif atyp == ATYP_IPV6:
        if len(data) < 19:
            raise SsError("IPv6 地址头不完整")
        host = socket.inet_ntop(socket.AF_INET6, data[1:17])
        offset = 17
    elif atyp == ATYP_DOMAIN:
        if len(data) < 2:
            raise SsError("域名地址头不完整")
        length = data[1]
        if len(data) < 2 + length + 2:
            raise SsError("域名地址头不完整")
        host = data[2 : 2 + length].decode("utf-8", "replace")
        offset = 2 + length
    else:
        raise SsError(f"未知的地址类型 0x{atyp:02x}")
    port = struct.unpack(">H", data[offset : offset + 2])[0]
    return host, port, offset + 2


def read_exact(sock: socket.socket, length: int) -> bytes:
    """从 socket 精确读取 length 字节；对端关闭时返回已读部分。"""
    chunks = []
    remaining = length
    while remaining > 0:
        chunk = sock.recv(min(remaining, 65536))
        if not chunk:
            break
        chunks.append(chunk)
        remaining -= len(chunk)
    return b"".join(chunks)


# --------------------------------------------------------------------------- #
# 隧道流
# --------------------------------------------------------------------------- #

class SsClientStream:
    """客户端方向：负责 salt 交换与分块封帧，对外暴露类 socket 接口。"""

    def __init__(self, sock: socket.socket, method: str, password: str) -> None:
        self._sock = sock
        self._method = method
        key_len = KEY_LEN[method]
        self._key_len = key_len
        self._master_key = derive_key(password, key_len)
        salt = os.urandom(key_len)
        subkey = hkdf(self._master_key, salt, b"ss-subkey", key_len)
        self._send = Direction(make_cipher(method, subkey))
        self._send_salt = salt
        self._recv: Direction | None = None
        self._buffer = b""
        self._closed = False

    def start(self) -> None:
        """发送 salt（首个字节必须是 salt，之后才是加密分块）。"""
        self._sock.sendall(self._send_salt)

    def sendall(self, data: bytes) -> None:
        for offset in range(0, len(data), MAX_PAYLOAD):
            self._sock.sendall(self._send.seal(data[offset : offset + MAX_PAYLOAD]))

    def _start_recv(self) -> None:
        salt = read_exact(self._sock, self._key_len)
        if len(salt) != self._key_len:
            raise SsError("读取服务端 salt 失败：连接被提前关闭")
        subkey = hkdf(self._master_key, salt, b"ss-subkey", self._key_len)
        self._recv = Direction(make_cipher(self._method, subkey))

    def _read_chunk(self) -> bytes:
        if self._recv is None:
            self._start_recv()
        assert self._recv is not None
        length_block = read_exact(self._sock, 2 + 16)
        if len(length_block) != 2 + 16:
            return b""
        length = self._recv.open_length(length_block)
        if length == 0 or length > MAX_PAYLOAD:
            raise SsError(f"分块长度非法：{length}")
        data_block = read_exact(self._sock, length + 16)
        if len(data_block) != length + 16:
            raise SsError("分块数据不完整")
        return self._recv.open_data(data_block)

    def connect_target(self, host: str, port: int) -> None:
        """首块明文必须是目标地址头。"""
        self.start()
        self.sendall(pack_target_address(host, port))

    def recv(self, size: int) -> bytes:
        while not self._buffer:
            chunk = self._read_chunk()
            if not chunk:
                return b""
            self._buffer += chunk
        data, self._buffer = self._buffer[:size], self._buffer[size:]
        return data

    def settimeout(self, timeout: float | None) -> None:
        self._sock.settimeout(timeout)

    def close(self) -> None:
        if self._closed:
            return
        self._closed = True
        try:
            self._sock.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass
        self._sock.close()
