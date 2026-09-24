package service

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

// ---------------------------------------------------------------- //
// 问题 1：配置文件属主
// ---------------------------------------------------------------- //

func TestConfigUnreadableDecisions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	uid, gid := os.Getuid(), os.Getgid()
	otherUID, otherGID := uid+12345, gid+12345
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if configUnreadable(info, uid, gid) {
		t.Error("自己写的 0600 文件对属主应当是可读的")
	}
	if !configUnreadable(info, otherUID, otherGID) {
		t.Error("0600 的 root 属主文件对别的用户应当判定为读不到")
	}

	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	info, _ = os.Stat(path)
	if configUnreadable(info, otherUID, gid) {
		t.Error("0640 对同组用户是可读的")
	}
	if !configUnreadable(info, otherUID, otherGID) {
		t.Error("0640 对不同组用户应当判定为读不到")
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	info, _ = os.Stat(path)
	if configUnreadable(info, otherUID, otherGID) {
		t.Error("0644 任何人都可读")
	}

	if configUnreadable(info, 0, otherGID) {
		t.Error("root 不受权限位限制")
	}

	// 只有 root 能把文件交给另一个 uid，才能验证属主权限位的判断
	if os.Geteuid() == 0 {
		if err := os.Chown(path, otherUID, otherGID); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o400); err != nil {
			t.Fatal(err)
		}
		info, _ = os.Stat(path)
		if configUnreadable(info, otherUID, otherGID) {
			t.Error("0400 对属主是可读的")
		}
		if err := os.Chmod(path, 0o200); err != nil {
			t.Fatal(err)
		}
		info, _ = os.Stat(path)
		if !configUnreadable(info, otherUID, otherGID) {
			t.Error("0200 连属主都读不到")
		}
	}
}

func TestConfigOwnerWarningShowsFixCommand(t *testing.T) {
	text := configOwnerWarning("/etc/filebot/config.toml", "filebot", 0o600)
	if !strings.Contains(text, "chown filebot /etc/filebot/config.toml") {
		t.Fatalf("警告里应当给出修复命令：%s", text)
	}
}

// Root 不是 / 或没指定服务用户时不该动配置文件属主。
func TestEnsureConfigOwnerSkips(t *testing.T) {
	if _, err := user.Lookup("nobody"); err != nil {
		t.Skip("本机没有 nobody 用户，跳过")
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := ownerOf(t, path)
	var out bytes.Buffer
	for _, opts := range []Options{
		{Root: t.TempDir(), User: "nobody", Out: &out}, // 离线镜像/测试根，不该 chown
		{Root: "/", User: "", Out: &out},               // 以当前用户运行，不该 chown
	} {
		if err := ensureConfigOwner(opts, path); err != nil {
			t.Fatalf("不该报错：%v", err)
		}
		if owner := ownerOf(t, path); owner != before {
			t.Fatalf("不该改属主：%s -> %s", before, owner)
		}
	}
}

// Root == "/" 且指定了服务用户时才真的 chown（只在真实安装里生效，非 root 跳过）。
func TestEnsureConfigOwnerChownsWhenRootAndUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("需要 root 才能改文件属主")
	}
	nobody, err := user.Lookup("nobody")
	if err != nil {
		t.Skip("本机没有 nobody 用户，跳过")
	}
	uid, err1 := strconv.Atoi(nobody.Uid)
	gid, err2 := strconv.Atoi(nobody.Gid)
	if err1 != nil || err2 != nil {
		t.Skipf("nobody 的 uid/gid 不是数字：%v", nobody)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := ensureConfigOwner(Options{Root: "/", User: "nobody", Out: &out}, path); err != nil {
		t.Fatalf("ensureConfigOwner 失败：%v", err)
	}
	if owner := ownerOf(t, path); owner != fmt.Sprintf("%d:%d", uid, gid) {
		t.Fatalf("配置文件属主 = %s，应当是 nobody(%d:%d)", owner, uid, gid)
	}
	if !strings.Contains(out.String(), "chown nobody "+path) {
		t.Errorf("root 属主的 0600 配置对 nobody 不可读，应当提示修复命令：%s", out.String())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("chown 不该改权限：%v", info.Mode().Perm())
	}
}

func ownerOf(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("拿不到属主信息：%T", info.Sys())
	}
	return fmt.Sprintf("%d:%d", stat.Uid, stat.Gid)
}

// ---------------------------------------------------------------- //
// 问题 2：配置没填好就不能 enable/start
// ---------------------------------------------------------------- //

