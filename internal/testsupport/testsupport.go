// Package testsupport 提供测试用的假服务端（假 Telegram、假 SOCKS5、假 HTTP 代理）。
//
// 只被 _test.go 引用，不会进入正式二进制。
package testsupport

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FilePart 是收到的上传文件。
type FilePart struct {
	Filename string
	Data     []byte
}

// errNoParts 表示请求体是"没有任何 part 的 multipart"，真实 Telegram 会以 400 拒绝。
var errNoParts = errors.New("no multipart parts")

// Call 是一次 Bot API 调用记录。
type Call struct {
	Method      string
	Fields      map[string]string
	Files       map[string]FilePart
	ContentType string
	BodyLength  int64
}

// Scripted 是预置的响应（用于注入错误）。
type Scripted struct {
	Status  int
	Payload map[string]any
	Raw     string // 配合 useRaw：原样返回这段内容（可用来模拟"响应不是 JSON"）
	useRaw  bool
}

// FakeTelegram 是假 Bot API 服务端。
type FakeTelegram struct {
	server       *httptest.Server
	mu           sync.Mutex
	calls        []Call
	script       []Scripted
	methodScript map[string][]Scripted
	onCall       func(Call)
	slowMethod   map[string]time.Duration
}

// NewFakeTelegram 启动假服务端。
func NewFakeTelegram() *FakeTelegram {
	fake := &FakeTelegram{}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.handle))
	return fake
}

// URL 是服务地址，可直接当 api_base。
func (f *FakeTelegram) URL() string { return f.server.URL }

// Close 关闭服务端。
func (f *FakeTelegram) Close() { f.server.Close() }

// DelayMethod 让某个方法的响应先等一会儿（模拟慢网络/慢自检）。
func (f *FakeTelegram) DelayMethod(method string, delay time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.slowMethod == nil {
		f.slowMethod = map[string]time.Duration{}
	}
	f.slowMethod[method] = delay
}

// ScriptNext 预置下一条响应（先到先用）。
func (f *FakeTelegram) ScriptNext(status int, payload map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.script = append(f.script, Scripted{Status: status, Payload: payload})
}

// ScriptRaw 预置一段非 JSON 的原始响应。
func (f *FakeTelegram) ScriptRaw(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.script = append(f.script, Scripted{Status: status, Raw: body, useRaw: true})
}

// ScriptMethod 为某个方法预置响应。
func (f *FakeTelegram) ScriptMethod(method string, status int, payload map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.methodScript == nil {
		f.methodScript = map[string][]Scripted{}
	}
	f.methodScript[method] = append(f.methodScript[method], Scripted{Status: status, Payload: payload})
}

// OnCall 注册回调（用于在收到调用时通知测试）。
func (f *FakeTelegram) OnCall(fn func(Call)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onCall = fn
}

// Calls 返回全部调用记录。
func (f *FakeTelegram) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Call, len(f.calls))
	copy(out, f.calls)
	return out
}

// Methods 返回调用过的方法名序列。
func (f *FakeTelegram) Methods() []string {
	out := []string{}
	for _, call := range f.Calls() {
		out = append(out, call.Method)
	}
	return out
}

// CallsOf 返回某个方法的全部调用。
func (f *FakeTelegram) CallsOf(method string) []Call {
	out := []Call{}
	for _, call := range f.Calls() {
		if call.Method == method {
			out = append(out, call)
		}
	}
	return out
}

