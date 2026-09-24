package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filebot/internal/config"
	"filebot/internal/testsupport"
	"filebot/internal/watch"
)

type env struct {
	t       *testing.T
	fake    *testsupport.FakeTelegram
	media   string
	state   string
	created []string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	fake := testsupport.NewFakeTelegram()
	t.Cleanup(fake.Close)
	root := t.TempDir()
	mediaDir := filepath.Join(root, "media")
	if err := os.MkdirAll(filepath.Join(mediaDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &env{
		t:     t,
		fake:  fake,
		media: mediaDir,
		state: filepath.Join(root, "state.json"),
	}
}

func (e *env) write(relative string, size int) string {
	e.t.Helper()
	path := filepath.Join(e.media, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		e.t.Fatal(err)
	}
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i % 253)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		e.t.Fatal(err)
	}
	return path
}

func (e *env) config(mutate func(*config.Config)) config.Config {
	cfg := config.Default()
	cfg.Telegram.Token = "test:token"
	cfg.Telegram.ChatIDs = []string{"-100999"}
	cfg.Telegram.APIBase = e.fake.URL()
	cfg.Watch.Dirs = []string{e.media}
	cfg.Watch.StateFile = e.state
	cfg.Watch.MinSize = 0
	if mutate != nil {
		mutate(&cfg)
	}
	return cfg
}

