package tg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"filebot/internal/testsupport"
)

func newTestClient(t *testing.T, fake *testsupport.FakeTelegram, mutate func(*Options)) *Client {
	t.Helper()
	opt := Options{
		Token:      "test:token",
		APIBase:    fake.URL(),
		Timeout:    30 * time.Second,
		MaxRetries: 3,
	}
	if mutate != nil {
		mutate(&opt)
	}
	client, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func writeFile(t *testing.T, name string, size int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMultipartBodyLengthMatchesBytes(t *testing.T) {
	path := writeFile(t, "样本 文件.bin", 5000)
	file := &FilePart{Field: "document", Path: path, Filename: "样本 文件.bin", ContentType: "application/octet-stream"}
	body, err := newMultipartBody(map[string]string{"chat_id": "123", "caption": "hello"}, file)
	if err != nil {
		t.Fatal(err)
	}
	serialized, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(serialized)) != body.Len() {
		t.Fatalf("Content-Length 不符：声明 %d，实际 %d", body.Len(), len(serialized))
	}
	if !bytes.Contains(serialized, []byte(`name="chat_id"`)) {
		t.Error("缺少 chat_id 字段")
	}
	if !bytes.Contains(serialized, []byte("样本 文件.bin")) {
		t.Error("文件名应当按 UTF-8 编码进请求体")
	}
	if !bytes.HasSuffix(serialized, []byte("--"+body.boundary+"--\r\n")) {
		t.Error("multipart 结尾不对")
	}
}

func TestMultipartBodyCanBeReplayed(t *testing.T) {
	path := writeFile(t, "a.bin", 3000)
	file := &FilePart{Field: "document", Path: path, Filename: "a.bin", ContentType: "application/octet-stream"}
	body, err := newMultipartBody(map[string]string{"chat_id": "1"}, file)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := io.ReadAll(body)
	body.Reset()
	second, _ := io.ReadAll(body)
	if !bytes.Equal(first, second) {
		t.Fatal("Reset 之后应当能完整重放")
	}
	body.Reset()
	chunk := make([]byte, 100)
	n, _ := body.Read(chunk)
	body.Reset()
	again, _ := io.ReadAll(body)
	if len(again) != len(first) || n != 100 {
		t.Fatalf("分段读取后 Reset 失败：%d vs %d", len(again), len(first))
	}
}

func TestFilenameEscaping(t *testing.T) {
	path := writeFile(t, "x.bin", 10)
	file := &FilePart{Field: "document", Path: path, Filename: "we\"ird\\name.jpg", ContentType: "image/jpeg"}
	body, err := newMultipartBody(nil, file)
	if err != nil {
		t.Fatal(err)
	}
	serialized, _ := io.ReadAll(body)
	if !bytes.Contains(serialized, []byte(`filename="we%22ird%5Cname.jpg"`)) {
		t.Fatalf("转义不对：%s", firstLine(serialized))
	}
}

func firstLine(data []byte) string {
	if idx := bytes.IndexByte(data, '\n'); idx >= 0 {
		return string(data[:idx])
	}
	return string(data)
}

func TestSendDocumentSendsExactBytes(t *testing.T) {
	fake := testsupport.NewFakeTelegram()
	defer fake.Close()
	client := newTestClient(t, fake, nil)

	path := writeFile(t, "报告 2024.mp4", 4096)
	expected, _ := os.ReadFile(path)
	if err := client.SendDocument(context.Background(), "-100123", path, "标题"); err != nil {
		t.Fatal(err)
	}
	calls := fake.CallsOf("sendDocument")
	if len(calls) != 1 {
		t.Fatalf("调用次数 = %d", len(calls))
	}
	call := calls[0]
	if call.Fields["chat_id"] != "-100123" || call.Fields["caption"] != "标题" {
		t.Fatalf("fields = %v", call.Fields)
	}
	part := call.Files["document"]
	if part.Filename != "报告 2024.mp4" {
		t.Fatalf("filename = %q", part.Filename)
	}
	if !bytes.Equal(part.Data, expected) {
		t.Fatal("收到的字节与原文件不一致")
	}
}

func TestMediaMethodsUseCorrectFields(t *testing.T) {
	fake := testsupport.NewFakeTelegram()
	defer fake.Close()
	client := newTestClient(t, fake, nil)
	ctx := context.Background()

	if err := client.SendPhoto(ctx, "@channel", writeFile(t, "a.jpg", 1000), ""); err != nil {
		t.Fatal(err)
	}
	if err := client.SendAnimation(ctx, "@channel", writeFile(t, "a.gif", 1000), ""); err != nil {
		t.Fatal(err)
	}
	if err := client.SendVideo(ctx, "@channel", writeFile(t, "a.mp4", 1000), "cap"); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.CallsOf("sendPhoto")[0].Files["photo"]; !ok {
		t.Error("sendPhoto 应当用 photo 字段")
	}
	if _, ok := fake.CallsOf("sendAnimation")[0].Files["animation"]; !ok {
		t.Error("sendAnimation 应当用 animation 字段")
	}
	video := fake.CallsOf("sendVideo")[0]
	if _, ok := video.Files["video"]; !ok {
		t.Error("sendVideo 应当用 video 字段")
	}
	if video.Fields["supports_streaming"] != "true" {
		t.Error("视频要开启流式播放")
	}
	if _, ok := fake.CallsOf("sendPhoto")[0].Fields["caption"]; ok {
		t.Error("空 caption 不应发送")
	}
}

func TestGetMe(t *testing.T) {
	fake := testsupport.NewFakeTelegram()
	defer fake.Close()
	client := newTestClient(t, fake, nil)
	me, err := client.GetMe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if me["username"] != "filebot_test" {
		t.Fatalf("me = %v", me)
	}
}

func TestCaptionTruncated(t *testing.T) {
	fake := testsupport.NewFakeTelegram()
	defer fake.Close()
	client := newTestClient(t, fake, nil)
	if err := client.SendPhoto(context.Background(), "1", writeFile(t, "a.jpg", 100), strings.Repeat("x", 5000)); err != nil {
		t.Fatal(err)
	}
	caption := fake.CallsOf("sendPhoto")[0].Fields["caption"]
	if len([]rune(caption)) != CaptionLimit {
		t.Fatalf("caption 长度 = %d", len([]rune(caption)))
	}
}

func TestRetryOn429(t *testing.T) {
	fake := testsupport.NewFakeTelegram()
	defer fake.Close()
	fake.ScriptNext(429, map[string]any{
		"ok": false, "error_code": 429, "description": "Too Many Requests",
		"parameters": map[string]any{"retry_after": 0},
	})
	client := newTestClient(t, fake, nil)
	if err := client.SendPhoto(context.Background(), "1", writeFile(t, "a.jpg", 100), ""); err != nil {
		t.Fatal(err)
	}
	if got := len(fake.CallsOf("sendPhoto")); got != 2 {
		t.Fatalf("应当重试一次，实际调用 %d 次", got)
	}
}

func TestRetryOnServerError(t *testing.T) {
	fake := testsupport.NewFakeTelegram()
	defer fake.Close()
	fake.ScriptNext(500, map[string]any{"ok": false, "description": "boom"})
	fake.ScriptNext(502, map[string]any{"ok": false, "description": "boom"})
	client := newTestClient(t, fake, nil)
	if err := client.SendPhoto(context.Background(), "1", writeFile(t, "a.jpg", 100), ""); err != nil {
		t.Fatal(err)
	}
	if got := len(fake.CallsOf("sendPhoto")); got != 3 {
		t.Fatalf("应当重试两次，实际 %d 次", got)
	}
}

func TestClientErrorIsNotRetried(t *testing.T) {
	fake := testsupport.NewFakeTelegram()
	defer fake.Close()
	fake.ScriptNext(400, map[string]any{
		"ok": false, "error_code": 400, "description": "Bad Request: chat not found",
	})
	client := newTestClient(t, fake, nil)
	err := client.SendPhoto(context.Background(), "1", writeFile(t, "a.jpg", 100), "")
	if err == nil {
		t.Fatal("应当报错")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("应当是 APIError：%v", err)
	}
	if apiErr.Code != 400 || !strings.Contains(apiErr.Description, "chat not found") {
		t.Fatalf("apiErr = %+v", apiErr)
	}
	if got := len(fake.CallsOf("sendPhoto")); got != 1 {
		t.Fatalf("400 不应重试，实际 %d 次", got)
	}
	if IsRetryable(err) {
		t.Error("APIError 不应被标记为可重试")
	}
}

func TestConnectionErrorIsRetried(t *testing.T) {
	client, err := New(Options{Token: "t", APIBase: "http://127.0.0.1:1", MaxRetries: 2, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := client.SendPhoto(context.Background(), "1", writeFile(t, "a.jpg", 100), ""); err == nil {
		t.Fatal("应当报错")
	}
	if time.Since(start) < time.Second {
		t.Error("重试之间应当有退避等待")
	}
}

func TestInvalidOptions(t *testing.T) {
	if _, err := New(Options{Token: "", APIBase: "https://api.telegram.org"}); err == nil {
		t.Error("缺少 token 应当报错")
	}
	if _, err := New(Options{Token: "t", APIBase: "ftp://example.com"}); err == nil {
		t.Error("非法 api_base 应当报错")
	}
}

func TestSendMessageTruncates(t *testing.T) {
	fake := testsupport.NewFakeTelegram()
	defer fake.Close()
	client := newTestClient(t, fake, nil)
	long := strings.Repeat("很", 5000)
	if err := client.SendMessage(context.Background(), "1", long); err != nil {
		t.Fatal(err)
	}
	text := fake.CallsOf("sendMessage")[0].Fields["text"]
	if len([]rune(text)) != 4096 {
		t.Fatalf("长度 = %d", len([]rune(text)))
	}
}

func TestUploadMissingFile(t *testing.T) {
	fake := testsupport.NewFakeTelegram()
	defer fake.Close()
	client := newTestClient(t, fake, nil)
	if err := client.SendDocument(context.Background(), "1", "/definitely/missing.mp4", ""); err == nil {
		t.Fatal("应当报错")
	}
}

// getMe 这种没有参数的方法必须发空 body 的 POST。
// 真实 Telegram 会拒绝"零部件 multipart"（HTTP 400 + 空 body），
// 这个用例就是那条线上事故的回归测试。
func TestGetMeSendsEmptyBodyNotEmptyMultipart(t *testing.T) {
	fake := testsupport.NewFakeTelegram()
	defer fake.Close()
	client := newTestClient(t, fake, nil)
	if _, err := client.GetMe(context.Background()); err != nil {
		t.Fatal(err)
	}
	call := fake.CallsOf("getMe")[0]
	if call.BodyLength != 0 {
		t.Fatalf("getMe 应当发空 body，实际 %d 字节", call.BodyLength)
	}
	if strings.Contains(call.ContentType, "multipart/form-data") {
		t.Fatalf("getMe 不该用 multipart，实际 Content-Type=%q", call.ContentType)
	}
}

// 带字段的方法仍然走 multipart
func TestMethodsWithFieldsUseMultipart(t *testing.T) {
	fake := testsupport.NewFakeTelegram()
	defer fake.Close()
	client := newTestClient(t, fake, nil)
	if err := client.SendMessage(context.Background(), "1", "hi"); err != nil {
		t.Fatal(err)
	}
	call := fake.CallsOf("sendMessage")[0]
	if !strings.Contains(call.ContentType, "multipart/form-data") {
		t.Fatalf("sendMessage 应当用 multipart，实际 %q", call.ContentType)
	}
	if call.Fields["text"] != "hi" {
		t.Fatalf("fields = %v", call.Fields)
	}
}

// 4xx + 非 JSON 响应（比如请求体被拒）不该白重试，错误信息要能看懂
func TestNonJSONClientErrorIsNotRetried(t *testing.T) {
	fake := testsupport.NewFakeTelegram()
	defer fake.Close()
	fake.ScriptRaw(400, "")
	client := newTestClient(t, fake, nil)
	err := client.SendPhoto(context.Background(), "1", writeFile(t, "a.jpg", 100), "")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("应当是 APIError：%v", err)
	}
	if apiErr.Code != 400 || !strings.Contains(apiErr.Description, "空响应体") {
		t.Fatalf("apiErr = %+v", apiErr)
	}
	if len(fake.CallsOf("sendPhoto")) != 1 {
		t.Fatalf("4xx 不该重试，实际 %d 次", len(fake.CallsOf("sendPhoto")))
	}
}

// 非 JSON 的 5xx 仍然可重试
func TestNonJSONServerErrorIsRetried(t *testing.T) {
	fake := testsupport.NewFakeTelegram()
	defer fake.Close()
	fake.ScriptRaw(502, "<html>bad gateway</html>")
	client := newTestClient(t, fake, nil)
	if err := client.SendPhoto(context.Background(), "1", writeFile(t, "a.jpg", 100), ""); err != nil {
		t.Fatal(err)
	}
	if len(fake.CallsOf("sendPhoto")) != 2 {
		t.Fatalf("5xx 应当重试，实际 %d 次", len(fake.CallsOf("sendPhoto")))
	}
}

// 429 但 body 是 HTML（接入层/CDN 的限流页很常见）同样要退避重试
func TestNonJSONRateLimitIsRetried(t *testing.T) {
	fake := testsupport.NewFakeTelegram()
	defer fake.Close()
	fake.ScriptRaw(429, "<html>rate limited by cdn</html>")
	client := newTestClient(t, fake, nil)
	if err := client.SendPhoto(context.Background(), "1", writeFile(t, "a.jpg", 100), ""); err != nil {
		t.Fatal(err)
	}
	if got := len(fake.CallsOf("sendPhoto")); got != 2 {
		t.Fatalf("非 JSON 的 429 应当重试，实际 %d 次", got)
	}
}

// 非 JSON 的 400 重试没有意义，仍然是不可重试的 APIError
func TestNonJSONBadRequestIsNotRetried(t *testing.T) {
	fake := testsupport.NewFakeTelegram()
	defer fake.Close()
	fake.ScriptRaw(400, "<html>bad</html>")
	client := newTestClient(t, fake, nil)
	err := client.SendPhoto(context.Background(), "1", writeFile(t, "a.jpg", 100), "")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("应当是 APIError：%v", err)
	}
	if apiErr.Code != 400 || !strings.Contains(apiErr.Description, "bad") {
		t.Fatalf("apiErr = %+v", apiErr)
	}
	if IsRetryable(err) {
		t.Error("4xx 不该是可重试类型")
	}
	if got := len(fake.CallsOf("sendPhoto")); got != 1 {
		t.Fatalf("4xx 不该重试，实际 %d 次", got)
	}
}

// 重试必须重新 os.Stat：文件在两次尝试之间变大，
// 旧实现复用同一个过期长度，net/http 会以长度不符直接失败。
func TestRetryRestatsFileLength(t *testing.T) {
	fake := testsupport.NewFakeTelegram()
	defer fake.Close()
	fake.ScriptMethod("sendDocument", 500, map[string]any{"ok": false, "description": "boom"})

	path := writeFile(t, "grow.bin", 4096)
	// 第一次请求被服务端完整读完之后再改文件大小
	fake.OnCall(func(call testsupport.Call) {
		if call.Method == "sendDocument" && len(fake.CallsOf("sendDocument")) == 1 {
			payload := make([]byte, 9000)
			for i := range payload {
				payload[i] = byte(i % 251)
			}
			_ = os.WriteFile(path, payload, 0o644)
		}
	})

	client := newTestClient(t, fake, nil)
	if err := client.SendDocument(context.Background(), "1", path, ""); err != nil {
		t.Fatalf("重试应当成功（每次尝试都重新 stat）：%v", err)
	}
	calls := fake.CallsOf("sendDocument")
	if len(calls) != 2 {
		t.Fatalf("调用次数 = %d，期望 2", len(calls))
	}
	second := calls[1]
	if got := len(second.Files["document"].Data); got != 9000 {
		t.Fatalf("第二次应当发新长度 9000，实际 %d", got)
	}
	if want := multipartLength(t, second); second.BodyLength != want {
		t.Fatalf("声明的 Content-Length %d != 实际字节数 %d", second.BodyLength, want)
	}
}

// fileContentType 复刻 upload 的推断逻辑，供 multipartLength 重建请求体用
func fileContentType(name string) string {
	if contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); contentType != "" {
		return contentType
	}
	return "application/octet-stream"
}

