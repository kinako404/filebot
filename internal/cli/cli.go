// Package cli 提供命令行入口与子命令。
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"filebot/internal/app"
	"filebot/internal/config"
	"filebot/internal/service"
)

// Version 是构建版本号，构建时可用 -ldflags 覆盖。
var Version = "2.1.2"

const usage = `filebot - 监控目录，把新增/更新的图片、视频发送到 Telegram

用法：
  filebot [run] [选项]              常驻监控（默认命令）
  filebot check [选项]              只做连接自检（token / 代理 / chat_id）
  filebot once [选项]               把目录里现有文件发一遍后退出（补发历史文件用）
  filebot install-service [选项]    生成并安装 systemd 服务（Debian 12/13）
  filebot uninstall-service [选项]  卸载 systemd 服务
  filebot version                   显示版本

常用选项：
  -c, --config PATH                 配置文件（TOML）
      --token TOKEN                 Bot token
      --chat-id ID                  接收方 chat_id，可重复或用逗号分隔
      --proxy URL                   socks5:// 或 http:// 代理，留空直连
  -d, --dir PATH                    监控目录，可重复
      --settle-seconds N            文件稳定 N 秒后才发送（默认 3）
      --max-upload-mb N             原文件上传上限（默认 50）
      --no-original                 不发送原文件
      --no-compressed               不发送可点开的版本
      --log-level LEVEL             DEBUG/INFO/WARN/ERROR

常驻模式只发送启动之后新增/变化的文件；要补发目录里已有的文件用 filebot once。

环境变量：FILEBOT_TG_TOKEN、FILEBOT_TG_CHAT_ID（多个用逗号分隔）、FILEBOT_PROXY、
          FILEBOT_WATCH_DIRS（冒号分隔）、FILEBOT_TG_API_BASE、
          FILEBOT_STATE_FILE、FILEBOT_LOG_LEVEL

示例：
  filebot -c /etc/filebot/config.toml
  sudo filebot install-service -c /etc/filebot/config.toml
  filebot once -d /data/media --token 123:abc --chat-id @mychannel
  filebot -c config.toml --chat-id @a --chat-id -1001234567890
`

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

// Main 是进程入口，返回退出码。
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "help", "-h", "--help":
			fmt.Fprint(stdout, usage)
			return 0
		case "version", "-v", "--version":
			fmt.Fprintf(stdout, "filebot %s\n", Version)
			return 0
		case "install-service", "install":
			return runInstallService(args[1:], stdout, stderr)
		case "uninstall-service", "uninstall":
			return runUninstallService(args[1:], stdout, stderr)
		case "run", "once", "check":
			return runBot(args[0], args[1:], stdout, stderr)
		}
	}
	return runBot("run", args, stdout, stderr)
}

// ------------------------------------------------------------------ //
// run / once / check
// ------------------------------------------------------------------ //

type botFlags struct {
	configPath string
	token      string
	chatIDs    stringList
	apiBase    string
	proxyURL   string
	dirs       stringList
	stateFile  string

	scanExisting  bool
	pollInterval  float64
	settleSeconds float64
	minSize       int64
	maxUploadMB   float64
	photoMaxMB    float64
	videoMaxMB    float64
	workers       int
	logLevel      string
	noOriginal    bool
	noCompressed  bool
	captionTempl  string
}

func (f *botFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&f.configPath, "c", "", "配置文件路径")
	fs.StringVar(&f.configPath, "config", "", "配置文件路径")
	fs.StringVar(&f.token, "token", "", "Bot token")
	fs.Var(&f.chatIDs, "chat-id", "接收方 chat_id，可重复或用逗号分隔")
	fs.StringVar(&f.apiBase, "api-base", "", "Bot API 地址")
	fs.StringVar(&f.proxyURL, "proxy", "", "socks5:// 或 http:// 代理")
	fs.Var(&f.dirs, "d", "监控目录，可重复")
	fs.Var(&f.dirs, "dir", "监控目录，可重复")
	fs.StringVar(&f.stateFile, "state-file", "", "发送记录文件")
	fs.Float64Var(&f.pollInterval, "poll-interval", 0, "扫描间隔秒数")
	fs.Float64Var(&f.settleSeconds, "settle-seconds", 0, "文件稳定多少秒后发送")
	fs.Int64Var(&f.minSize, "min-size", 0, "小于该字节数的文件忽略")
	fs.Float64Var(&f.maxUploadMB, "max-upload-mb", 0, "原文件上传上限 MB")
	fs.Float64Var(&f.photoMaxMB, "photo-max-mb", 0, "图片内联上限 MB")
	fs.Float64Var(&f.videoMaxMB, "video-max-mb", 0, "视频内联上限 MB")
	fs.IntVar(&f.workers, "workers", 0, "发送线程数")
	fs.StringVar(&f.logLevel, "log-level", "", "日志级别")
	fs.BoolVar(&f.noOriginal, "no-original", false, "不发送原文件")
	fs.BoolVar(&f.noCompressed, "no-compressed", false, "不发送可点开的版本")
	fs.StringVar(&f.captionTempl, "caption", "", "消息标题模板")
}