func (e *env) runOnce(cfg config.Config) {
	e.t.Helper()
	runtime, err := New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if err != nil {
		e.t.Fatal(err)
	}
	defer runtime.Close()
	if err := runtime.RunOnce(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

func TestRunOnceSendsOriginalAndInline(t *testing.T) {
	e := newEnv(t)
	jpeg := e.write("pic.jpg", 2048)
	e.write("sub/clip.mp4", 4096)
	expected, _ := os.ReadFile(jpeg)

	e.runOnce(e.config(nil))

	methods := e.fake.Methods()
	if count(methods, "sendDocument") != 2 {
		t.Fatalf("sendDocument 次数 = %d（%v）", count(methods, "sendDocument"), methods)
	}
	if count(methods, "sendPhoto") != 1 || count(methods, "sendVideo") != 1 {
		t.Fatalf("内联消息不对：%v", methods)
	}

	docs := e.fake.CallsOf("sendDocument")
	var photoCall testsupport.Call
	for _, call := range docs {
		if call.Files["document"].Filename == "pic.jpg" {
			if string(call.Files["document"].Data) != string(expected) {
				t.Fatal("原文件字节被改动")
			}
			if call.Fields["chat_id"] != "-100999" {
				t.Fatalf("chat_id = %q", call.Fields["chat_id"])
			}
			if !strings.Contains(call.Fields["caption"], "pic.jpg") {
				t.Fatalf("caption = %q", call.Fields["caption"])
			}
		}
	}
	photoCall = e.fake.CallsOf("sendPhoto")[0]
	if photoCall.Files["photo"].Filename != "pic.jpg" {
		t.Fatalf("photo = %+v", photoCall.Files)
	}
	// 已经发过原文件，内联消息不再重复贴 caption
	if _, ok := photoCall.Fields["caption"]; ok {
		t.Fatalf("内联消息不应重复 caption：%v", photoCall.Fields)
	}
	video := e.fake.CallsOf("sendVideo")[0]
	if video.Fields["supports_streaming"] != "true" || video.Files["video"].Filename != "clip.mp4" {
		t.Fatalf("video = %+v", video)
	}
}

func TestRunOnceIgnoresNonMedia(t *testing.T) {
	e := newEnv(t)
	e.write("notes.txt", 2048)
	e.runOnce(e.config(nil))
	if len(e.fake.Calls()) != 0 {
		t.Fatalf("不该发送：%v", e.fake.Methods())
	}
}

func TestStateFilePreventsResending(t *testing.T) {
	e := newEnv(t)
	e.write("pic.jpg", 2048)
	cfg := e.config(nil)
	e.runOnce(cfg)
	first := len(e.fake.Calls())
	if first == 0 {
		t.Fatal("第一次应当发送")
	}
	e.runOnce(cfg)
	if len(e.fake.Calls()) != first {
		t.Fatalf("第二次不应再发：%d → %d", first, len(e.fake.Calls()))
	}
	if _, err := os.Stat(e.state); err != nil {
		t.Fatalf("状态文件应当存在：%v", err)
	}
}

func TestModifiedFileIsResent(t *testing.T) {
	e := newEnv(t)
	path := e.write("pic.jpg", 2048)
	cfg := e.config(nil)
	e.runOnce(cfg)
	before := len(e.fake.Calls())

	if err := os.WriteFile(path, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	e.runOnce(cfg)
	if len(e.fake.Calls()) <= before {
		t.Fatal("文件变化后应当重新发送")
	}
}

func TestOversizedOriginalIsSkippedButInlineSent(t *testing.T) {
	e := newEnv(t)
	e.write("big.jpg", 4096)
	e.runOnce(e.config(func(c *config.Config) { c.Upload.MaxUploadMB = 0.001 }))
	if count(e.fake.Methods(), "sendDocument") != 0 {
		t.Fatal("超限的原文件不该发送")
	}
	if count(e.fake.Methods(), "sendPhoto") != 1 {
		t.Fatal("内联版本仍应发送")
	}
}

func TestNotifySkipped(t *testing.T) {
	e := newEnv(t)
	e.write("big.jpg", 4096)
	e.runOnce(e.config(func(c *config.Config) {
		c.Upload.MaxUploadMB = 0.001
		c.Upload.NotifySkipped = true
	}))
	if count(e.fake.Methods(), "sendMessage") != 1 {
		t.Fatalf("应当发一条提醒：%v", e.fake.Methods())
	}
}

func TestNoCompressedFlag(t *testing.T) {
	e := newEnv(t)
	e.write("pic.jpg", 2048)
	e.runOnce(e.config(func(c *config.Config) { c.Upload.SendCompressed = false }))
	if got := e.fake.Methods(); len(got) != 1 || got[0] != "sendDocument" {
		t.Fatalf("methods = %v", got)
	}
}

func TestNoOriginalFlagPutsCaptionOnInline(t *testing.T) {
	e := newEnv(t)
	e.write("pic.jpg", 2048)
	e.runOnce(e.config(func(c *config.Config) { c.Upload.SendOriginal = false }))
	calls := e.fake.CallsOf("sendPhoto")
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	if !strings.Contains(calls[0].Fields["caption"], "pic.jpg") {
		t.Fatalf("caption = %q", calls[0].Fields["caption"])
	}
}

func TestTelegramErrorDoesNotStopTheRun(t *testing.T) {
	e := newEnv(t)
	e.write("a.jpg", 2048)
	e.write("b.jpg", 2048)
	e.fake.ScriptMethod("sendDocument", 400, map[string]any{
		"ok": false, "error_code": 400, "description": "chat not found",
	})
	e.runOnce(e.config(nil))
	// 第一个文件的原文件失败，但第二个文件照样要发
	if count(e.fake.Methods(), "sendDocument") < 2 {
		t.Fatalf("methods = %v", e.fake.Methods())
	}
	if count(e.fake.Methods(), "sendPhoto") < 1 {
		t.Fatalf("失败不该影响后续：%v", e.fake.Methods())
	}
}

func TestRunSendsNewFilesAndStops(t *testing.T) {
	e := newEnv(t)
	// 启动前就存在的文件：常驻模式不管它（即使配了 scan_existing）
	old := e.write("old.jpg", 2048)

	cfg := e.config(func(c *config.Config) {
		c.Watch.PollInterval = 0.05
		c.Watch.SettleSeconds = 0.05
		c.Watch.ScanExisting = true
		c.Telegram.MinSendInterval = 0
		c.Workers = 1
	})
	runtime, err := New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()

	// 等 Run 自检完再等几拍，确保"启动瞬间"的快照已经拍过，之后写入的才算新增
	waitFor(t, 15*time.Second, func() bool {
		return len(e.fake.CallsOf("getMe")) > 0
	}, "Run 没有完成自检")
	time.Sleep(300 * time.Millisecond)

	e.write("new.jpg", 3000)
	waitFor(t, 15*time.Second, func() bool {
		return count(e.fake.Methods(), "sendPhoto") >= 1
	}, "运行中新增的图片没有被发送")

	// 已有的旧文件不能被牵扯进来
	for _, call := range e.fake.Calls() {
		for _, part := range call.Files {
			if part.Filename == "old.jpg" {
				t.Fatalf("启动前就存在的文件不该被发送：%+v", call.Fields)
			}
		}
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("旧文件不该被动过：%v", err)
	}

	// 运行中出现的视频要发送（拷贝大文件的场景：先出现、长完再发）
	e.write("clip.mp4", 4096)
	waitFor(t, 15*time.Second, func() bool {
		return count(e.fake.Methods(), "sendVideo") >= 1
	}, "运行中出现的视频没有被发送")

	// 运行中已被发过的文件又被改写 → 算新版本，重新发送
	if err := os.WriteFile(filepath.Join(e.media, "clip.mp4"), make([]byte, 8192), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 15*time.Second, func() bool {
		return count(e.fake.Methods(), "sendVideo") >= 2
	}, "运行中被改写的文件没有被重新发送")

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run 返回错误：%v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run 没有在 ctx 取消后退出")
	}
	if _, err := os.Stat(e.state); err != nil {
		t.Fatalf("状态文件应当存在：%v", err)
	}
}

func TestMultipleChatIDsAllReceiveEveryFile(t *testing.T) {
	e := newEnv(t)
	path := e.write("pic.jpg", 2048)
	expected, _ := os.ReadFile(path)

	e.runOnce(e.config(func(c *config.Config) {
		c.Telegram.ChatIDs = []string{"-100999", "@channel_b", "-42"}
	}))

	for _, chat := range []string{"-100999", "@channel_b", "-42"} {
		docs := e.fake.CallsOf("sendDocument")
		found := false
		for _, call := range docs {
			if call.Fields["chat_id"] == chat && call.Files["document"].Filename == "pic.jpg" {
				found = true
				if string(call.Files["document"].Data) != string(expected) {
					t.Fatalf("%s 收到的原文件字节不对", chat)
				}
			}
		}
		if !found {
			t.Errorf("%s 没收到原文件", chat)
		}
		photos := 0
		for _, call := range e.fake.CallsOf("sendPhoto") {
			if call.Fields["chat_id"] == chat {
				photos++
			}
		}
		if photos != 1 {
			t.Errorf("%s 收到 %d 条预览，期望 1 条", chat, photos)
		}
	}
	if len(e.fake.CallsOf("sendDocument")) != 3 || len(e.fake.CallsOf("sendPhoto")) != 3 {
		t.Fatalf("methods = %v", e.fake.Methods())
	}
}

// 同一个接收方内部，原文件必须排在可点开的版本前面
func TestOriginalIsSentBeforeInlinePerChat(t *testing.T) {
	e := newEnv(t)
	e.write("pic.jpg", 2048)
	e.runOnce(e.config(func(c *config.Config) {
		c.Telegram.ChatIDs = []string{"@a", "@b"}
	}))
	methods := e.fake.Methods()
	want := []string{"sendDocument", "sendPhoto", "sendDocument", "sendPhoto"}
	if len(methods) != len(want) {
		t.Fatalf("methods = %v", methods)
	}
	for i := range want {
		if methods[i] != want[i] {
			t.Fatalf("methods = %v，期望 %v", methods, want)
		}
	}
}

// 某个接收方失败不影响其它接收方，而且重试只重发失败的
func TestOneBadChatDoesNotBlockOthers(t *testing.T) {
	e := newEnv(t)
	e.write("pic.jpg", 2048)
	e.fake.ScriptMethod("sendDocument", 400, map[string]any{
		"ok": false, "error_code": 400, "description": "Bad Request: chat not found",
	})

	e.runOnce(e.config(func(c *config.Config) {
		c.Telegram.ChatIDs = []string{"@broken", "@ok"}
	}))

	// 第二个接收方照样拿到原文件和预览
	var docs, photos int
	for _, call := range e.fake.CallsOf("sendDocument") {
		if call.Fields["chat_id"] == "@ok" {
			docs++
		}
	}
	for _, call := range e.fake.CallsOf("sendPhoto") {
		if call.Fields["chat_id"] == "@ok" {
			photos++
		}
	}
	if docs != 1 || photos != 1 {
		t.Fatalf("@ok 应当完整收到（docs=%d photos=%d）", docs, photos)
	}
	// 失败的接收方不再重试（400 是永久错误）
	broken := 0
	for _, call := range e.fake.CallsOf("sendDocument") {
		if call.Fields["chat_id"] == "@broken" {
			broken++
		}
	}
	if broken != 1 {
		t.Fatalf("@broken 应该只尝试一次，实际 %d 次", broken)
	}
	// 但预览消息仍然发给它（每条消息独立失败）
	brokenPhotos := 0
	for _, call := range e.fake.CallsOf("sendPhoto") {
		if call.Fields["chat_id"] == "@broken" {
			brokenPhotos++
		}
	}
	if brokenPhotos != 1 {
		t.Fatalf("@broken 的预览应当照发，实际 %d 条", brokenPhotos)
	}
}

func TestRunWithoutToken(t *testing.T) {
	e := newEnv(t)
	cfg := e.config(func(c *config.Config) { c.Telegram.Token = "" })
	if _, err := New(cfg, nil); err == nil {
		t.Fatal("初始化应当报错")
	}
}

func TestHumanSize(t *testing.T) {
	cases := map[int64]string{512: "512 B", 2048: "2.0 KB", 5 << 20: "5.0 MB", 3 << 30: "3.0 GB"}
	for size, want := range cases {
		if got := HumanSize(size); got != want {
			t.Errorf("HumanSize(%d) = %q，期望 %q", size, got, want)
		}
	}
}

func TestRenderCaption(t *testing.T) {
	if got := RenderCaption("{name}|{size}|{kind}", "/data/a.mp4", 2048, "video"); got != "a.mp4|2.0 KB|video" {
		t.Errorf("got = %q", got)
	}
	if got := RenderCaption("", "/data/a.mp4", 1, "video"); got != "a.mp4" {
		t.Errorf("空模板应当退化成文件名，got = %q", got)
	}
	if got := RenderCaption("{unknown}", "/data/a.jpg", 1, "image"); got != "{unknown}" {
		t.Errorf("未知占位符保持原样，got = %q", got)
	}
}

func TestWatchStatHelpers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.jpg")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime, size, ok := watch.Stat(path)
	if !ok || size != 1 || mtime == 0 {
		t.Fatalf("stat = %d %d %v", mtime, size, ok)
	}
}

func count(items []string, want string) int {
	total := 0
	for _, item := range items {
		if item == want {
			total++
		}
	}
	return total
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal(message)
}

// 启动自检（getMe）期间落进目录的文件也属于"运行中新增"，必须发送。
// 回归：基准快照曾经在自检之后才拍，这段窗口里的文件会被当成"启动前就存在"而永远丢失。
func TestFilesArrivingDuringStartupCheckAreSent(t *testing.T) {
	e := newEnv(t)
	e.fake.DelayMethod("getMe", 1500*time.Millisecond)

	cfg := e.config(func(c *config.Config) {
		c.Watch.PollInterval = 0.05
		c.Watch.SettleSeconds = 0.05
		c.Telegram.MinSendInterval = 0
	})
	runtime, err := New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
		}
	}()

	// 自检还没返回时就把文件放进去（这正是之前会被丢掉的那类文件）
	time.Sleep(200 * time.Millisecond)
	e.write("during-check.jpg", 3000)

	waitFor(t, 20*time.Second, func() bool {
		return count(e.fake.Methods(), "sendPhoto") >= 1
	}, "启动自检期间新增的文件被丢掉了")
}

