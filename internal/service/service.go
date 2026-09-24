// Package service 负责生成并安装 systemd 单元（面向 Debian 12/13）。
//
// 安装流程：检查环境 → 必要时补一份配置模板 → 创建系统用户 → 渲染单元文件 →
// 用 systemd-analyze verify 校验 → 落盘 → daemon-reload → enable/start。
package service

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// 默认路径（相对 --root）。
const (
	DefaultUnitName = "filebot"
	DefaultUser     = "filebot"
	defaultBinDir   = "/usr/local/bin"
	defaultConfDir  = "/etc/filebot"
	defaultStateDir = "/var/lib/filebot"
	unitFileMode    = 0o644
	configFileMode  = 0o600
)

// Options 是安装/卸载参数。
type Options struct {
	Root       string // 前缀，默认 "/"；非 "/" 时用于离线镜像或测试
	UnitName   string
	BinPath    string // 默认当前可执行文件
	ConfigPath string
	StateDir   string
	User       string // 空字符串表示以当前用户运行（不创建系统用户）
	Start      bool
	Enable     bool
	DryRun     bool
	Force      bool

	Out    io.Writer
	runner func(name string, args ...string) (string, error)
}

type paths struct {
	root     string
	unit     string
	bin      string
	config   string
	stateDir string
}

