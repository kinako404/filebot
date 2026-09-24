package main_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"filebot/internal/testsupport"
)

// buildBinary 编译一次真实二进制，供所有子命令测试使用。
var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

func binary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "filebot-bin-")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "filebot")
		cmd := exec.Command("go", "build", "-o", binPath, ".")
		cmd.Dir = repoRoot()
		output, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("go build 失败：%v\n%s", err, output)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binPath
}

func repoRoot() string {
	// go test 会把工作目录设为被测包所在目录，这里就是仓库根（main 包）
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	return dir
}

type env struct {
	t    *testing.T
	fake *testsupport.FakeTelegram
	root string
	bin  string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	fake := testsupport.NewFakeTelegram()
	t.Cleanup(fake.Close)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &env{t: t, fake: fake, root: root, bin: binary(t)}
}

func (e *env) write(relative string, size int) string {
	e.t.Helper()
	path := filepath.Join(e.root, "media", relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		e.t.Fatal(err)
	}
	return path
}

func (e *env) configFile(extra string) string {
	e.t.Helper()
	path := filepath.Join(e.root, "config.toml")
	content := fmt.Sprintf(`
[telegram]
token = "cli:token"
chat_id = "-100777"
api_base = "%s"

[watch]
dirs = ["%s"]
state_file = "%s"
min_size = 0
scan_existing = true

[upload]
caption_template = "{name}"
%s
`, e.fake.URL(), filepath.Join(e.root, "media"), filepath.Join(e.root, "state.json"), extra)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		e.t.Fatal(err)
	}
	return path
}

type result struct {
	code   int
	stdout string
	stderr string
}

func (e *env) run(args ...string) result {
	e.t.Helper()
	cmd := exec.Command(e.bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = cleanEnv()
	err := cmd.Run()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		} else {
			e.t.Fatalf("执行失败：%v", err)
		}
	}
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func cleanEnv() []string {
	out := []string{}
	for _, item := range os.Environ() {
		if strings.HasPrefix(item, "FILEBOT_") {
			continue
		}
		out = append(out, item)
	}
	return out
}

func TestVersionAndHelp(t *testing.T) {
	e := newEnv(t)
	if got := e.run("version"); got.code != 0 || !strings.Contains(got.stdout, "filebot") {
		t.Fatalf("version = %+v", got)
	}
	if got := e.run("help"); got.code != 0 || !strings.Contains(got.stdout, "install-service") {
		t.Fatalf("help = %+v", got)
	}
}

func TestOnceSendsFiles(t *testing.T) {
	e := newEnv(t)
	e.write("photo.jpg", 2048)
	e.write("sub/clip.mp4", 4096)

	got := e.run("once", "-c", e.configFile(""))
	if got.code != 0 {
		t.Fatalf("退出码 = %d\n%s", got.code, got.stderr)
	}
	methods := e.fake.Methods()
	if count(methods, "sendDocument") != 2 || count(methods, "sendPhoto") != 1 || count(methods, "sendVideo") != 1 {
		t.Fatalf("methods = %v", methods)
	}
	if e.fake.CallsOf("sendDocument")[0].Fields["chat_id"] != "-100777" {
		t.Fatalf("chat_id 不对：%v", e.fake.CallsOf("sendDocument")[0].Fields)
	}
	if _, err := os.Stat(filepath.Join(e.root, "state.json")); err != nil {
		t.Fatalf("状态文件应当存在：%v", err)
	}
}

func TestOnceIsIdempotent(t *testing.T) {
	e := newEnv(t)
	e.write("photo.jpg", 2048)
	config := e.configFile("")
	e.run("once", "-c", config)
	first := len(e.fake.Calls())
	e.run("once", "-c", config)
	if len(e.fake.Calls()) != first {
		t.Fatalf("第二次不该再发：%d → %d", first, len(e.fake.Calls()))
	}
}

func TestCheckOnlyCallsGetMe(t *testing.T) {
	e := newEnv(t)
	got := e.run("check", "-c", e.configFile(""))
	if got.code != 0 {
		t.Fatalf("退出码 = %d\n%s", got.code, got.stderr)
	}
	if methods := e.fake.Methods(); len(methods) != 1 || methods[0] != "getMe" {
		t.Fatalf("methods = %v", methods)
	}
	if !strings.Contains(got.stderr, "filebot_test") {
		t.Fatalf("自检日志里应当有 bot 用户名：%s", got.stderr)
	}
}