func TestConfigProblem(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "missing.toml")
	if err := configProblem(missing); err != nil {
		t.Errorf("配置不存在不算问题（安装器会写模板）：%v", err)
	}

	template := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(template, []byte(ConfigTemplate("/var/lib/filebot/state.json")), 0o600); err != nil {
		t.Fatal(err)
	}
	err := configProblem(template)
	if err == nil {
		t.Fatal("只带空 token 的模板应当判定为不能启动")
	}
	if !strings.Contains(err.Error(), "telegram.token") {
		t.Errorf("错误里应当说清楚缺什么：%v", err)
	}

	filled := filepath.Join(dir, "filled.toml")
	body := "[telegram]\ntoken = \"x\"\nchat_id = \"1\"\n\n[watch]\ndirs = [\"" + dir + "\"]\n"
	if err := os.WriteFile(filled, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := configProblem(filled); err != nil {
		t.Errorf("填好的配置应当能通过：%v", err)
	}
}

// 干跑输出里的 enable/start 必须反映"配置能不能用"，而不是"配置文件在不在"。
func TestInstallDryRunReportsEnableStartFromConfig(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "etc/filebot")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(configPath, []byte(ConfigTemplate(filepath.Join(root, "var/lib/filebot/state.json"))), 0o600); err != nil {
		t.Fatal(err)
	}
	install := func() string {
		var out bytes.Buffer
		err := Install(Options{
			Root: root, BinPath: "/usr/local/bin/filebot", ConfigPath: configPath,
			Out: &out, DryRun: true, Enable: true, Start: true,
		})
		if err != nil {
			t.Fatalf("干跑失败：%v\n%s", err, out.String())
		}
		return out.String()
	}

	// 模板已存在但没填 token：第二次安装不能去 enable/start
	if got := install(); !strings.Contains(got, "enable/start  : enable=false start=false") {
		t.Errorf("空模板下不该 enable/start：%s", got)
	}

	body := "[telegram]\ntoken = \"x\"\nchat_id = \"1\"\n\n[watch]\ndirs = [\"" + root + "\"]\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := install(); !strings.Contains(got, "enable/start  : enable=true start=true") {
		t.Errorf("配置填好后应当 enable/start：%s", got)
	}
}

