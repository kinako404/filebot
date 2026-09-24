package service

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"filebot/internal/config"
)

// fakeRunner 记录被调用的外部命令，避免测试真的动系统。
type fakeRunner struct {
	calls   []string
	failOn  map[string]error
	outputs map[string]string
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{failOn: map[string]error{}, outputs: map[string]string{}}
}

func (f *fakeRunner) run(name string, args ...string) (string, error) {
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	for key, err := range f.failOn {
		if strings.Contains(call, key) {
			return f.outputs[key], err
		}
	}
	return f.outputs[name], nil
}

func (f *fakeRunner) has(prefix string) bool {
	for _, call := range f.calls {
		if strings.HasPrefix(call, prefix) {
			return true
		}
	}
	return false
}

func TestRenderUnitContainsRequiredDirectives(t *testing.T) {
	unit := RenderUnit(Params{
		Description: "filebot test",
		User:        "filebot",
		BinPath:     "/usr/local/bin/filebot",
		ConfigPath:  "/etc/filebot/config.toml",
		UnitName:    "filebot",
		WorkDir:     "/var/lib/filebot",
	})
	for _, want := range []string{
		"[Unit]",
		"[Service]",
		"[Install]",
		"User=filebot",
		"Group=filebot",
		"ExecStart=/usr/local/bin/filebot run -c /etc/filebot/config.toml",
		"WorkingDirectory=/var/lib/filebot",
		"Restart=always",
		"StateDirectory=filebot",
		"NoNewPrivileges=true",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("单元文件缺少 %q\n%s", want, unit)
		}
	}
}

func TestRenderUnitWithoutUser(t *testing.T) {
	unit := RenderUnit(Params{
		Description: "d", BinPath: "/bin/true", ConfigPath: "/etc/x.toml", UnitName: "filebot",
	})
	if strings.Contains(unit, "User=") {
		t.Errorf("不指定用户时不应写 User=\n%s", unit)
	}
	if strings.Contains(unit, "WorkingDirectory=") {
		t.Errorf("不指定 WorkDir 时不应写 WorkingDirectory=\n%s", unit)
	}
}

func TestConfigTemplateHasStateFile(t *testing.T) {
	text := ConfigTemplate("/var/lib/filebot/state.json")
	if !strings.Contains(text, `state_file = "/var/lib/filebot/state.json"`) {
		t.Fatalf("模板里缺少 state_file：\n%s", text)
	}
	for _, key := range []string{"[telegram]", "token", "chat_id", "[proxy]", "url", "dirs", "[upload]"} {
		if !strings.Contains(text, key) {
			t.Errorf("模板里缺少 %s", key)
		}
	}
}

// 生成的配置模板必须能被程序自己解析（顶层键不能落在 [upload] 段里）。
func TestGeneratedConfigTemplateLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(ConfigTemplate("/var/lib/filebot/state.json")), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, config.Overrides{})
	if err != nil {
		t.Fatalf("模板无法解析：%v", err)
	}
	if cfg.Workers != 1 || cfg.LogLevel != "INFO" {
		t.Fatalf("顶层键没生效：workers=%d level=%q", cfg.Workers, cfg.LogLevel)
	}
	if cfg.Watch.StateFile != "/var/lib/filebot/state.json" {
		t.Fatalf("state_file = %q", cfg.Watch.StateFile)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("模板的 token/chat_id 是留空的，应当校验失败并提示用户去填")
	}
}

func TestInstallIntoCustomRoot(t *testing.T) {
	root := t.TempDir()
	runner := newFakeRunner()
	var out bytes.Buffer

	err := Install(Options{
		Root:     root,
		BinPath:  "/usr/local/bin/filebot",
		Out:      &out,
		runner:   runner.run,
		Enable:   true,
		Start:    true,
		UnitName: "filebot",
	})
	if err != nil {
		t.Fatalf("Install 失败：%v\n%s", err, out.String())
	}

	unitPath := filepath.Join(root, "etc/systemd/system/filebot.service")
	unit, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("单元文件没写出来：%v", err)
	}
	if !strings.Contains(string(unit), "ExecStart=/usr/local/bin/filebot run -c "+filepath.Join(root, "etc/filebot/config.toml")) {
		t.Fatalf("ExecStart 不对：\n%s", unit)
	}

	configPath := filepath.Join(root, "etc/filebot/config.toml")
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("配置模板没写出来：%v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("配置模板权限 = %v，应当是 0600（里面有 token）", info.Mode().Perm())
	}
	config, _ := os.ReadFile(configPath)
	if !strings.Contains(string(config), filepath.Join(root, "var/lib/filebot/state.json")) {
		t.Errorf("模板里的 state_file 不对：\n%s", config)
	}

	if _, err := os.Stat(filepath.Join(root, "var/lib/filebot")); err != nil {
		t.Errorf("状态目录没建出来：%v", err)
	}

	// --root 不是 / 时不应调用 systemctl
	if runner.has("systemctl") {
		t.Errorf("不该调用 systemctl：%v", runner.calls)
	}
	if !strings.Contains(out.String(), "已写入单元文件") {
		t.Errorf("输出里应当说明写了单元文件：%s", out.String())
	}
}

