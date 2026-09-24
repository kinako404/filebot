package proxy

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"filebot/internal/testsupport"
)

func TestDirectWhenEmpty(t *testing.T) {
	for _, value := range []string{"", "   ", "direct", "none"} {
		transport, err := New(value, 10*time.Second)
		if err != nil {
			t.Fatalf("%q: %v", value, err)
		}
		if transport.Proxy != nil || transport.DialContext == nil {
			t.Fatalf("%q 应当是直连 transport", value)
		}
	}
}

func TestUnknownSchemeRejected(t *testing.T) {
	if _, err := New("ftp://127.0.0.1:21", time.Second); err == nil {
		t.Fatal("应当报错")
	}
	if _, err := New("ss://whatever@1.2.3.4:8388", time.Second); err == nil {
		t.Fatal("ss:// 不再支持，应当报错而不是静默直连")
	}
	if _, err := New("socks5://", time.Second); err == nil {
		t.Fatal("缺少主机名应当报错")
	}
}

func newEchoServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello-through-proxy")
	}))
	t.Cleanup(server.Close)
	return server
}

func clientFor(transport *http.Transport) *http.Client {
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	return &http.Client{Transport: transport, Timeout: 20 * time.Second}
}

func TestSocks5ProxyChain(t *testing.T) {
	target := newEchoServer(t)
	socks, err := testsupport.NewFakeSocks5()
	if err != nil {
		t.Fatal(err)
	}
	defer socks.Close()

	transport, err := New("socks5://"+socks.Addr(), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := clientFor(transport).Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "hello-through-proxy") {
		t.Fatalf("body = %q", body)
	}

	targets := socks.Targets()
	if len(targets) != 1 {
		t.Fatalf("代理没有收到建连请求：%v", targets)
	}
	// 域名/地址交给代理解析，所以看到的是 host:port 原样
	want := strings.TrimPrefix(target.URL, "https://")
	if targets[0] != want {
		t.Fatalf("目标 = %v，期望 %v", targets, want)
	}
	methods := socks.Methods()
	if len(methods) == 0 || len(methods[0]) != 1 || methods[0][0] != 0x00 {
		t.Fatalf("无认证时应当只提供 0x00：%v", methods)
	}
}

func TestSocks5WithAuth(t *testing.T) {
	target := newEchoServer(t)
	socks, err := testsupport.NewFakeSocks5()
	if err != nil {
		t.Fatal(err)
	}
	socks.Username, socks.Password = "user", "p@ss"
	defer socks.Close()

	transport, err := New("socks5://user:p%40ss@"+socks.Addr(), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := clientFor(transport).Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(socks.Methods()) == 0 || len(socks.Methods()[0]) != 2 {
		t.Fatalf("带认证时应当提供两种方式：%v", socks.Methods())
	}
}

func TestSocks5AuthFailure(t *testing.T) {
	target := newEchoServer(t)
	socks, err := testsupport.NewFakeSocks5()
	if err != nil {
		t.Fatal(err)
	}
	socks.Username, socks.Password = "user", "right"
	defer socks.Close()

	transport, err := New("socks5://user:wrong@"+socks.Addr(), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clientFor(transport).Get(target.URL); err == nil {
		t.Fatal("认证失败应当报错")
	}
}

func TestSocks5MissingCredentials(t *testing.T) {
	target := newEchoServer(t)
	socks, err := testsupport.NewFakeSocks5()
	if err != nil {
		t.Fatal(err)
	}
	socks.Username, socks.Password = "user", "pass"
	defer socks.Close()

	transport, err := New("socks5://"+socks.Addr(), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = clientFor(transport).Get(target.URL)
	if err == nil || !strings.Contains(err.Error(), "认证") {
		t.Fatalf("应当提示需要认证：%v", err)
	}
}

func TestSocks5ConnectionRefused(t *testing.T) {
	socks, err := testsupport.NewFakeSocks5()
	if err != nil {
		t.Fatal(err)
	}
	defer socks.Close()

	// 找一个没人监听的端口
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := probe.Addr().String()
	probe.Close()

	transport, err := New("socks5://"+socks.Addr(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = clientFor(transport).Get("https://" + deadAddr + "/")
	if err == nil || !strings.Contains(err.Error(), "拒绝") {
		t.Fatalf("应当报连接被拒绝：%v", err)
	}
}

func TestHTTPProxyChain(t *testing.T) {
	target := newEchoServer(t)
	proxyServer, err := testsupport.NewFakeHTTPProxy()
	if err != nil {
		t.Fatal(err)
	}
	defer proxyServer.Close()

	transport, err := New(proxyServer.URL(), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := clientFor(transport).Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "hello-through-proxy") {
		t.Fatalf("body = %q", body)
	}
	if len(proxyServer.Targets()) != 1 {
		t.Fatalf("代理没有收到 CONNECT：%v", proxyServer.Targets())
	}
}

func TestHTTPProxyAddressWithoutPort(t *testing.T) {
	transport, err := New("http://127.0.0.1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	url, err := transport.Proxy(&http.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if url.Host != "127.0.0.1:80" {
		t.Fatalf("默认端口没补上：%s", url.Host)
	}
}

func TestSocks5HandshakeAgainstClosedPort(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close()

	transport, err := New("socks5://"+addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clientFor(transport).Get("https://127.0.0.1:1/"); err == nil {
		t.Fatal("应当报错")
	}
}