func (f *botFlags) overrides(fs *flag.FlagSet) config.Overrides {
	set := map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	ov := config.Overrides{}
	if set["token"] {
		ov.Token = &f.token
	}
	if set["chat-id"] {
		for _, item := range f.chatIDs {
			// 支持 --chat-id "a,b" 与 --chat-id a --chat-id b
			for _, piece := range strings.FieldsFunc(item, func(r rune) bool {
				return r == ',' || r == ';'
			}) {
				if text := strings.TrimSpace(piece); text != "" {
					ov.ChatIDs = append(ov.ChatIDs, text)
				}
			}
		}
	}
	if set["api-base"] {
		ov.APIBase = &f.apiBase
	}
	if set["proxy"] {
		ov.Proxy = &f.proxyURL
	}
	ov.Dirs = f.dirs
	if set["state-file"] {
		ov.StateFile = &f.stateFile
	}
	if set["poll-interval"] {
		ov.PollInterval = &f.pollInterval
	}
	if set["settle-seconds"] {
		ov.SettleSeconds = &f.settleSeconds
	}
	if set["min-size"] {
		ov.MinSize = &f.minSize
	}
	if set["max-upload-mb"] {
		ov.MaxUploadMB = &f.maxUploadMB
	}
	if set["photo-max-mb"] {
		ov.PhotoMaxMB = &f.photoMaxMB
	}
	if set["video-max-mb"] {
		ov.VideoMaxMB = &f.videoMaxMB
	}
	if set["workers"] {
		ov.Workers = &f.workers
	}
	if set["log-level"] {
		ov.LogLevel = &f.logLevel
	}
	if set["caption"] {
		ov.CaptionTemplate = &f.captionTempl
	}
	if f.noOriginal {
		off := false
		ov.SendOriginal = &off
	}
	if f.noCompressed {
		off := false
		ov.SendCompressed = &off
	}
	return ov
}

func runBot(command string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, usage)
	}
	flags := &botFlags{}
	flags.bind(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	cfg, err := config.Load(flags.configPath, flags.overrides(fs))
	if err != nil {
		fmt.Fprintf(stderr, "配置错误：%v\n", err)
		return 2
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(stderr, "配置错误：%v\n", err)
		return 2
	}

	logger := newLogger(stderr, cfg.LogLevel)
	slog.SetDefault(logger)
	logger.Info("接收方", "chat_ids", strings.Join(cfg.Telegram.ChatIDs, ", "))

	for _, dir := range cfg.MissingDirs() {
		logger.Warn("监控目录不存在（挂载好之后会自动生效）", "dir", dir)
	}

	runtime, err := app.New(cfg, logger)
	if err != nil {
		fmt.Fprintf(stderr, "初始化失败：%v\n", err)
		return 1
	}
	defer runtime.Close()

	if command == "check" {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := runtime.Check(ctx); err != nil {
			fmt.Fprintf(stderr, "自检失败：%v\n", err)
			return 1
		}
		return 0
	}

	if command == "once" {
		ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
		defer cancel()
		if err := runtime.RunOnce(ctx); err != nil {
			fmt.Fprintf(stderr, "执行失败：%v\n", err)
			return 1
		}
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runtime.Run(ctx); err != nil {
		fmt.Fprintf(stderr, "运行失败：%v\n", err)
		return 1
	}
	return 0
}

func newLogger(w io.Writer, level string) *slog.Logger {
	var parsed slog.Level
	switch strings.ToUpper(level) {
	case "DEBUG":
		parsed = slog.LevelDebug
	case "WARN", "WARNING":
		parsed = slog.LevelWarn
	case "ERROR":
		parsed = slog.LevelError
	default:
		parsed = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: parsed}))
}