func (o *Options) normalize() error {
	if o.Root == "" {
		o.Root = "/"
	}
	abs, err := filepath.Abs(o.Root)
	if err != nil {
		return err
	}
	o.Root = abs
	if o.UnitName == "" {
		o.UnitName = DefaultUnitName
	}
	if strings.ContainsAny(o.UnitName, "/ \t") {
		return fmt.Errorf("服务名不合法：%q", o.UnitName)
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.runner == nil {
		o.runner = runCommand
	}
	return nil
}

func (o *Options) paths() (paths, error) {
	bin := o.BinPath
	if bin == "" {
		exe, err := os.Executable()
		if err != nil {
			return paths{}, fmt.Errorf("无法确定当前可执行文件路径：%w", err)
		}
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		bin = exe
	}
	bin, err := filepath.Abs(bin)
	if err != nil {
		return paths{}, err
	}
	config := o.ConfigPath
	if config == "" {
		config = inRoot(o.Root, defaultConfDir, "config.toml")
	}
	stateDir := o.StateDir
	if stateDir == "" {
		stateDir = inRoot(o.Root, defaultStateDir, "")
	}
	return paths{
		root:     o.Root,
		unit:     inRoot(o.Root, "/etc/systemd/system", o.UnitName+".service"),
		bin:      bin,
		config:   config,
		stateDir: stateDir,
	}, nil
}

func inRoot(root, dir, file string) string {
	if file == "" {
		return filepath.Join(root, dir)
	}
	return filepath.Join(root, dir, file)
}

// Params 是渲染 systemd 单元需要的参数。
type Params struct {
	Description string
	User        string
	BinPath     string
	ConfigPath  string
	UnitName    string
	WorkDir     string
}

// RenderUnit 生成 systemd 单元内容。
func RenderUnit(p Params) string {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=" + p.Description + "\n")
	b.WriteString("After=network-online.target\n")
	b.WriteString("Wants=network-online.target\n")
	b.WriteString("\n[Service]\n")
	b.WriteString("Type=simple\n")
	if p.User != "" {
		b.WriteString("User=" + p.User + "\n")
		b.WriteString("Group=" + p.User + "\n")
	}
	b.WriteString("ExecStart=" + p.BinPath + " run -c " + p.ConfigPath + "\n")
	if p.WorkDir != "" {
		b.WriteString("WorkingDirectory=" + p.WorkDir + "\n")
	}
	b.WriteString("Restart=always\n")
	b.WriteString("RestartSec=5\n")
	b.WriteString("StateDirectory=" + p.UnitName + "\n")
	b.WriteString("NoNewPrivileges=true\n")
	b.WriteString("ProtectSystem=full\n")
	b.WriteString("PrivateTmp=true\n")
	b.WriteString("ProtectKernelTunables=true\n")
	b.WriteString("ProtectControlGroups=true\n")
	b.WriteString("RestrictSUIDSGID=true\n")
	b.WriteString("StandardOutput=journal\n")
	b.WriteString("StandardError=journal\n")
	b.WriteString("SyslogIdentifier=" + p.UnitName + "\n")
	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=multi-user.target\n")
	return b.String()
}

// ConfigTemplate 是自动补全的配置模板；stateFile 会写进模板。
func ConfigTemplate(stateFile string) string {
	return `# filebot 配置（由 "filebot install-service" 自动生成）
# 改完执行：systemctl restart filebot
#
# 顶层选项（必须写在任何 [section] 之前）
workers = 1
log_level = "INFO"

[telegram]
# 必填：@BotFather 给的 token
token = ""
# 必填：接收方（私聊数字 id、@channel 或 -100 开头的群/频道 id）；支持多个
chat_id = ""
# chat_id = ["-1001234567890", "@mychannel"]

# 可选：自建 local Bot API server 时改成 http://127.0.0.1:8081 可突破 50MB 上传上限
api_base = "https://api.telegram.org"
min_send_interval = 1.0
timeout = 120
max_retries = 3

[proxy]
# 可选：socks5://[user:pass@]host:port 或 http://[user:pass@]host:port；留空直连
url = ""

[watch]
# 必填：要监控的目录，含子目录
dirs = ["/data/media"]
poll_interval = 2.0
settle_seconds = 3.0
min_size = 1024
exclude = ["*.part", "*.tmp", "*.crdownload", "*.download", "*.partial"]
# 常驻模式只发送启动之后新增/变化的文件；补发已有文件请跑 filebot once
state_file = "` + stateFile + `"

[upload]
send_original = true
send_compressed = true
max_upload_mb = 50
photo_max_mb = 10
video_max_mb = 50
caption_template = "{name}\n{size} · {path}"
notify_skipped = false
`
}

// Install 安装并（按需）启用服务。
func Install(opts Options) error {
	if err := opts.normalize(); err != nil {
		return err
	}
	p, err := opts.paths()
	if err != nil {
		return err
	}
	out := opts.Out

	if err := checkEnvironment(opts); err != nil {
		return err
	}

	unitParams := Params{
		Description: "filebot - 监控目录并把图片/视频发送到 Telegram",
		User:        opts.User,
		BinPath:     p.bin,
		ConfigPath:  p.config,
		UnitName:    opts.UnitName,
	}
	if opts.User != "" {
		unitParams.WorkDir = p.stateDir
	}
	unit := RenderUnit(unitParams)

	configMissing := false
	if _, err := os.Stat(p.config); err != nil {
		configMissing = true
	}
	start := opts.Start && !configMissing
	enable := opts.Enable && !configMissing

	if opts.DryRun {
		fmt.Fprintf(out, "== 干跑模式，不会写入任何文件 ==\n")
		fmt.Fprintf(out, "二进制        : %s\n", p.bin)
		fmt.Fprintf(out, "配置文件      : %s%s\n", p.config, markIf(configMissing, "（不存在，将写入模板）"))
		fmt.Fprintf(out, "状态目录      : %s\n", p.stateDir)
		fmt.Fprintf(out, "单元文件      : %s\n", p.unit)
		fmt.Fprintf(out, "运行用户      : %s\n", orDefault(opts.User, "（当前用户）"))
		fmt.Fprintf(out, "systemctl     : %s\n", orDefault(lookPath("systemctl"), "未找到"))
		if opts.User != "" {
			fmt.Fprintf(out, "useradd       : %s\n", orDefault(lookPath("useradd"), "未找到"))
		}
		fmt.Fprintf(out, "enable/start  : enable=%v start=%v\n", enable, start)
		fmt.Fprintf(out, "\n---- %s ----\n%s", filepath.Base(p.unit), unit)
		return nil
	}

	if opts.User != "" && opts.Root == "/" {
		if err := ensureUser(opts, opts.User, p.stateDir); err != nil {
			return err
		}
	}
	if err := prepareStateDir(opts, p.stateDir); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(p.config), 0o755); err != nil {
		return fmt.Errorf("创建配置目录失败：%w", err)
	}
	if configMissing {
		if err := os.WriteFile(p.config, []byte(ConfigTemplate(filepath.Join(p.stateDir, "state.json"))), configFileMode); err != nil {
			return fmt.Errorf("写入配置模板失败：%w", err)
		}
		fmt.Fprintf(out, "已生成配置模板：%s（请填入 token / chat_id / 监控目录）\n", p.config)
	}

	if err := verifyUnit(opts, unit); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(p.unit), 0o755); err != nil {
		return fmt.Errorf("创建单元目录失败：%w", err)
	}
	if _, err := os.Stat(p.unit); err == nil && !opts.Force {
		if changed, _ := fileChanged(p.unit, unit); !changed {
			fmt.Fprintf(out, "单元文件已是最新：%s\n", p.unit)
		} else {
			fmt.Fprintf(out, "覆盖已有单元文件：%s（用 --force 可跳过此提示）\n", p.unit)
		}
	}
	if err := os.WriteFile(p.unit, []byte(unit), unitFileMode); err != nil {
		return fmt.Errorf("写入单元文件失败：%w", err)
	}
	fmt.Fprintf(out, "已写入单元文件：%s\n", p.unit)

	if opts.Root != "/" {
		fmt.Fprintf(out, "\n--root=%s 不是 /，跳过 systemctl 调用（离线镜像场景请手工启用）\n", opts.Root)
		return nil
	}

	if _, err := opts.runner("systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload 失败：%w", err)
	}
	if enable {
		if _, err := opts.runner("systemctl", "enable", opts.UnitName); err != nil {
			return fmt.Errorf("systemctl enable 失败：%w", err)
		}
		fmt.Fprintf(out, "已设置开机自启：%s\n", opts.UnitName)
	}
	if start {
		if _, err := opts.runner("systemctl", "restart", opts.UnitName); err != nil {
			return fmt.Errorf("systemctl restart 失败：%w", err)
		}
		fmt.Fprintf(out, "服务已启动：%s\n", opts.UnitName)
	}

	fmt.Fprintf(out, "\n后续命令：\n")
	if configMissing {
		fmt.Fprintf(out, "  1) 编辑配置：%s\n", p.config)
		fmt.Fprintf(out, "  2) 启动服务：systemctl start %s\n", opts.UnitName)
		fmt.Fprintf(out, "  3) 看日志　：journalctl -u %s -f\n", opts.UnitName)
	} else {
		fmt.Fprintf(out, "  看日志：journalctl -u %s -f\n", opts.UnitName)
		fmt.Fprintf(out, "  改配置后重启：systemctl restart %s\n", opts.UnitName)
	}
	return nil
}