func TestMissingTokenExitsWithCode2(t *testing.T) {
	e := newEnv(t)
	config := e.configFile("")
	content, _ := os.ReadFile(config)
	patched := strings.Replace(string(content), `token = "cli:token"`, `token = ""`, 1)
	if err := os.WriteFile(config, []byte(patched), 0o600); err != nil {
		t.Fatal(err)
	}
	got := e.run("once", "-c", config)
	if got.code != 2 {
		t.Fatalf("退出码 = %d，期望 2\n%s", got.code, got.stderr)
	}
	if !strings.Contains(got.stderr, "配置错误") {
		t.Fatalf("stderr = %s", got.stderr)
	}
}

func TestEnvOnlyInvocation(t *testing.T) {
	e := newEnv(t)
	e.write("photo.jpg", 2048)

	cmd := exec.Command(e.bin, "once", "--min-size", "0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Env = append(cleanEnv(),
		"FILEBOT_TG_TOKEN=env:token",
		"FILEBOT_TG_CHAT_ID=-100888",
		"FILEBOT_TG_API_BASE="+e.fake.URL(),
		"FILEBOT_WATCH_DIRS="+filepath.Join(e.root, "media"),
		"FILEBOT_STATE_FILE="+filepath.Join(e.root, "state-env.json"),
	)
	if err := cmd.Run(); err != nil {
		t.Fatalf("执行失败：%v\n%s", err, stderr.String())
	}
	calls := e.fake.CallsOf("sendPhoto")
	if len(calls) != 1 || calls[0].Fields["chat_id"] != "-100888" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestFlagsOverrideEnvAndFile(t *testing.T) {
	e := newEnv(t)
	e.write("photo.jpg", 2048)
	config := e.configFile("")
	cmd := exec.Command(e.bin, "once", "-c", config,
		"--chat-id", "-42", "--no-compressed", "--log-level", "DEBUG")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Env = append(cleanEnv(), "FILEBOT_TG_CHAT_ID=-999")
	if err := cmd.Run(); err != nil {
		t.Fatalf("执行失败：%v\n%s", err, stderr.String())
	}
	methods := e.fake.Methods()
	if len(methods) != 1 || methods[0] != "sendDocument" {
		t.Fatalf("methods = %v", methods)
	}
	if e.fake.CallsOf("sendDocument")[0].Fields["chat_id"] != "-42" {
		t.Fatalf("--chat-id 没生效：%v", e.fake.CallsOf("sendDocument")[0].Fields)
	}
	if !strings.Contains(stderr.String(), "已发送原文件") {
		t.Fatalf("stderr = %s", stderr.String())
	}
}

func TestSocks5ProxyConfiguration(t *testing.T) {
	e := newEnv(t)
	e.write("photo.jpg", 2048)
	socks, err := testsupport.NewFakeSocks5()
	if err != nil {
		t.Fatal(err)
	}
	defer socks.Close()

	got := e.run("once", "-c", e.configFile(""),
		"--proxy", "socks5://"+socks.Addr())
	if got.code != 0 {
		t.Fatalf("退出码 = %d\n%s", got.code, got.stderr)
	}
	if count(e.fake.Methods(), "sendDocument") != 1 {
		t.Fatalf("methods = %v", e.fake.Methods())
	}
	targets := socks.Targets()
	if len(targets) == 0 {
		t.Fatal("流量没有走 socks5 代理")
	}
}

func TestHTTPProxyConfiguration(t *testing.T) {
	e := newEnv(t)
	e.write("photo.jpg", 2048)
	proxyServer, err := testsupport.NewFakeHTTPProxy()
	if err != nil {
		t.Fatal(err)
	}
	defer proxyServer.Close()

	got := e.run("once", "-c", e.configFile(""), "--proxy", proxyServer.URL())
	if got.code != 0 {
		t.Fatalf("退出码 = %d\n%s", got.code, got.stderr)
	}
	if count(e.fake.Methods(), "sendDocument") != 1 {
		t.Fatalf("methods = %v\n%s", e.fake.Methods(), got.stderr)
	}
	if len(proxyServer.Targets()) == 0 {
		t.Fatal("流量没有走 http 代理")
	}
}

func TestBadProxySchemeIsRejected(t *testing.T) {
	e := newEnv(t)
	e.write("photo.jpg", 2048)
	got := e.run("once", "-c", e.configFile(""), "--proxy", "ss://aes-256-gcm:pw@1.2.3.4:8388")
	if got.code == 0 {
		t.Fatal("不支持的代理协议应当报错退出")
	}
	if !strings.Contains(got.stderr, "不支持的代理协议") {
		t.Fatalf("stderr = %s", got.stderr)
	}
}

func TestDaemonModeAndSigterm(t *testing.T) {
	e := newEnv(t)

	cmd := exec.Command(e.bin, "run", "-c", e.configFile(""),
		"--poll-interval", "0.2", "--settle-seconds", "0.2")
	logs := &syncBuffer{}
	cmd.Stderr = logs
	cmd.Env = cleanEnv()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()

	// 常驻模式只发运行中新增的文件：先等它自检完并拍完基准快照，再放文件进去
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && len(e.fake.CallsOf("getMe")) == 0 {
		time.Sleep(100 * time.Millisecond)
	}
	if len(e.fake.CallsOf("getMe")) == 0 {
		t.Fatalf("常驻进程没有完成自检\n%s", logs.String())
	}
	time.Sleep(500 * time.Millisecond)
	e.write("photo.jpg", 2048)

	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && (len(e.fake.CallsOf("sendDocument")) == 0 || len(e.fake.CallsOf("sendPhoto")) == 0) {
		time.Sleep(200 * time.Millisecond)
	}
	// 断言真正关心的事：运行中新增的文件被发出去了（不能只数 getMe）
	if len(e.fake.CallsOf("sendDocument")) == 0 {
		t.Fatalf("常驻模式没有发送运行中新增的文件\n%s", logs.String())
	}
	if len(e.fake.CallsOf("sendPhoto")) == 0 {
		t.Fatalf("可点开的版本没有发送\n%s", logs.String())
	}

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	select {
	case err := <-waitErr:
		if err != nil {
			t.Fatalf("SIGINT 之后应当正常退出：%v\n%s", err, logs.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("SIGINT 之后没有退出")
	}
	if !strings.Contains(logs.String(), "已退出") {
		t.Fatalf("logs = %s", logs.String())
	}
}

func TestMultipleChatIDsViaFlags(t *testing.T) {
	e := newEnv(t)
	e.write("photo.jpg", 2048)

	// 逗号分隔 + 重复出现两种写法都要能用
	got := e.run("once", "-c", e.configFile(""),
		"--chat-id", "@a,@b", "--chat-id=-1001234567890")
	if got.code != 0 {
		t.Fatalf("退出码 = %d\n%s", got.code, got.stderr)
	}
	seen := map[string]int{}
	for _, call := range e.fake.CallsOf("sendDocument") {
		seen[call.Fields["chat_id"]]++
	}
	for _, chat := range []string{"@a", "@b", "-1001234567890"} {
		if seen[chat] != 1 {
			t.Errorf("%s 收到 %d 份原文件，期望 1（全部：%v）", chat, seen[chat], seen)
		}
	}
	if len(e.fake.CallsOf("sendPhoto")) != 3 {
		t.Fatalf("预览消息数 = %d，期望 3", len(e.fake.CallsOf("sendPhoto")))
	}
}

func TestChatIDFromConfigArray(t *testing.T) {
	e := newEnv(t)
	e.write("photo.jpg", 2048)
	config := e.configFile("")
	content, _ := os.ReadFile(config)
	patched := strings.Replace(string(content),
		`chat_id = "-100777"`, "chat_id = [\"-100777\", \"@second\"]", 1)
	if err := os.WriteFile(config, []byte(patched), 0o600); err != nil {
		t.Fatal(err)
	}
	got := e.run("once", "-c", config)
	if got.code != 0 {
		t.Fatalf("退出码 = %d\n%s", got.code, got.stderr)
	}
	seen := map[string]int{}
	for _, call := range e.fake.CallsOf("sendDocument") {
		seen[call.Fields["chat_id"]]++
	}
	if seen["-100777"] != 1 || seen["@second"] != 1 {
		t.Fatalf("两个接收方都该收到：%v", seen)
	}
}

func TestScanExistingFlagIsGone(t *testing.T) {
	e := newEnv(t)
	got := e.run("once", "-c", e.configFile(""), "--scan-existing")
	if got.code == 0 {
		t.Fatal("--scan-existing 已经移除，应当报未知参数")
	}
	if !strings.Contains(got.stderr, "scan-existing") {
		t.Fatalf("stderr = %s", got.stderr)
	}
}

func TestDaemonIgnoresScanExistingFromConfig(t *testing.T) {
	e := newEnv(t)
	// 这份配置里本来就写着 scan_existing = true；常驻模式必须忽略它，只发运行中新增的
	config := e.configFile("")
	raw, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "scan_existing = true") {
		t.Fatal("测试夹具应当包含 scan_existing = true")
	}
	e.write("old.jpg", 2048)

	cmd := exec.Command(e.bin, "run", "-c", config, "--poll-interval", "0.1", "--settle-seconds", "0.1")
	logs := &syncBuffer{}
	cmd.Stderr = logs
	cmd.Env = cleanEnv()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(logs.String(), "已忽略 watch.scan_existing") {
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(logs.String(), "已忽略 watch.scan_existing") {
		t.Fatalf("应当提示忽略了 scan_existing：\n%s", logs.String())
	}
	// 这条日志打在基准快照之前，必须等快照拍完再放文件，否则它会被当成"启动前就存在"
	time.Sleep(500 * time.Millisecond)

	e.write("new.jpg", 2048)
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && len(e.fake.CallsOf("sendPhoto")) == 0 {
		time.Sleep(100 * time.Millisecond)
	}
	cmd.Process.Signal(os.Interrupt)
	cmd.Wait()

	for _, call := range e.fake.Calls() {
		for _, part := range call.Files {
			if part.Filename == "old.jpg" {
				t.Fatalf("启动前就存在的文件不该被发送：%+v", call.Fields)
			}
		}
	}
	if len(e.fake.CallsOf("sendPhoto")) != 1 {
		t.Fatalf("新文件应当被发送一次：%v\n%s", e.fake.Methods(), logs.String())
	}
}

// syncBuffer 允许一边被进程写入、一边被测试读取（bytes.Buffer 不是并发安全的）。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestInstallServiceDryRun(t *testing.T) {
	e := newEnv(t)
	got := e.run("install-service", "-c", e.configFile(""), "--dry-run")
	if got.code != 0 {
		t.Fatalf("退出码 = %d\n%s", got.code, got.stderr)
	}
	for _, want := range []string{"干跑模式", "ExecStart=", "[Service]", "/etc/systemd/system/filebot.service"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("输出里缺少 %q：\n%s", want, got.stdout)
		}
	}
	if !strings.Contains(got.stdout, e.bin) {
		t.Errorf("应当用当前二进制路径：\n%s", got.stdout)
	}
}

func TestInstallServiceIntoCustomRoot(t *testing.T) {
	e := newEnv(t)
	got := e.run("install-service", "-c", e.configFile(""),
		"--root", e.root, "--user", "")
	if got.code != 0 {
		t.Fatalf("退出码 = %d\n%s\n%s", got.code, got.stdout, got.stderr)
	}
	unit := filepath.Join(e.root, "etc/systemd/system/filebot.service")
	content, err := os.ReadFile(unit)
	if err != nil {
		t.Fatalf("单元文件没写出来：%v", err)
	}
	if !strings.Contains(string(content), "ExecStart="+e.bin+" run -c "+e.configFile("")) {
		t.Fatalf("单元内容不对：\n%s", content)
	}
	if !strings.Contains(got.stdout, "跳过 systemctl") {
		t.Fatalf("root != / 应当跳过 systemctl：%s", got.stdout)
	}

	// 卸载
	got = e.run("uninstall-service", "--root", e.root, "--purge")
	if got.code != 0 {
		t.Fatalf("卸载退出码 = %d\n%s", got.code, got.stderr)
	}
	if _, err := os.Stat(unit); !os.IsNotExist(err) {
		t.Fatal("卸载后单元文件应当消失")
	}
}

func TestUnknownSubcommandFallsBackToRun(t *testing.T) {
	// 未知子命令会被当作 run 的参数解析，最终因缺少必填配置而退出码 2
	e := newEnv(t)
	got := e.run("--config", "/definitely/missing.toml")
	if got.code != 2 {
		t.Fatalf("退出码 = %d\n%s", got.code, got.stderr)
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