func (f *FakeTelegram) handle(w http.ResponseWriter, r *http.Request) {
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	fields, files, err := parseMultipart(r)
	if err != nil {
		if errors.Is(err, errNoParts) {
			// 与真实 API 一致：400 且 body 为空
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	call := Call{
		Method:      method,
		Fields:      fields,
		Files:       files,
		ContentType: r.Header.Get("Content-Type"),
		BodyLength:  r.ContentLength,
	}

	f.mu.Lock()
	f.calls = append(f.calls, call)
	scripted := f.takeScript(method)
	callback := f.onCall
	delay := f.slowMethod[method]
	f.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}

	if callback != nil {
		callback(call)
	}

	if scripted != nil {
		if scripted.useRaw {
			w.WriteHeader(scripted.Status)
			_, _ = w.Write([]byte(scripted.Raw))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(scripted.Status)
		_ = json.NewEncoder(w).Encode(scripted.Payload)
		return
	}

	result := map[string]any{"message_id": len(f.Calls())}
	if method == "getMe" {
		result = map[string]any{"id": 1, "username": "filebot_test", "is_bot": true}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func (f *FakeTelegram) takeScript(method string) *Scripted {
	if len(f.script) > 0 {
		item := f.script[0]
		f.script = f.script[1:]
		return &item
	}
	if list, ok := f.methodScript[method]; ok && len(list) > 0 {
		item := list[0]
		f.methodScript[method] = list[1:]
		return &item
	}
	return nil
}

func parseMultipart(r *http.Request) (map[string]string, map[string]FilePart, error) {
	fields := map[string]string{}
	files := map[string]FilePart{}
	// 空 body 的 POST 是合法的（真实 API 也接受，getMe 就走这条路）
	if r.ContentLength == 0 {
		return fields, files, nil
	}
	reader, err := r.MultipartReader()
	if err != nil {
		return fields, files, err
	}
	parts := 0
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fields, files, err
		}
		parts++
		name := part.FormName()
		data, err := io.ReadAll(part)
		if err != nil {
			return fields, files, err
		}
		if filename := part.FileName(); filename != "" {
			files[name] = FilePart{Filename: filename, Data: data}
		} else {
			fields[name] = string(data)
		}
		part.Close()
	}
	// 真实的 Telegram 会拒绝"零部件的 multipart"：HTTP 400 + 空 body。
	// 这里保持一致，否则这种请求在测试里全绿、到线上必挂。
	if parts == 0 {
		return fields, files, errNoParts
	}
	return fields, files, nil
}

// ------------------------------------------------------------------ //
// 假 SOCKS5 服务端
// ------------------------------------------------------------------ //

// FakeSocks5 是一个最小的 SOCKS5 服务端，用于验证客户端握手。
type FakeSocks5 struct {
	listener net.Listener
	mu       sync.Mutex
	targets  []string
	methods  [][]byte

	// 需要认证时设置
	Username string
	Password string

	done chan struct{}
}

// NewFakeSocks5 启动假 SOCKS5 代理。
func NewFakeSocks5() (*FakeSocks5, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	fake := &FakeSocks5{listener: listener, done: make(chan struct{})}
	go fake.serve()
	return fake, nil
}

// Addr 返回 127.0.0.1:port。
func (f *FakeSocks5) Addr() string { return f.listener.Addr().String() }

// Targets 返回建连过的目标地址。
func (f *FakeSocks5) Targets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.targets))
	copy(out, f.targets)
	return out
}

// Methods 返回客户端提供的认证方式。
func (f *FakeSocks5) Methods() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.methods))
	copy(out, f.methods)
	return out
}

// Close 关闭监听。
func (f *FakeSocks5) Close() { f.listener.Close() }

func (f *FakeSocks5) serve() {
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			close(f.done)
			return
		}
		go f.handle(conn)
	}
}

func (f *FakeSocks5) handle(conn net.Conn) {
	defer conn.Close()
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil {
		return
	}
	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	f.mu.Lock()
	f.methods = append(f.methods, append([]byte(nil), methods...))
	needAuth := f.Username != ""
	f.mu.Unlock()

	if needAuth {
		if !containsByte(methods, 0x02) {
			conn.Write([]byte{0x05, 0xFF})
			return
		}
		conn.Write([]byte{0x05, 0x02})
		if err := f.auth(conn); err != nil {
			return
		}
	} else {
		conn.Write([]byte{0x05, 0x00})
	}

	request := make([]byte, 4)
	if _, err := io.ReadFull(conn, request); err != nil {
		return
	}
	host, port, err := readAddr(conn, request[3])
	if err != nil {
		conn.Write([]byte{0x05, 0x08, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	target := net.JoinHostPort(host, strconv.Itoa(port))
	f.mu.Lock()
	f.targets = append(f.targets, target)
	f.mu.Unlock()

	remote, err := net.Dial("tcp", target)
	if err != nil {
		conn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer remote.Close()
	conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	relay(conn, remote)
}

func (f *FakeSocks5) auth(conn net.Conn) error {
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil {
		return err
	}
	user := make([]byte, int(head[1]))
	if _, err := io.ReadFull(conn, user); err != nil {
		return err
	}
	length := make([]byte, 1)
	if _, err := io.ReadFull(conn, length); err != nil {
		return err
	}
	pass := make([]byte, int(length[0]))
	if _, err := io.ReadFull(conn, pass); err != nil {
		return err
	}
	if string(user) != f.Username || string(pass) != f.Password {
		conn.Write([]byte{0x01, 0x01})
		return fmt.Errorf("bad credentials")
	}
	conn.Write([]byte{0x01, 0x00})
	return nil
}

func readAddr(conn net.Conn, atyp byte) (string, int, error) {
	var host string
	switch atyp {
	case 0x01:
		buf := make([]byte, 4)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return "", 0, err
		}
		host = net.IP(buf).String()
	case 0x04:
		buf := make([]byte, 16)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return "", 0, err
		}
		host = net.IP(buf).String()
	case 0x03:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return "", 0, err
		}
		buf := make([]byte, int(length[0]))
		if _, err := io.ReadFull(conn, buf); err != nil {
			return "", 0, err
		}
		host = string(buf)
	default:
		return "", 0, fmt.Errorf("未知地址类型 0x%02x", atyp)
	}
	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBuf); err != nil {
		return "", 0, err
	}
	return host, int(binary.BigEndian.Uint16(portBuf)), nil
}

