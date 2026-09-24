package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `
workers = 2
log_level = "debug"

[telegram]
token = "123:ABC"
chat_id = "@mychannel"
api_base = "https://api.telegram.org"
min_send_interval = 2.5
timeout = 30

[proxy]
url = "socks5://127.0.0.1:1080"

[watch]
dirs = ["/data/a", "/data/b"]
poll_interval = 5
settle_seconds = 1.5
min_size = 4096
exclude = ["*.skip"]
scan_existing = true

[upload]
send_original = true
send_compressed = false
max_upload_mb = 2000
caption_template = "{name} ({size})"
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, item := range os.Environ() {
		key := strings.SplitN(item, "=", 2)[0]
		if strings.HasPrefix(key, "FILEBOT_") {
			t.Setenv(key, "")
			os.Unsetenv(key)
		}
	}
}

func TestLoadFullConfig(t *testing.T) {
	clearEnv(t)
	cfg, err := Load(writeConfig(t, sample), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telegram.Token != "123:ABC" {
		t.Errorf("token = %q", cfg.Telegram.Token)
	}
	if len(cfg.Telegram.ChatIDs) != 1 || cfg.Telegram.ChatIDs[0] != "@mychannel" {
		t.Errorf("chat_id = %v", cfg.Telegram.ChatIDs)
	}
	if cfg.Telegram.MinSendInterval != 2.5 {
		t.Errorf("min_send_interval = %v", cfg.Telegram.MinSendInterval)
	}
	if cfg.Proxy.URL != "socks5://127.0.0.1:1080" {
		t.Errorf("proxy = %q", cfg.Proxy.URL)
	}
	if len(cfg.Watch.Dirs) != 2 || cfg.Watch.Dirs[0] != "/data/a" {
		t.Errorf("dirs = %v", cfg.Watch.Dirs)
	}
	if cfg.Watch.PollInterval != 5 || cfg.Watch.SettleSeconds != 1.5 || cfg.Watch.MinSize != 4096 {
		t.Errorf("watch = %+v", cfg.Watch)
	}
	if !cfg.Watch.ScanExisting || len(cfg.Watch.Exclude) != 1 {
		t.Errorf("watch = %+v", cfg.Watch)
	}
	if cfg.Upload.SendCompressed || cfg.Upload.MaxUploadMB != 2000 {
		t.Errorf("upload = %+v", cfg.Upload)
	}
	if cfg.MaxUploadBytes() != 2000*1024*1024 {
		t.Errorf("maxUploadBytes = %d", cfg.MaxUploadBytes())
	}
	if cfg.Workers != 2 || cfg.LogLevel != "DEBUG" {
		t.Errorf("workers=%d level=%q", cfg.Workers, cfg.LogLevel)
	}
}

func TestUnknownKeyIsRejected(t *testing.T) {
	clearEnv(t)
	_, err := Load(writeConfig(t, "[telegram]\ntoken = \"x\"\nnope = 1\n"), Overrides{})
	if err == nil || !strings.Contains(err.Error(), "无法识别的配置项") {
		t.Fatalf("err = %v", err)
	}
}

// workers/log_level 是顶层键；写在 [upload] 段里必须报错而不是被忽略。
func TestTopLevelKeysMustNotBeNested(t *testing.T) {
	clearEnv(t)
	_, err := Load(writeConfig(t, "[upload]\nworkers = 2\n"), Overrides{})
	if err == nil || !strings.Contains(err.Error(), "upload.workers") {
		t.Fatalf("err = %v", err)
	}
}

func TestMissingFile(t *testing.T) {
	if _, err := Load("/definitely/missing.toml", Overrides{}); err == nil {
		t.Fatal("应当报错")
	}
}

func TestBrokenToml(t *testing.T) {
	clearEnv(t)
	if _, err := Load(writeConfig(t, "[telegram\ntoken=\n"), Overrides{}); err == nil {
		t.Fatal("应当报错")
	}
}

func TestValidateReportsProblems(t *testing.T) {
	clearEnv(t)
	cfg, err := Load("", Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	err = cfg.Validate()
	if err == nil {
		t.Fatal("应当报错")
	}
	for _, want := range []string{"telegram.token", "telegram.chat_id", "watch.dirs"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息里缺少 %s：%v", want, err)
		}
	}
}

func TestEnvOverridesFile(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, sample)
	t.Setenv("FILEBOT_TG_TOKEN", "env:TOKEN")
	t.Setenv("FILEBOT_PROXY", "http://127.0.0.1:3128")
	t.Setenv("FILEBOT_WATCH_DIRS", "/env/one:/env/two")
	cfg, err := Load(path, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telegram.Token != "env:TOKEN" {
		t.Errorf("token = %q", cfg.Telegram.Token)
	}
	if cfg.Proxy.URL != "http://127.0.0.1:3128" {
		t.Errorf("proxy = %q", cfg.Proxy.URL)
	}
	if len(cfg.Watch.Dirs) != 2 || cfg.Watch.Dirs[1] != "/env/two" {
		t.Errorf("dirs = %v", cfg.Watch.Dirs)
	}
}

func TestCLIOverridesEnvAndFile(t *testing.T) {
	clearEnv(t)
	t.Setenv("FILEBOT_TG_TOKEN", "env:TOKEN")
	token, dirs, level := "cli:TOKEN", []string{"/cli"}, "warn"
	cfg, err := Load(writeConfig(t, sample), Overrides{
		Token:    &token,
		ChatIDs:  []string{"-42", "@second"},
		Dirs:     dirs,
		LogLevel: &level,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telegram.Token != "cli:TOKEN" {
		t.Errorf("token=%q", cfg.Telegram.Token)
	}
	if len(cfg.Telegram.ChatIDs) != 2 || cfg.Telegram.ChatIDs[0] != "-42" || cfg.Telegram.ChatIDs[1] != "@second" {
		t.Errorf("chat_ids = %v", cfg.Telegram.ChatIDs)
	}
	if len(cfg.Watch.Dirs) != 1 || cfg.Watch.Dirs[0] != "/cli" {
		t.Errorf("dirs = %v", cfg.Watch.Dirs)
	}
	if cfg.LogLevel != "WARN" {
		t.Errorf("level = %q", cfg.LogLevel)
	}
}

func TestChatIDForms(t *testing.T) {
	clearEnv(t)
	cases := []struct {
		name string
		toml string
		want []string
	}{
		{"单个字符串", "[telegram]\nchat_id = \"@a\"\n", []string{"@a"}},
		{"不带引号的整数", "[telegram]\nchat_id = -1001234567890\n", []string{"-1001234567890"}},
		{"数组", "[telegram]\nchat_id = [\"@a\", \"-1001234567890\"]\n", []string{"@a", "-1001234567890"}},
		{"数组里混整数", "[telegram]\nchat_id = [\"@a\", -42]\n", []string{"@a", "-42"}},
		{"重复项去重", "[telegram]\nchat_id = [\"@a\", \"@a\", \"@b\"]\n", []string{"@a", "@b"}},
		{"单个字符串带空白的数组", "[telegram]\nchat_id = \" @a \"\n", []string{"@a"}},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, item.toml), Overrides{})
			if err != nil {
				t.Fatal(err)
			}
			got := []string(cfg.Telegram.ChatIDs)
			if len(got) != len(item.want) {
				t.Fatalf("chat_ids = %v，期望 %v", got, item.want)
			}
			for i := range item.want {
				if got[i] != item.want[i] {
					t.Fatalf("chat_ids = %v，期望 %v", got, item.want)
				}
			}
		})
	}
}

func TestChatIDEnvAcceptsMultiple(t *testing.T) {
	clearEnv(t)
	t.Setenv("FILEBOT_TG_CHAT_ID", "@a, -1001234567890,@a")
	cfg, err := Load("", Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Telegram.ChatIDs) != 2 ||
		cfg.Telegram.ChatIDs[0] != "@a" || cfg.Telegram.ChatIDs[1] != "-1001234567890" {
		t.Fatalf("chat_ids = %v", cfg.Telegram.ChatIDs)
	}
}

func TestDirsAcceptsSingleString(t *testing.T) {
	clearEnv(t)
	cfg, err := Load(writeConfig(t, "[watch]\ndirs = \"/data/media\"\n"), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Watch.Dirs) != 1 || cfg.Watch.Dirs[0] != "/data/media" {
		t.Fatalf("dirs = %v", cfg.Watch.Dirs)
	}
}

func TestDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := Load("", Overrides{Dirs: []string{"/tmp"}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Upload.MaxUploadMB != 50 || cfg.Upload.PhotoMaxMB != 10 || cfg.Upload.VideoMaxMB != 50 {
		t.Errorf("upload = %+v", cfg.Upload)
	}
	if cfg.Watch.PollInterval != 2 || cfg.Watch.SettleSeconds != 3 || cfg.Watch.MinSize != 1024 {
		t.Errorf("watch = %+v", cfg.Watch)
	}
	if !cfg.Upload.SendOriginal || !cfg.Upload.SendCompressed {
		t.Errorf("upload = %+v", cfg.Upload)
	}
	if cfg.Workers != 1 || cfg.Telegram.MaxRetries != 3 {
		t.Errorf("cfg = %+v", cfg)
	}
	if !strings.Contains(cfg.Upload.CaptionTemplate, "{name}") {
		t.Errorf("caption = %q", cfg.Upload.CaptionTemplate)
	}
}

func TestMissingDirs(t *testing.T) {
	clearEnv(t)
	cfg, _ := Load("", Overrides{Dirs: []string{"/no/such/dir"}})
	if got := cfg.MissingDirs(); len(got) != 1 {
		t.Errorf("missing = %v", got)
	}
	dir := t.TempDir()
	cfg, _ = Load("", Overrides{Dirs: []string{dir}})
	if got := cfg.MissingDirs(); len(got) != 0 {
		t.Errorf("missing = %v", got)
	}
}

func TestExpandPathTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := ExpandPath("~/media")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(home, "media") {
		t.Errorf("got %q", got)
	}
}

func TestRelativeDirsBecomeAbsolute(t *testing.T) {
	clearEnv(t)
	cfg, err := Load("", Overrides{Dirs: []string{"relative/dir"}})
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(cfg.Watch.Dirs[0]) {
		t.Errorf("dirs = %v", cfg.Watch.Dirs)
	}
}