// Uninstall 停止并移除服务。
func Uninstall(opts Options, purge bool) error {
	if err := opts.normalize(); err != nil {
		return err
	}
	p, err := opts.paths()
	if err != nil {
		return err
	}
	out := opts.Out

	if opts.DryRun {
		fmt.Fprintf(out, "== 干跑模式 ==\n")
		fmt.Fprintf(out, "将停止并禁用：%s\n", opts.UnitName)
		fmt.Fprintf(out, "将删除单元文件：%s\n", p.unit)
		if purge {
			fmt.Fprintf(out, "将删除配置：%s\n", p.config)
			fmt.Fprintf(out, "将删除状态目录：%s\n", p.stateDir)
			if opts.User != "" {
				fmt.Fprintf(out, "将删除系统用户：%s\n", opts.User)
			}
		}
		return nil
	}

	if opts.Root == "/" && lookPath("systemctl") != "" {
		for _, args := range [][]string{
			{"stop", opts.UnitName},
			{"disable", opts.UnitName},
		} {
			if _, err := opts.runner("systemctl", args...); err != nil {
				fmt.Fprintf(out, "systemctl %s 未成功（可能本来就没装）：%v\n", strings.Join(args, " "), err)
			}
		}
	}

	if err := os.Remove(p.unit); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("删除单元文件失败：%w", err)
	}
	fmt.Fprintf(out, "已删除单元文件：%s\n", p.unit)

	if opts.Root == "/" && lookPath("systemctl") != "" {
		if _, err := opts.runner("systemctl", "daemon-reload"); err != nil {
			return fmt.Errorf("systemctl daemon-reload 失败：%w", err)
		}
		if _, err := opts.runner("systemctl", "reset-failed", opts.UnitName); err != nil {
			fmt.Fprintf(out, "systemctl reset-failed 忽略：%v\n", err)
		}
	}

	if purge {
		for _, dir := range []string{filepath.Dir(p.config), p.stateDir} {
			if err := os.RemoveAll(dir); err != nil {
				fmt.Fprintf(out, "删除 %s 失败：%v\n", dir, err)
			} else {
				fmt.Fprintf(out, "已删除：%s\n", dir)
			}
		}
		if opts.User != "" && opts.Root == "/" {
			if err := removeUser(opts, opts.User); err != nil {
				fmt.Fprintf(out, "删除用户失败（可忽略）：%v\n", err)
			} else {
				fmt.Fprintf(out, "已删除系统用户：%s\n", opts.User)
			}
		}
	}
	return nil
}