func containsByte(data []byte, want byte) bool {
	for _, item := range data {
		if item == want {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------------ //
// 假 HTTP 代理（CONNECT）
// ------------------------------------------------------------------ //

// FakeHTTPProxy 是支持 CONNECT 的假 HTTP 代理。
type FakeHTTPProxy struct {
	listener net.Listener
	mu       sync.Mutex
	targets  []string
}

// NewFakeHTTPProxy 启动假代理。
func NewFakeHTTPProxy() (*FakeHTTPProxy, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	fake := &FakeHTTPProxy{listener: listener}
	go fake.serve()
	return fake, nil
}

// Addr 返回监听地址。
func (f *FakeHTTPProxy) Addr() string { return f.listener.Addr().String() }

// URL 返回可用作 proxy.url 的链接。
func (f *FakeHTTPProxy) URL() string { return "http://" + f.Addr() }

// Targets 返回 CONNECT 过的目标。
func (f *FakeHTTPProxy) Targets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.targets))
	copy(out, f.targets)
	return out
}

// Close 关闭监听。
func (f *FakeHTTPProxy) Close() { f.listener.Close() }

func (f *FakeHTTPProxy) serve() {
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *FakeHTTPProxy) handle(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		return
	}
	parts := strings.Fields(strings.TrimSpace(line))
	if len(parts) < 3 {
		return
	}
	method, requestTarget := parts[0], parts[1]

	if strings.EqualFold(method, "CONNECT") {
		// 必须先读完整个请求头（到空行）再建隧道：否则请求头如果分多个 TCP 段到达，
		// 后面那半截会被当成隧道数据转发给目标（真实代理不会这么做）
		drainHeader(reader)
		f.tunnel(conn, requestTarget)
		return
	}
	f.forward(conn, reader, method, requestTarget, parts[2])
}

// drainHeader 读掉请求头剩余部分，直到空行。
func drainHeader(reader *bufio.Reader) {
	for {
		line, err := reader.ReadString('\n')
		if err != nil || strings.TrimSpace(line) == "" {
			return
		}
	}
}

// tunnel 处理 CONNECT（HTTPS 走这条）。
func (f *FakeHTTPProxy) tunnel(conn net.Conn, target string) {
	remote, err := net.Dial("tcp", target)
	if err != nil {
		conn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"))
		return
	}
	defer remote.Close()
	f.record(target)
	conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	relay(conn, remote)
}

// forward 处理绝对 URI 形式（明文 HTTP 走这条）。
func (f *FakeHTTPProxy) forward(conn net.Conn, reader *bufio.Reader, method, target, version string) {
	parsed, err := url.Parse(target)
	if err != nil || parsed.Host == "" {
		conn.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n"))
		return
	}
	host := parsed.Host
	if parsed.Port() == "" {
		host = net.JoinHostPort(parsed.Hostname(), "80")
	}
	remote, err := net.Dial("tcp", host)
	if err != nil {
		conn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"))
		return
	}
	defer remote.Close()
	f.record(host)

	header := &strings.Builder{}
	for {
		text, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		header.WriteString(text)
		if strings.TrimSpace(text) == "" {
			break
		}
	}
	// 把请求行改回 origin-form
	if _, err := fmt.Fprintf(remote, "%s %s %s\r\n", method, parsed.RequestURI(), version); err != nil {
		return
	}
	if _, err := remote.Write([]byte(header.String())); err != nil {
		return
	}
	if buffered := reader.Buffered(); buffered > 0 {
		chunk := make([]byte, buffered)
		if _, err := reader.Read(chunk); err != nil {
			return
		}
		if _, err := remote.Write(chunk); err != nil {
			return
		}
	}
	relay(conn, remote)
}

func (f *FakeHTTPProxy) record(target string) {
	f.mu.Lock()
	f.targets = append(f.targets, target)
	f.mu.Unlock()
}

func relay(a, b net.Conn) {
	done := make(chan struct{}, 2)
	copyFn := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if tcp, ok := dst.(*net.TCPConn); ok {
			tcp.CloseWrite()
		}
		done <- struct{}{}
	}
	go copyFn(a, b)
	go copyFn(b, a)
	<-done
	<-done
}
