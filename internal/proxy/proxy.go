// Package proxy 把 proxy.url 变成 http.Transport 需要的拨号方式。
//
// 支持：http:// / https://（交给 net/http 的 Proxy，自动处理 CONNECT 与绝对 URI）、
// socks5://（自写握手，域名交给代理解析，等价于 socks5h）。
// 留空表示直连；不认识的协议直接报错，绝不静默直连。
package proxy

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// New 按代理链接构造 http.Transport。rawURL 为空时返回直连 transport。
func New(rawURL string, timeout time.Duration) (*http.Transport, error) {
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		ExpectContinueTimeout: 10 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" || strings.EqualFold(rawURL, "direct") || strings.EqualFold(rawURL, "none") {
		return transport, nil
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("代理链接无法解析：%w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	host := parsed.Hostname()
	if host == "" {
		return nil, fmt.Errorf("代理链接缺少主机名：%s", rawURL)
	}

	switch scheme {
	case "http", "https":
		port := parsed.Port()
		if port == "" {
			if scheme == "https" {
				port = "443"
			} else {
				port = "80"
			}
			parsed.Host = net.JoinHostPort(host, port)
		}
		transport.Proxy = http.ProxyURL(parsed)
		return transport, nil
	case "socks5", "socks5h", "socks":
		port := parsed.Port()
		if port == "" {
			port = "1080"
		}
		username, password := "", ""
		if parsed.User != nil {
			username = parsed.User.Username()
			password, _ = parsed.User.Password()
		}
		dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		transport.Proxy = nil
		transport.DialContext = socks5DialContext(net.JoinHostPort(host, port), username, password, dialer)
		return transport, nil
	default:
		return nil, fmt.Errorf("不支持的代理协议 %q：只支持 http:// 与 socks5://，留空表示直连", scheme)
	}
}

// socks5DialContext 返回一个通过 SOCKS5 代理建连的 DialContext（RFC 1928 + RFC 1929）。
func socks5DialContext(proxyAddr, username, password string, dialer *net.Dialer) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, "tcp", proxyAddr)
		if err != nil {
			return nil, fmt.Errorf("连接 SOCKS5 代理 %s 失败：%w", proxyAddr, err)
		}
		if deadline, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(deadline)
		} else {
			_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
		}
		if err := socks5Handshake(conn, address, username, password); err != nil {
			conn.Close()
			return nil, err
		}
		_ = conn.SetDeadline(time.Time{})
		return conn, nil
	}
}

func socks5Handshake(conn net.Conn, address, username, password string) error {
	useAuth := username != ""
	methods := []byte{0x00}
	if useAuth {
		methods = append(methods, 0x02)
	}
	greeting := append([]byte{0x05, byte(len(methods))}, methods...)
	if _, err := conn.Write(greeting); err != nil {
		return err
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fmt.Errorf("SOCKS5 握手失败：%w", err)
	}
	if reply[0] != 0x05 {
		return fmt.Errorf("SOCKS5 版本异常：0x%02x", reply[0])
	}
	switch reply[1] {
	case 0x00:
		// 无需认证
	case 0x02:
		if !useAuth {
			return errors.New("SOCKS5 代理要求用户名/密码认证，请在代理链接里写上 socks5://user:pass@host:port")
		}
		if err := socks5Auth(conn, username, password); err != nil {
			return err
		}
	case 0xFF:
		return errors.New("SOCKS5 代理拒绝了所有认证方式")
	default:
		return fmt.Errorf("SOCKS5 代理返回了不支持的认证方式 0x%02x", reply[1])
	}

	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("目标地址非法 %q：%w", address, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return fmt.Errorf("目标端口非法 %q", portText)
	}
	request := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			request = append(request, 0x01)
			request = append(request, ip4...)
		} else {
			request = append(request, 0x04)
			request = append(request, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return fmt.Errorf("域名过长：%s", host)
		}
		request = append(request, 0x03, byte(len(host)))
		request = append(request, host...)
	}
	request = binary.BigEndian.AppendUint16(request, uint16(port))
	if _, err := conn.Write(request); err != nil {
		return err
	}

	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fmt.Errorf("读取 SOCKS5 应答失败：%w", err)
	}
	if header[1] != 0x00 {
		return fmt.Errorf("SOCKS5 建连被拒绝：%s", socks5ReplyText(header[1]))
	}
	switch header[3] {
	case 0x01:
		_, err = io.ReadFull(conn, make([]byte, 4+2))
	case 0x04:
		_, err = io.ReadFull(conn, make([]byte, 16+2))
	case 0x03:
		length := make([]byte, 1)
		if _, err = io.ReadFull(conn, length); err == nil {
			_, err = io.ReadFull(conn, make([]byte, int(length[0])+2))
		}
	default:
		return fmt.Errorf("SOCKS5 应答地址类型未知：0x%02x", header[3])
	}
	return err
}

func socks5Auth(conn net.Conn, username, password string) error {
	if len(username) > 255 || len(password) > 255 {
		return errors.New("SOCKS5 用户名/密码过长")
	}
	payload := []byte{0x01, byte(len(username))}
	payload = append(payload, username...)
	payload = append(payload, byte(len(password)))
	payload = append(payload, password...)
	if _, err := conn.Write(payload); err != nil {
		return err
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return err
	}
	if reply[1] != 0x00 {
		return errors.New("SOCKS5 用户名/密码认证失败")
	}
	return nil
}

func socks5ReplyText(code byte) string {
	switch code {
	case 0x01:
		return "代理内部错误"
	case 0x02:
		return "规则不允许"
	case 0x03:
		return "网络不可达"
	case 0x04:
		return "主机不可达"
	case 0x05:
		return "连接被拒绝"
	case 0x06:
		return "TTL 超时"
	case 0x07:
		return "命令不支持"
	case 0x08:
		return "地址类型不支持"
	default:
		return fmt.Sprintf("未知错误 0x%02x", code)
	}
}