func checkEnvironment(opts Options) error {
	if opts.Root != "/" {
		return nil
	}
	if os.Geteuid() != 0 && !opts.DryRun {
		return errors.New("需要 root 权限：请用 sudo 运行 install-service")
	}
	if lookPath("systemctl") == "" && !opts.DryRun {
		return errors.New("找不到 systemctl，这台机器不是 systemd 系统（Debian 12/13 才有）")
	}
	if data, err := os.ReadFile("/etc/os-release"); err == nil {
		text := strings.ToLower(string(data))
		if !strings.Contains(text, "debian") && !strings.Contains(text, "ubuntu") {
			fmt.Fprintf(opts.Out, "提示：当前系统不是 Debian/Ubuntu（%s），单元文件可能仍可用，请自行确认\n",
				firstLine(data))
		}
	}
	return nil
}

func verifyUnit(opts Options, unit string) error {
	analyze := lookPath("systemd-analyze")
	if analyze == "" {
		fmt.Fprintln(opts.Out, "提示：没有 systemd-analyze，跳过单元文件校验")
		return nil
	}
	tmp, err := os.CreateTemp("", "filebot-*.service")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.WriteString(unit); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	output, err := opts.runner(analyze, "verify", name)
	if err != nil {
		return fmt.Errorf("systemd-analyze verify 不通过：%w\n%s", err, output)
	}
	return nil
}

func ensureUser(opts Options, name, stateDir string) error {
	if _, err := user.Lookup(name); err == nil {
		return nil
	}
	useradd := lookPath("useradd")
	if useradd == "" {
		return fmt.Errorf("找不到 useradd，无法创建系统用户 %s（可用 --user root 指定已有用户）", name)
	}
	args := []string{
		"--system", "--no-create-home", "--shell", "/usr/sbin/nologin",
		"--home-dir", stateDir, "--comment", "filebot service", name,
	}
	if _, err := opts.runner(useradd, args...); err != nil {
		return fmt.Errorf("创建系统用户 %s 失败：%w", name, err)
	}
	fmt.Fprintf(opts.Out, "已创建系统用户：%s\n", name)
	return nil
}

// prepareStateDir 预先建好状态目录，避免 WorkingDirectory 指向不存在的路径。
func prepareStateDir(opts Options, stateDir string) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("创建状态目录失败：%w", err)
	}
	if opts.User == "" || opts.Root != "/" {
		return nil
	}
	account, err := user.Lookup(opts.User)
	if err != nil {
		return nil
	}
	uid, err1 := strconv.Atoi(account.Uid)
	gid, err2 := strconv.Atoi(account.Gid)
	if err1 != nil || err2 != nil {
		return nil
	}
	if err := os.Chown(stateDir, uid, gid); err != nil {
		return fmt.Errorf("设置状态目录属主失败：%w", err)
	}
	return nil
}

func removeUser(opts Options, name string) error {
	userdel := lookPath("userdel")
	if userdel == "" {
		return errors.New("找不到 userdel")
	}
	_, err := opts.runner(userdel, name)
	return err
}

func runCommand(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func lookPath(name string) string {
	path, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return path
}

func fileChanged(path, content string) (bool, error) {
	existing, err := os.ReadFile(path)
	if err != nil {
		return true, err
	}
	return string(existing) != content, nil
}

func firstLine(data []byte) string {
	text := strings.TrimSpace(string(data))
	if idx := strings.IndexByte(text, '\n'); idx >= 0 {
		text = text[:idx]
	}
	return text
}

func markIf(condition bool, mark string) string {
	if condition {
		return mark
	}
	return ""
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