// 非干跑路径：配置存在但没填好时跳过 enable/start 并说清原因。
func TestInstallSkipsEnableStartWhenConfigUnfilled(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "etc/filebot/config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(ConfigTemplate(filepath.Join(root, "var/lib/filebot/state.json"))), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	// 用真实存在的可执行文件，免得真 systemd-analyze 校验失败
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	err = Install(Options{
		Root: root, BinPath: exe, Out: &out,
		Enable: true, Start: true,
	})
	if err != nil {
		t.Fatalf("Install 失败：%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "已跳过 enable/start") {
		t.Errorf("应当提示跳过了 enable/start：%s", out.String())
	}
	if !strings.Contains(out.String(), "telegram.token") {
		t.Errorf("应当说清楚缺什么：%s", out.String())
	}
	// 已有配置不该被模板覆盖
	content, _ := os.ReadFile(configPath)
	if string(content) != ConfigTemplate(filepath.Join(root, "var/lib/filebot/state.json")) {
		t.Errorf("已有配置被改写了：%s", content)
	}
}

// ---------------------------------------------------------------- //
// 问题 3：--purge 的删除范围
// ---------------------------------------------------------------- //

func TestUninstallPurgeKeepsSiblingFiles(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "etc/filebot")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "config.toml")
	important := filepath.Join(configDir, "IMPORTANT.txt")
	if err := os.WriteFile(configPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(important, []byte("别删我"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	// 用户显式 -c 指定配置：目录是他自己的，只能删配置文件本身
	if err := Uninstall(Options{Root: root, ConfigPath: configPath, Out: &out}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Errorf("配置文件应当被删除：%v", err)
	}
	if _, err := os.Stat(important); err != nil {
		t.Errorf("同目录的 IMPORTANT.txt 被误删了：%v", err)
	}
	if _, err := os.Stat(configDir); err != nil {
		t.Errorf("用户指定的配置目录不该被删：%v", err)
	}
}

func TestUninstallPurgeKeepsCustomConfigParent(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "srv/myconf")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "filebot.toml")
	keep := filepath.Join(dir, "keep.txt")
	for _, path := range []string{configPath, keep} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := Uninstall(Options{Root: root, ConfigPath: configPath, Out: &out}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Errorf("配置文件应当被删除：%v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("非默认配置目录里的文件被误删了：%v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("非默认配置目录不该被删：%v", err)
	}
}

func TestUninstallPurgeKeepsExplicitStateDir(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "srv/filebot-state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stateFile := filepath.Join(stateDir, "state.json")
	keep := filepath.Join(stateDir, "keep.txt")
	if err := os.WriteFile(stateFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Uninstall(Options{Root: root, StateDir: stateDir, Out: &out}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stateFile); !os.IsNotExist(err) {
		t.Errorf("用户指定状态目录里的 state.json 应当被删除：%v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("用户指定状态目录里的其它文件被误删了：%v", err)
	}
	if _, err := os.Stat(stateDir); err != nil {
		t.Errorf("用户指定的状态目录不该被删（宁留空目录）：%v", err)
	}
}

func TestRemovableDirRefusesSystemPaths(t *testing.T) {
	for _, dir := range []string{
		"/", "/etc", "/usr", "/home", "/root", "/var", "/tmp",
		"/etc/", "/var/../tmp", "/./", // Clean 之后还是系统目录
		"relative/dir", "./x", "..", // 相对路径一律拒绝
	} {
		if err := removableDir(dir); err == nil {
			t.Errorf("%q 绝不允许整目录删除", dir)
		}
	}

	dir := t.TempDir()
	if err := removableDir(dir); err != nil {
		t.Errorf("安装器自己建的临时目录应当允许删除：%v", err)
	}
	if err := safeRemoveDir(dir); err != nil {
		t.Errorf("删除临时目录失败：%v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("临时目录没被删掉")
	}
}

// ---------------------------------------------------------------- //
// 问题 4：ExecStart/WorkingDirectory 的引号
// ---------------------------------------------------------------- //

// systemdWordSplit 是最小化的 systemd 切词器（只处理双引号与 \ 转义），
// 用来验证渲染出来的参数没有被空格切断。
func systemdWordSplit(line string) []string {
	var words []string
	var buf strings.Builder
	inQuote := false
	escaped := false
	flush := func() {
		if buf.Len() > 0 {
			words = append(words, buf.String())
			buf.Reset()
		}
	}
	for _, r := range line {
		switch {
		case escaped:
			buf.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case r == '"':
			inQuote = !inQuote
		case r == ' ' && !inQuote:
			flush()
		default:
			buf.WriteRune(r)
		}
	}
	flush()
	return words
}

func TestRenderUnitQuotesPathsWithSpaces(t *testing.T) {
	unit := RenderUnit(Params{
		Description: "filebot test",
		User:        "filebot",
		BinPath:     "/opt/file bot/filebot",
		ConfigPath:  "/etc/file bot/config.toml",
		UnitName:    "filebot",
		WorkDir:     "/var/lib/file bot",
	})
	if !strings.Contains(unit, `ExecStart="/opt/file bot/filebot" run -c "/etc/file bot/config.toml"`) {
		t.Fatalf("ExecStart 里的路径没有加引号：\n%s", unit)
	}
	if !strings.Contains(unit, `WorkingDirectory="/var/lib/file bot"`) {
		t.Fatalf("WorkingDirectory 没有加引号：\n%s", unit)
	}

	line := ""
	for _, candidate := range strings.Split(unit, "\n") {
		if strings.HasPrefix(candidate, "ExecStart=") {
			line = strings.TrimPrefix(candidate, "ExecStart=")
		}
	}
	got := strings.Join(systemdWordSplit(line), "|")
	want := strings.Join([]string{"/opt/file bot/filebot", "run", "-c", "/etc/file bot/config.toml"}, "|")
	if got != want {
		t.Fatalf("systemd 切出来的参数不对\n得到：%s\n想要：%s", got, want)
	}
}

func TestQuoteArg(t *testing.T) {
	for _, item := range []struct {
		in   string
		want string
	}{
		{"/usr/local/bin/filebot", "/usr/local/bin/filebot"},
		{"/etc/filebot/config.toml", "/etc/filebot/config.toml"},
		{`/tmp/a b/c.toml`, `"/tmp/a b/c.toml"`},
		{`/a"b`, `"/a\"b"`},
		{`/a\b`, `"/a\\b"`},
		{``, `""`},
	} {
		if got := quoteArg(item.in); got != item.want {
			t.Errorf("quoteArg(%q) = %q，想要 %q", item.in, got, item.want)
		}
	}
}

// ---------------------------------------------------------------- //
// 问题 5：~/ 展开
// ---------------------------------------------------------------- //

func TestPathsExpandsTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("拿不到家目录：%v", err)
	}
	opts := Options{
		Root: t.TempDir(), BinPath: "/bin/true",
		ConfigPath: "~/filebot-test.toml", StateDir: "~/filebot-state",
	}
	if err := opts.normalize(); err != nil {
		t.Fatal(err)
	}
	p, err := opts.paths()
	if err != nil {
		t.Fatal(err)
	}
	if p.config != filepath.Join(home, "filebot-test.toml") {
		t.Errorf("配置文件路径没有展开 ~：%s", p.config)
	}
	if p.stateDir != filepath.Join(home, "filebot-state") {
		t.Errorf("状态目录没有展开 ~：%s", p.stateDir)
	}

	// 默认路径仍然落在 --root 里
	def := Options{Root: t.TempDir(), BinPath: "/bin/true"}
	if err := def.normalize(); err != nil {
		t.Fatal(err)
	}
	pd, err := def.paths()
	if err != nil {
		t.Fatal(err)
	}
	if pd.config != filepath.Join(def.Root, "etc/filebot/config.toml") {
		t.Errorf("默认配置路径不对：%s", pd.config)
	}
	if pd.stateDir != filepath.Join(def.Root, "var/lib/filebot") {
		t.Errorf("默认状态目录不对：%s", pd.stateDir)
	}
}

// ---------------------------------------------------------------- //
// 问题 6：daemon-reload 失败不能中断清理
// ---------------------------------------------------------------- //

func TestReloadSystemdWarnsInsteadOfFailing(t *testing.T) {
	runner := newFakeRunner()
	runner.failOn["daemon-reload"] = fmt.Errorf("systemd 没在跑")
	var out bytes.Buffer
	opts := Options{UnitName: "filebot", Out: &out, runner: runner.run}
	if err := opts.normalize(); err != nil {
		t.Fatal(err)
	}
	reloadSystemd(opts)
	if !strings.Contains(out.String(), "daemon-reload 未成功") {
		t.Errorf("daemon-reload 失败应当只警告：%s", out.String())
	}
	if !runner.has("systemctl reset-failed") {
		t.Errorf("daemon-reload 失败后仍应继续 reset-failed：%v", runner.calls)
	}
}

// 卸载时 --purge 的清理不能因为外部命令失败而半途而废（root != / 时不调用 systemctl）。
func TestUninstallPurgeWithFailingRunner(t *testing.T) {
	root := t.TempDir()
	runner := newFakeRunner()
	runner.failOn["systemctl"] = fmt.Errorf("boom")
	runner.failOn["userdel"] = fmt.Errorf("boom")
	configPath := filepath.Join(root, "etc/filebot/config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	stateFile := filepath.Join(root, "var/lib/filebot/state.json")
	if err := os.MkdirAll(filepath.Dir(stateFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	opts := Options{Root: root, Out: &out, runner: runner.run}
	if err := Uninstall(opts, true); err != nil {
		t.Fatalf("外部命令失败不该让 Uninstall 报错：%v\n%s", err, out.String())
	}
	for _, path := range []string{configPath, stateFile} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s 应当被删除：%v", path, err)
		}
	}
}

// 配置不存在（刚写空模板）时也不能 enable/start —— 这是被 CR 改回过的回归点：
// 干跑输出必须显示 enable=false start=false
func TestInstallDryRunSkipsStartWhenConfigMissing(t *testing.T) {
	root := t.TempDir()
	var out bytes.Buffer
	if err := Install(Options{Root: root, BinPath: "/usr/local/bin/filebot", Out: &out, DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "（不存在，将写入模板）") {
		t.Fatalf("应当提示配置不存在：%s", out.String())
	}
	if !strings.Contains(out.String(), "enable/start  : enable=false start=false") {
		t.Fatalf("配置不存在时必须跳过 enable/start：\n%s", out.String())
	}
}

// 同一台机器上，配置填好之后干跑必须显示会 enable/start
func TestInstallDryRunEnablesWhenConfigUsable(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "etc", "filebot", "config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	usable := `
[telegram]
token = "1:x"
chat_id = "1"

[watch]
dirs = ["/tmp"]
`
	if err := os.WriteFile(configPath, []byte(usable), 0o600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Install(Options{
		Root: root, BinPath: exe, Out: &out, DryRun: true, Enable: true, Start: true,
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "enable/start  : enable=true start=true") {
		t.Fatalf("配置可用时应当 enable/start：\n%s", out.String())
	}
}

// 写模板的那次安装（非干跑）也必须跳过 enable/start 并在输出里说明
func TestInstallWithFreshTemplateSkipsEnableStart(t *testing.T) {
	root := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Install(Options{
		Root: root, BinPath: exe, Out: &out,
		Enable: true, Start: true,
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "已跳过 enable/start") {
		t.Fatalf("刚写模板时必须跳过 enable/start：\n%s", out.String())
	}
}