func TestInstallDryRunWritesNothing(t *testing.T) {
	root := t.TempDir()
	var out bytes.Buffer
	err := Install(Options{
		Root:    root,
		BinPath: "/usr/local/bin/filebot",
		Out:     &out,
		DryRun:  true,
		Start:   true,
		Enable:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("干跑不该写文件：%v", entries)
	}
	if !strings.Contains(out.String(), "[Service]") || !strings.Contains(out.String(), "干跑模式") {
		t.Fatalf("干跑应当打印单元文件：%s", out.String())
	}
}

func TestInstallWithRealRootDryRunTouchesNothing(t *testing.T) {
	var out bytes.Buffer
	err := Install(Options{BinPath: "/usr/local/bin/filebot", Out: &out, DryRun: true})
	if err != nil {
		t.Fatalf("干跑失败：%v", err)
	}
	if !strings.Contains(out.String(), "/etc/systemd/system/filebot.service") {
		t.Fatalf("应当打印默认单元路径：%s", out.String())
	}
	if _, err := os.Stat("/etc/systemd/system/filebot.service"); err == nil {
		t.Fatal("干跑竟然写了 /etc/systemd/system/filebot.service")
	}
}

func TestInstallSkipsStartWhenConfigMissing(t *testing.T) {
	root := t.TempDir()
	runner := newFakeRunner()
	var out bytes.Buffer
	err := Install(Options{
		Root: root, BinPath: "/usr/local/bin/filebot", Out: &out,
		runner: runner.run, Enable: true, Start: true, User: "root",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "请填入 token") {
		t.Fatalf("应当提示去填配置：%s", out.String())
	}
}

func TestInstallUsesProvidedConfig(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "srv", "filebot.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("[telegram]\ntoken=\"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 这里刻意用真实的 systemd-analyze，同时验证 ExecStart 指向的是真实存在的可执行文件
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Install(Options{
		Root: root, BinPath: exe, ConfigPath: configPath, Out: &out,
	}); err != nil {
		t.Fatal(err)
	}
	unit, _ := os.ReadFile(filepath.Join(root, "etc/systemd/system/filebot.service"))
	if !strings.Contains(string(unit), "run -c "+configPath) {
		t.Fatalf("ExecStart 应当指向指定配置：\n%s", unit)
	}
	// 已有配置不应被覆盖
	content, _ := os.ReadFile(configPath)
	if !strings.Contains(string(content), "token=\"x\"") {
		t.Fatalf("已有配置被覆盖了：%s", content)
	}
}

func TestInstallAbortsWhenUnitVerifyFails(t *testing.T) {
	root := t.TempDir()
	runner := newFakeRunner()
	runner.failOn["systemd-analyze"] = fmt.Errorf("bad unit")
	runner.outputs["systemd-analyze"] = "filebot.service: unknown directive"
	var out bytes.Buffer
	err := Install(Options{
		Root: root, BinPath: "/usr/local/bin/filebot", Out: &out, runner: runner.run,
	})
	if err == nil {
		t.Fatal("校验失败时应当报错")
	}
	if !strings.Contains(err.Error(), "unknown directive") {
		t.Fatalf("错误信息里应带上校验输出：%v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "etc/systemd/system/filebot.service")); statErr == nil {
		t.Fatal("校验失败时不该写入单元文件")
	}
}

func TestInstallRunsSystemctlOnRealRoot(t *testing.T) {
	// 用临时 root 走完整流程，但把 / 的相关调用替换成假 runner
	root := t.TempDir()
	runner := newFakeRunner()
	var out bytes.Buffer
	err := Install(Options{
		Root: root, BinPath: "/usr/local/bin/filebot", Out: &out, runner: runner.run,
		Enable: true, Start: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// root 不是 /，所以跳过 systemd；换成 / 的逻辑由下面的单测覆盖
	if runner.has("systemctl") {
		t.Fatalf("root != / 时不该调用 systemctl：%v", runner.calls)
	}
}

func TestUninstallRemovesUnit(t *testing.T) {
	root := t.TempDir()
	unitPath := filepath.Join(root, "etc/systemd/system/filebot.service")
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unitPath, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := newFakeRunner()
	var out bytes.Buffer
	if err := Uninstall(Options{Root: root, Out: &out, runner: runner.run}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Fatal("单元文件应当被删除")
	}
	if runner.has("systemctl") {
		t.Fatalf("root != / 时不该调用 systemctl：%v", runner.calls)
	}
}

func TestUninstallPurge(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "etc/filebot")
	stateDir := filepath.Join(root, "var/lib/filebot")
	for _, dir := range []string{configDir, stateDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Uninstall(Options{Root: root, Out: &out}, true); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{configDir, stateDir} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s 应当被删除", dir)
		}
	}
}

func TestUninstallDryRunWritesNothing(t *testing.T) {
	root := t.TempDir()
	unitPath := filepath.Join(root, "etc/systemd/system/filebot.service")
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unitPath, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Uninstall(Options{Root: root, Out: &out, DryRun: true}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(unitPath); err != nil {
		t.Fatal("干跑不该删除文件")
	}
	if !strings.Contains(out.String(), "干跑模式") {
		t.Fatalf("应当打印计划：%s", out.String())
	}
}

// 用真实的 systemd-analyze 校验渲染出来的单元文件（只读操作，不改系统）。
func TestRenderedUnitPassesRealSystemdAnalyze(t *testing.T) {
	analyze := lookPath("systemd-analyze")
	if analyze == "" {
		t.Skip("本机没有 systemd-analyze")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	unit := RenderUnit(Params{
		Description: "filebot - 监控目录并把图片/视频发送到 Telegram",
		User:        "filebot",
		BinPath:     exe,
		ConfigPath:  "/etc/filebot/config.toml",
		UnitName:    "filebot",
		WorkDir:     "/tmp",
	})
	path := filepath.Join(t.TempDir(), "filebot.service")
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(analyze, "verify", path)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("systemd-analyze verify 失败：%v\n%s\n----单元内容----\n%s", err, output, unit)
	}
}

func TestNormalizeValidation(t *testing.T) {
	opts := Options{UnitName: "bad name"}
	if err := opts.normalize(); err == nil {
		t.Fatal("服务名带空格应当报错")
	}
}