// multipartLength 按服务端收到的内容重建请求体应有的字节数：
// 声明的 Content-Length 必须与它相等，否则 net/http 会写出截断的请求。
func multipartLength(t *testing.T, call testsupport.Call) int64 {
	t.Helper()
	_, params, err := mime.ParseMediaType(call.ContentType)
	if err != nil {
		t.Fatalf("Content-Type 解析失败：%v", err)
	}
	boundary := params["boundary"]
	if boundary == "" {
		t.Fatalf("Content-Type 缺少 boundary：%q", call.ContentType)
	}
	var total int64
	part := func(headers string, body []byte) {
		total += int64(len("--"+boundary+"\r\n") + len(headers) + len(body) + len("\r\n"))
	}
	for name, value := range call.Fields {
		part(fmt.Sprintf("Content-Disposition: form-data; name=%q\r\n\r\n", name), []byte(value))
	}
	for name, file := range call.Files {
		headers := fmt.Sprintf(
			"Content-Disposition: form-data; name=%q; filename=%q\r\nContent-Type: %s\r\n\r\n",
			name, escapeFilename(file.Filename), fileContentType(file.Filename),
		)
		part(headers, file.Data)
	}
	total += int64(len("--" + boundary + "--\r\n"))
	return total
}

// 跟随重定向会把上传静默降级成没有 body 的 GET，
// 如果中转对 GET 回 ok:true，上层还会以为文件已经发出去了。
func TestRedirectIsNotFollowed(t *testing.T) {
	var mu sync.Mutex
	var hits []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.URL.Path != "/redirected" {
			w.Header().Set("Location", "/redirected")
			w.WriteHeader(http.StatusMovedPermanently)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer server.Close()

	// token 用真实长度（真实 Bot token 是 <数字>:<长串>）：
	// 脱敏是按子串替换的，一两个字符的假 token 会把无关文本也顶掉。
	client, err := New(Options{Token: "123456:SECRET-TOKEN", APIBase: server.URL, Timeout: 5 * time.Second, MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	err = client.SendMessage(context.Background(), "1", "hi")
	if err == nil {
		t.Fatal("301 必须报错，不能跟随成 GET 后当成发送成功")
	}
	if !strings.Contains(err.Error(), "301") {
		t.Fatalf("错误信息里应当有状态码 301：%v", err)
	}
	if !strings.Contains(err.Error(), "/redirected") {
		t.Fatalf("错误信息里应当带上 Location（方便定位）：%v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != 301 {
		t.Fatalf("应当是 HTTP 301 的 APIError：%v", err)
	}
	if IsRetryable(err) {
		t.Error("重定向不该是可重试错误")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 1 {
		t.Fatalf("不该跟随重定向，实际收到的请求：%v", hits)
	}
}

// token 会出现在请求 URL 里，http.Client.Do 的报错又整条带出去，
// 配置写错 crash-loop 时会被反复写进持久化的 journal，必须脱敏。
func TestErrorsDoNotLeakToken(t *testing.T) {
	const token = "123456:SECRET-TOKEN"
	client, err := New(Options{Token: token, APIBase: "http://127.0.0.1:1", Timeout: 2 * time.Second, MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	err = client.SendPhoto(context.Background(), "1", writeFile(t, "a.jpg", 16), "")
	if err == nil {
		t.Fatal("应当报错")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("错误信息泄露了 token：%v", err)
	}
	if !strings.Contains(err.Error(), "<token>") {
		t.Fatalf("应当把 token 换成占位符：%v", err)
	}
	if !strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("脱敏后仍要保留定位信息（主机）：%v", err)
	}
	if !IsRetryable(err) {
		t.Fatalf("连接错误仍应当是可重试类型：%v", err)
	}
}
