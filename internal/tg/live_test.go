package tg

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"filebot/internal/proxy"
)

// 打真实 Telegram API 的联调测试：用假 token，只验证"请求格式被接受"。
// 真实 API 对格式正确的请求会回 JSON 401；如果回 400 + 空 body，说明请求被拒。
//
//	FILEBOT_LIVE_TEST=1 go test ./internal/tg/ -run Live -v
//	FILEBOT_LIVE_TEST=1 FILEBOT_TEST_PROXY=socks5://127.0.0.1:1080 go test ./internal/tg/ -run Live -v
func TestLiveRequestFormatAccepted(t *testing.T) {
	if os.Getenv("FILEBOT_LIVE_TEST") == "" {
		t.Skip("设置 FILEBOT_LIVE_TEST=1 才打真实 API")
	}
	options := Options{
		Token:      "123456:FAKE-TOKEN-FOR-FORMAT-CHECK",
		APIBase:    "https://api.telegram.org",
		MaxRetries: 1,
		Timeout:    30 * time.Second,
	}
	if raw := os.Getenv("FILEBOT_TEST_PROXY"); raw != "" {
		transport, err := proxy.New(raw, 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		options.Transport = transport
	}
	client, err := New(options)
	if err != nil {
		t.Fatal(err)
	}

	tmp := t.TempDir()
	jpg := tmp + "/a.jpg"
	mp4 := tmp + "/a.mp4"
	if err := os.WriteFile(jpg, []byte("JPEGDATA"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mp4, []byte("MP4DATA"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cases := []struct {
		name string
		call func() error
	}{
		{"getMe", func() error { _, err := client.GetMe(ctx); return err }},
		{"sendMessage", func() error { return client.SendMessage(ctx, "1", "hi") }},
		{"sendDocument", func() error { return client.SendDocument(ctx, "1", jpg, "cap") }},
		{"sendPhoto", func() error { return client.SendPhoto(ctx, "1", jpg, "cap") }},
		{"sendVideo", func() error { return client.SendVideo(ctx, "1", mp4, "cap") }},
		{"sendAnimation", func() error { return client.SendAnimation(ctx, "1", jpg, "cap") }},
	}
	for _, item := range cases {
		err := item.call()
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Errorf("%s: 期望拿到 JSON 401（说明请求被接受），实际 err=%v", item.name, err)
			continue
		}
		if apiErr.Code != 401 {
			t.Errorf("%s: 期望 401，实际 %d %s", item.name, apiErr.Code, apiErr.Description)
			continue
		}
		t.Logf("%s: OK（真实 API 接受了请求，只是 token 是假的）", item.name)
	}
}
