// Package tg 是 Telegram Bot API 客户端。
//
// 只依赖标准库：multipart 请求体是自定义 io.Reader，按顺序流式读取文件，
// 预先算好 Content-Length，不把整个文件读进内存。
package tg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// CaptionLimit 是 Telegram 的单条 caption 上限。
const CaptionLimit = 1024

// APIError 是 Bot API 返回的业务错误（不重试）。
type APIError struct {
	Method      string
	Code        int
	Description string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: HTTP %d %s", e.Method, e.Code, e.Description)
}

type retryableError struct {
	err   error
	delay time.Duration
}

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

// Options 是客户端配置。
type Options struct {
	Token           string
	APIBase         string
	Timeout         time.Duration
	MaxRetries      int
	MinSendInterval time.Duration
	Transport       http.RoundTripper
}

// FilePart 是要上传的一个文件字段。
type FilePart struct {
	Field       string
	Path        string
	Filename    string
	ContentType string
}

// Client 是 Bot API 客户端。
type Client struct {
	token      string
	baseURL    string
	http       *http.Client
	maxRetries int
	minGap     time.Duration

	mu       sync.Mutex
	lastSend time.Time
}

// New 创建客户端。
func New(opt Options) (*Client, error) {
	if strings.TrimSpace(opt.Token) == "" {
		return nil, errors.New("缺少 Bot token")
	}
	base := strings.TrimRight(strings.TrimSpace(opt.APIBase), "/")
	if base == "" {
		base = "https://api.telegram.org"
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return nil, fmt.Errorf("api_base 必须是 http(s) 地址：%s", opt.APIBase)
	}
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	retries := opt.MaxRetries
	if retries < 1 {
		retries = 1
	}
	transport := opt.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &Client{
		token:      opt.Token,
		baseURL:    base,
		http:       &http.Client{Transport: transport, Timeout: timeout},
		maxRetries: retries,
		minGap:     opt.MinSendInterval,
	}, nil
}