// ------------------------------------------------------------------ //
// install-service / uninstall-service
// ------------------------------------------------------------------ //

func runInstallService(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("install-service", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		opts     = service.Options{}
		noStart  bool
		noEnable bool
		unitName string
		userName string
		stateDir string
	)
	fs.StringVar(&opts.ConfigPath, "c", "", "配置文件路径")
	fs.StringVar(&opts.ConfigPath, "config", "", "配置文件路径")
	fs.StringVar(&opts.BinPath, "bin", "", "二进制路径（默认当前可执行文件）")
	fs.StringVar(&unitName, "name", service.DefaultUnitName, "服务名")
	fs.StringVar(&userName, "user", service.DefaultUser, "运行用户（root 表示不创建新用户）")
	fs.StringVar(&stateDir, "state-dir", "", "状态目录（默认 /var/lib/<name>）")
	fs.StringVar(&opts.Root, "root", "/", "安装根目录（离线镜像/测试用）")
	fs.BoolVar(&opts.DryRun, "dry-run", false, "只打印将要执行的操作，不写任何文件")
	fs.BoolVar(&opts.Force, "force", false, "跳过“已覆盖已有单元文件”的提示")
	fs.BoolVar(&noStart, "no-start", false, "不启动服务")
	fs.BoolVar(&noEnable, "no-enable", false, "不设置开机自启")
	fs.Usage = func() {
		fmt.Fprint(stderr, `用法：filebot install-service [选项]

自动为当前环境补全 systemd 服务（Debian 12/13）：生成配置模板、创建系统用户、
写入 /etc/systemd/system/filebot.service 并 enable/start。

选项：
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	explicit := map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { explicit[fl.Name] = true })

	opts.UnitName = unitName
	if strings.EqualFold(userName, "root") || userName == "" {
		opts.User = ""
	} else {
		opts.User = userName
	}
	// 默认状态目录由 service 包按 --root 计算（root 非 / 时会加上前缀）
	opts.StateDir = stateDir
	// 非 "/" 的 root 是离线镜像场景，默认不碰宿主机的 systemd；
	// 确需操作时显式写 --no-start=false / --no-enable=false
	opts.Start = !noStart
	opts.Enable = !noEnable
	if opts.Root != "/" {
		if !explicit["no-start"] {
			opts.Start = false
		}
		if !explicit["no-enable"] {
			opts.Enable = false
		}
	}
	opts.Out = stdout

	if err := service.Install(opts); err != nil {
		fmt.Fprintf(stderr, "安装服务失败：%v\n", err)
		return 1
	}
	return 0
}

func runUninstallService(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("uninstall-service", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		opts     = service.Options{}
		unitName string
		userName string
		stateDir string
		purge    bool
	)
	fs.StringVar(&opts.ConfigPath, "c", "", "配置文件路径")
	fs.StringVar(&opts.ConfigPath, "config", "", "配置文件路径")
	fs.StringVar(&unitName, "name", service.DefaultUnitName, "服务名")
	fs.StringVar(&userName, "user", service.DefaultUser, "运行用户")
	fs.StringVar(&stateDir, "state-dir", "", "状态目录")
	fs.StringVar(&opts.Root, "root", "/", "安装根目录（离线镜像/测试用）")
	fs.BoolVar(&opts.DryRun, "dry-run", false, "只打印将要执行的操作")
	fs.BoolVar(&purge, "purge", false, "同时删除配置、状态目录与系统用户")
	fs.Usage = func() {
		fmt.Fprint(stderr, `用法：filebot uninstall-service [选项]

停止并移除 systemd 服务；--purge 会额外删除配置、状态目录和系统用户。

选项：
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	opts.UnitName = unitName
	if userName == "" || strings.EqualFold(userName, "root") {
		opts.User = ""
	} else {
		opts.User = userName
	}
	// 默认状态目录由 service 包按 --root 计算（root 非 / 时会加上前缀）
	opts.StateDir = stateDir
	opts.Out = stdout

	if err := service.Uninstall(opts, purge); err != nil {
		fmt.Fprintf(stderr, "卸载服务失败：%v\n", err)
		return 1
	}
	return 0
}