// 退出时不能把"刚落地、还在去抖窗口里"的文件丢掉
func TestShutdownFlushesPendingFiles(t *testing.T) {
	e := newEnv(t)
	cfg := e.config(func(c *config.Config) {
		c.Watch.PollInterval = 0.05
		c.Watch.SettleSeconds = 1.0 // 故意让去抖窗口比写入时刻长
		c.Telegram.MinSendInterval = 0
	})
	runtime, err := New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()

	waitFor(t, 15*time.Second, func() bool {
		return len(e.fake.CallsOf("getMe")) > 0
	}, "Run 没有完成自检")
	time.Sleep(200 * time.Millisecond) // 等基准快照拍完

	e.write("late.jpg", 2048)
	time.Sleep(200 * time.Millisecond) // 远小于 settle，文件仍在去抖窗口里
	cancel()                           // 此刻退出

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run 返回错误：%v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run 没有退出")
	}
	if count(e.fake.Methods(), "sendDocument") == 0 {
		t.Fatal("退出时去抖窗口里的文件被静默丢掉了")
	}
}

// 一条消息都没发出去时绝不能记成"已发送"，否则这个文件以后永远不会再尝试
func TestNothingDeliveredIsNotMarkedAsSent(t *testing.T) {
	e := newEnv(t)
	// 关掉原文件 + 用一个无法转换的图片（内容是垃圾字节，ffmpeg/imagemagick 都解不开）
	path := e.write("broken.bmp", 2048)
	if err := os.WriteFile(path, []byte("not a real image at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := e.config(func(c *config.Config) { c.Upload.SendOriginal = false })

	runtime, err := New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	err = runtime.RunOnce(context.Background())
	if err == nil {
		t.Fatal("一个文件都没发出去，RunOnce 应当返回错误（让脚本/运维能发现）")
	}
	if len(e.fake.Calls()) != 0 {
		t.Fatalf("不该有任何调用：%v", e.fake.Methods())
	}
	if raw, readErr := os.ReadFile(e.state); readErr == nil && strings.Contains(string(raw), "broken.bmp") {
		t.Fatalf("没发出去的文件不该被记进状态文件：%s", raw)
	}
}