// Call 调用任意 API 方法，返回 result 原始 JSON。
//
// 没有任何字段/文件的方法（比如 getMe）会发一个空 body 的 POST：
// 真实 Telegram 会拒绝"零部件 multipart"（400 + 空 body）。
func (c *Client) Call(ctx context.Context, method string, fields map[string]string, file *FilePart) (json.RawMessage, error) {
	var body *multipartBody
	if len(fields) > 0 || file != nil {
		var err error
		body, err = newMultipartBody(fields, file)
		if err != nil {
			return nil, err
		}
	}
	url := fmt.Sprintf("%s/bot%s/%s", c.baseURL, c.token, method)

	var lastErr error
	for attempt := 1; attempt <= c.maxRetries; attempt++ {
		c.throttle(ctx)
		if body != nil {
			body.Reset()
		}
		result, err := c.once(ctx, url, method, body)
		if err == nil {
			return result, nil
		}
		var retryable *retryableError
		if !errors.As(err, &retryable) {
			return nil, err
		}
		lastErr = err
		if attempt == c.maxRetries {
			break
		}
		delay := retryable.delay
		if delay <= 0 {
			delay = time.Duration(1<<uint(attempt)) * time.Second
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, fmt.Errorf("%s 多次重试后仍失败：%w", method, lastErr)
}

func (c *Client) once(ctx context.Context, url, method string, body *multipartBody) (json.RawMessage, error) {
	var reader io.Reader
	if body != nil {
		reader = body
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = body.Len()
		req.Header.Set("Content-Type", body.ContentType())
	}
	req.Header.Set("User-Agent", "filebot/2.0")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &retryableError{err: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, &retryableError{err: err}
	}

	var payload struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter *float64 `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		preview := describeBody(raw)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			// 4xx 重试没有意义（比如请求体被拒），直接把状态码摊开给用户看
			return nil, &APIError{
				Method:      method,
				Code:        resp.StatusCode,
				Description: fmt.Sprintf("响应不是 JSON：%s", preview),
			}
		}
		return nil, &retryableError{
			err: fmt.Errorf("响应不是 JSON（HTTP %d）：%s", resp.StatusCode, preview),
		}
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests || payload.ErrorCode == http.StatusTooManyRequests:
		delay := 3 * time.Second
		if payload.Parameters.RetryAfter != nil {
			delay = time.Duration(*payload.Parameters.RetryAfter*float64(time.Second)) + 500*time.Millisecond
		}
		return nil, &retryableError{
			err:   fmt.Errorf("触发限流：%s", payload.Description),
			delay: delay,
		}
	case resp.StatusCode >= 500:
		return nil, &retryableError{err: fmt.Errorf("服务端错误 HTTP %d：%s", resp.StatusCode, payload.Description)}
	case !payload.OK:
		return nil, &APIError{Method: method, Code: payload.ErrorCode, Description: payload.Description}
	}
	return payload.Result, nil
}

func (c *Client) throttle(ctx context.Context) {
	if c.minGap <= 0 {
		return
	}
	c.mu.Lock()
	wait := time.Until(c.lastSend.Add(c.minGap))
	if wait < 0 {
		wait = 0
	}
	c.lastSend = time.Now().Add(wait)
	c.mu.Unlock()
	if wait > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
	}
}

// GetMe 用于自检。
func (c *Client) GetMe(ctx context.Context) (map[string]any, error) {
	raw, err := c.Call(ctx, "getMe", nil, nil)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SendMessage 发送纯文本。
func (c *Client) SendMessage(ctx context.Context, chatID, text string) error {
	if len([]rune(text)) > 4096 {
		text = string([]rune(text)[:4096])
	}
	_, err := c.Call(ctx, "sendMessage", map[string]string{"chat_id": chatID, "text": text}, nil)
	return err
}

// SendDocument 发送原文件（不压缩）。
func (c *Client) SendDocument(ctx context.Context, chatID, path, caption string) error {
	return c.upload(ctx, "sendDocument", "document", chatID, path, caption, nil)
}

// SendPhoto 发送图片（可点开查看）。
func (c *Client) SendPhoto(ctx context.Context, chatID, path, caption string) error {
	return c.upload(ctx, "sendPhoto", "photo", chatID, path, caption, nil)
}

// SendAnimation 发送动图。
func (c *Client) SendAnimation(ctx context.Context, chatID, path, caption string) error {
	return c.upload(ctx, "sendAnimation", "animation", chatID, path, caption, nil)
}

// SendVideo 发送视频（开启流式播放）。
func (c *Client) SendVideo(ctx context.Context, chatID, path, caption string) error {
	return c.upload(ctx, "sendVideo", "video", chatID, path, caption, map[string]string{
		"supports_streaming": "true",
	})
}

func (c *Client) upload(ctx context.Context, method, field, chatID, path, caption string, extra map[string]string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s 是目录", path)
	}
	filename := filepath.Base(path)
	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	fields := map[string]string{"chat_id": chatID}
	if caption != "" {
		fields["caption"] = truncateRunes(caption, CaptionLimit)
	}
	for key, value := range extra {
		fields[key] = value
	}
	_, err = c.Call(ctx, method, fields, &FilePart{
		Field: field, Path: path, Filename: filename, ContentType: contentType,
	})
	return err
}

// ------------------------------------------------------------------ //
// multipart 请求体
// ------------------------------------------------------------------ //

type segment struct {
	data []byte
	file string
}

type multipartBody struct {
	boundary string
	segments []segment
	length   int64

	index  int
	offset int
	handle *os.File
}

func newMultipartBody(fields map[string]string, file *FilePart) (*multipartBody, error) {
	// boundary 用时间戳 + 进程号，避免与文件内容撞车
	body := &multipartBody{
		boundary: fmt.Sprintf("filebot%x%x", time.Now().UnixNano(), os.Getpid()),
	}
	add := func(data []byte) {
		body.segments = append(body.segments, segment{data: data})
		body.length += int64(len(data))
	}
	// 固定几个常用字段的顺序，便于人工排查；其余按字典序
	preferred := []string{"chat_id", "caption", "supports_streaming"}
	keys := make([]string, 0, len(fields))
	for _, key := range preferred {
		if _, ok := fields[key]; ok {
			keys = append(keys, key)
		}
	}
	rest := make([]string, 0, len(fields))
	for key := range fields {
		known := false
		for _, item := range preferred {
			if item == key {
				known = true
				break
			}
		}
		if !known {
			rest = append(rest, key)
		}
	}
	sort.Strings(rest)
	keys = append(keys, rest...)
	for _, key := range keys {
		add([]byte("--" + body.boundary + "\r\n"))
		add([]byte(fmt.Sprintf("Content-Disposition: form-data; name=%q\r\n\r\n", key)))
		add([]byte(fields[key]))
		add([]byte("\r\n"))
	}
	if file != nil {
		info, err := os.Stat(file.Path)
		if err != nil {
			return nil, err
		}
		add([]byte("--" + body.boundary + "\r\n"))
		add([]byte(fmt.Sprintf(
			"Content-Disposition: form-data; name=%q; filename=%q\r\n",
			file.Field, escapeFilename(file.Filename),
		)))
		add([]byte("Content-Type: " + file.ContentType + "\r\n\r\n"))
		body.segments = append(body.segments, segment{file: file.Path})
		body.length += info.Size()
		add([]byte("\r\n"))
	}
	add([]byte("--" + body.boundary + "--\r\n"))
	return body, nil
}

func (b *multipartBody) Len() int64 { return b.length }

// ContentType 返回 multipart 的 Content-Type。
func (b *multipartBody) ContentType() string {
	return "multipart/form-data; boundary=" + b.boundary
}

// Reset 让请求体可以重放（重试时用）。
func (b *multipartBody) Reset() {
	if b.handle != nil {
		b.handle.Close()
		b.handle = nil
	}
	b.index = 0
	b.offset = 0
}

func (b *multipartBody) Read(p []byte) (int, error) {
	total := 0
	for total < len(p) {
		if b.index >= len(b.segments) {
			if total > 0 {
				return total, nil
			}
			return 0, io.EOF
		}
		seg := b.segments[b.index]
		if seg.file == "" {
			if b.offset >= len(seg.data) {
				b.index++
				b.offset = 0
				continue
			}
			n := copy(p[total:], seg.data[b.offset:])
			b.offset += n
			total += n
			continue
		}
		if b.handle == nil {
			handle, err := os.Open(seg.file)
			if err != nil {
				if total > 0 {
					return total, nil
				}
				return 0, err
			}
			b.handle = handle
		}
		n, err := b.handle.Read(p[total:])
		total += n
		switch {
		case err == io.EOF:
			b.closeHandle()
			b.index++
			b.offset = 0
		case err != nil:
			return total, err
		case n == 0:
			// 理论上不会发生；避免死循环，直接跳到下一段
			b.closeHandle()
			b.index++
			b.offset = 0
		}
	}
	return total, nil
}

func (b *multipartBody) closeHandle() {
	if b.handle != nil {
		b.handle.Close()
		b.handle = nil
	}
}

// Close 释放可能打开的文件句柄。
func (b *multipartBody) Close() error {
	if b.handle != nil {
		err := b.handle.Close()
		b.handle = nil
		return err
	}
	return nil
}

func escapeFilename(name string) string {
	var buf bytes.Buffer
	for _, r := range name {
		switch {
		case r == '"':
			buf.WriteString("%22")
		case r == '\\':
			buf.WriteString("%5C")
		case r < 0x20 || r == 0x7f:
			// 丢弃控制字符
		default:
			buf.WriteRune(r)
		}
	}
	return buf.String()
}

func truncate(data []byte, limit int) string {
	if len(data) <= limit {
		return string(data)
	}
	return string(data[:limit]) + "…"
}

// describeBody 把响应体变成一句能看懂的说明（空 body 太常见了，直接说明白）。
func describeBody(raw []byte) string {
	if len(raw) == 0 {
		return "（空响应体）"
	}
	return fmt.Sprintf("%q", truncate(raw, 200))
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

// IsRetryable 供调用方判断错误性质（测试与日志用）。
func IsRetryable(err error) bool {
	var retryable *retryableError
	return errors.As(err, &retryable)
}
