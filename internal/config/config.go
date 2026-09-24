// Package config 负责读取配置：TOML 文件、环境变量、命令行覆盖。
//
// 优先级：命令行 > 环境变量 > 配置文件 > 默认值。
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// ChatList 是接收方列表。TOML 里既支持单个也支持多个：
//
//	chat_id = "-1001234567890"            # 数字可以不写引号
//	chat_id = "@mychannel"
//	chat_id = ["-1001234567890", "@other"]
type ChatList []string

// UnmarshalTOML 让 chat_id 同时接受字符串、整数和数组。
func (c *ChatList) UnmarshalTOML(value any) error {
	items, err := toList(value)
	if err != nil {
		return fmt.Errorf("chat_id %w", err)
	}
	for _, item := range items {
		if text := strings.TrimSpace(item); text != "" {
			*c = append(*c, text)
		}
	}
	return nil
}

func toList(value any) ([]string, error) {
	switch typed := value.(type) {
	case nil:
		return nil, nil
	case string:
		return []string{typed}, nil
	case int64:
		return []string{strconv.FormatInt(typed, 10)}, nil
	case float64:
		return []string{strconv.FormatFloat(typed, 'f', -1, 64)}, nil
	case bool:
		return nil, errors.New("必须是字符串或字符串数组")
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			part, err := toList(item)
			if err != nil {
				return nil, err
			}
			out = append(out, part...)
		}
		return out, nil
	case []string:
		return typed, nil
	default:
		return nil, fmt.Errorf("不支持的类型 %T", value)
	}
}

// UnmarshalTOML 让 dirs 也支持单个字符串写法。
type DirsList []string

func (d *DirsList) UnmarshalTOML(value any) error {
	items, err := toList(value)
	if err != nil {
		return fmt.Errorf("watch.dirs %w", err)
	}
	for _, item := range items {
		if text := strings.TrimSpace(item); text != "" {
			*d = append(*d, text)
		}
	}
	return nil
}

// Telegram 是 Bot API 相关配置。
type Telegram struct {
	Token           string   `toml:"token"`
	ChatIDs         ChatList `toml:"chat_id"`
	APIBase         string   `toml:"api_base"`
	Timeout         float64  `toml:"timeout"`           // 秒
	MaxRetries      int      `toml:"max_retries"`       // 单次上传的最大尝试次数
	MinSendInterval float64  `toml:"min_send_interval"` // 两条消息之间的最小间隔（秒）
}

// Proxy 是代理配置，留空表示直连。
type Proxy struct {
	URL string `toml:"url"`
}

// Watch 是目录监控配置。
type Watch struct {
	Dirs          DirsList `toml:"dirs"`
	PollInterval  float64  `toml:"poll_interval"`  // 扫描间隔（秒）
	SettleSeconds float64  `toml:"settle_seconds"` // 文件稳定多久后才发送
	MinSize       int64    `toml:"min_size"`
	Exclude       []string `toml:"exclude"`
	// ScanExisting 只对 `filebot once`（补发历史文件）有意义；
	// 常驻模式只发送启动后新增/变化的文件，会忽略这个开关。
	ScanExisting bool   `toml:"scan_existing"`
	StateFile    string `toml:"state_file"`
}

// Upload 是发送策略配置。
type Upload struct {
	SendOriginal    bool    `toml:"send_original"`
	SendCompressed  bool    `toml:"send_compressed"`
	MaxUploadMB     float64 `toml:"max_upload_mb"`
	PhotoMaxMB      float64 `toml:"photo_max_mb"`
	VideoMaxMB      float64 `toml:"video_max_mb"`
	CaptionTemplate string  `toml:"caption_template"`
	NotifySkipped   bool    `toml:"notify_skipped"`
}

// Config 是完整配置。
type Config struct {
	Telegram Telegram `toml:"telegram"`
	Proxy    Proxy    `toml:"proxy"`
	Watch    Watch    `toml:"watch"`
	Upload   Upload   `toml:"upload"`

	Workers  int    `toml:"workers"`
	LogLevel string `toml:"log_level"`

	// Path 记录配置来源，便于日志与 install-service 复用。
	Path string `toml:"-"`
}

// Default 返回一套可用的默认值。
func Default() Config {
	return Config{
		Telegram: Telegram{
			APIBase:         "https://api.telegram.org",
			Timeout:         120,
			MaxRetries:      3,
			MinSendInterval: 1,
		},
		Watch: Watch{
			PollInterval:  2,
			SettleSeconds: 3,
			MinSize:       1024,
			Exclude:       []string{"*.part", "*.tmp", "*.crdownload", "*.download", "*.partial"},
			StateFile:     "~/.local/state/filebot/state.json",
		},
		Upload: Upload{
			SendOriginal:    true,
			SendCompressed:  true,
			MaxUploadMB:     50,
			PhotoMaxMB:      10,
			VideoMaxMB:      50,
			CaptionTemplate: "{name}\n{size} · {path}",
		},
		Workers:  1,
		LogLevel: "INFO",
	}
}

// MaxUploadBytes 是原文件上传上限。
func (c Config) MaxUploadBytes() int64 { return mbToBytes(c.Upload.MaxUploadMB) }

// PhotoMaxBytes 是图片内联上限。
func (c Config) PhotoMaxBytes() int64 { return mbToBytes(c.Upload.PhotoMaxMB) }

// VideoMaxBytes 是视频内联上限。
func (c Config) VideoMaxBytes() int64 { return mbToBytes(c.Upload.VideoMaxMB) }

func mbToBytes(mb float64) int64 { return int64(mb * 1024 * 1024) }

// Overrides 是命令行覆盖项，nil 表示不覆盖。
type Overrides struct {
	Token           *string
	ChatIDs         []string
	APIBase         *string
	Proxy           *string
	Dirs            []string
	StateFile       *string
	ScanExisting    *bool
	PollInterval    *float64
	SettleSeconds   *float64
	MinSize         *int64
	MaxUploadMB     *float64
	PhotoMaxMB      *float64
	VideoMaxMB      *float64
	Workers         *int
	LogLevel        *string
	SendOriginal    *bool
	SendCompressed  *bool
	CaptionTemplate *string
}

// Load 读取配置。path 为空时只用默认值 + 环境变量 + 覆盖项。
func Load(path string, ov Overrides) (Config, error) {
	cfg := Default()
	if path != "" {
		expanded, err := ExpandPath(path)
		if err != nil {
			return cfg, err
		}
		if _, err := os.Stat(expanded); err != nil {
			return cfg, fmt.Errorf("配置文件不可用：%w", err)
		}
		meta, err := toml.DecodeFile(expanded, &cfg)
		if err != nil {
			return cfg, fmt.Errorf("解析配置文件失败：%w", err)
		}
		if unknown := meta.Undecoded(); len(unknown) > 0 {
			keys := make([]string, 0, len(unknown))
			for _, key := range unknown {
				keys = append(keys, key.String())
			}
			return cfg, fmt.Errorf("配置文件里有无法识别的配置项：%s", strings.Join(keys, ", "))
		}
		cfg.Path = expanded
	}

	applyEnv(&cfg)
	applyOverrides(&cfg, ov)

	if err := normalize(&cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Validate 检查必填项。
func (c Config) Validate() error {
	var problems []string
	if strings.TrimSpace(c.Telegram.Token) == "" {
		problems = append(problems, "缺少 telegram.token（或环境变量 FILEBOT_TG_TOKEN，或 --token）")
	}
	if len(c.Telegram.ChatIDs) == 0 {
		problems = append(problems, "缺少 telegram.chat_id（或环境变量 FILEBOT_TG_CHAT_ID，或 --chat-id）")
	}
	if len(c.Watch.Dirs) == 0 {
		problems = append(problems, "缺少 watch.dirs（或环境变量 FILEBOT_WATCH_DIRS，或 -d/--dir）")
	}
	if c.Upload.MaxUploadMB <= 0 {
		problems = append(problems, "upload.max_upload_mb 必须大于 0")
	}
	if c.Telegram.MaxRetries < 1 {
		problems = append(problems, "telegram.max_retries 必须大于 0")
	}
	if c.Workers < 1 {
		problems = append(problems, "workers 必须大于 0")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "；"))
	}
	return nil
}

// MissingDirs 返回尚不存在的监控目录（挂载晚到时不致命，只提示）。
func (c Config) MissingDirs() []string {
	var missing []string
	for _, dir := range c.Watch.Dirs {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			missing = append(missing, dir)
		}
	}
	return missing
}

// ExpandPath 展开 ~ 前缀并转成绝对路径。
func ExpandPath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("无法确定家目录：%w", err)
		}
		path = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
	}
	return filepath.Abs(path)
}

func normalize(cfg *Config) error {
	dirs := make(DirsList, 0, len(cfg.Watch.Dirs))
	for _, dir := range cfg.Watch.Dirs {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		expanded, err := ExpandPath(dir)
		if err != nil {
			return err
		}
		dirs = append(dirs, expanded)
	}
	cfg.Watch.Dirs = dirs

	if cfg.Watch.StateFile != "" {
		expanded, err := ExpandPath(cfg.Watch.StateFile)
		if err != nil {
			return err
		}
		cfg.Watch.StateFile = expanded
	}
	cfg.Telegram.Token = strings.TrimSpace(cfg.Telegram.Token)
	cfg.Telegram.ChatIDs = dedupeStrings(splitList(strings.Join(cfg.Telegram.ChatIDs, ",")))
	cfg.Telegram.APIBase = strings.TrimRight(strings.TrimSpace(cfg.Telegram.APIBase), "/")
	cfg.Proxy.URL = strings.TrimSpace(cfg.Proxy.URL)
	cfg.LogLevel = strings.ToUpper(strings.TrimSpace(cfg.LogLevel))
	if cfg.LogLevel == "" {
		cfg.LogLevel = "INFO"
	}
	return nil
}

func applyEnv(cfg *Config) {
	if value := os.Getenv("FILEBOT_TG_TOKEN"); value != "" {
		cfg.Telegram.Token = value
	}
	if value := os.Getenv("FILEBOT_TG_CHAT_ID"); value != "" {
		cfg.Telegram.ChatIDs = splitList(value)
	}
	if value := os.Getenv("FILEBOT_TG_API_BASE"); value != "" {
		cfg.Telegram.APIBase = value
	}
	if value := os.Getenv("FILEBOT_PROXY"); value != "" {
		cfg.Proxy.URL = value
	}
	if value := os.Getenv("FILEBOT_WATCH_DIRS"); value != "" {
		var dirs []string
		for _, item := range strings.Split(value, string(os.PathListSeparator)) {
			if strings.TrimSpace(item) != "" {
				dirs = append(dirs, strings.TrimSpace(item))
			}
		}
		cfg.Watch.Dirs = dirs
	}
	if value := os.Getenv("FILEBOT_STATE_FILE"); value != "" {
		cfg.Watch.StateFile = value
	}
	if value := os.Getenv("FILEBOT_SCAN_EXISTING"); value != "" {
		if parsed, err := strconv.ParseBool(value); err == nil {
			cfg.Watch.ScanExisting = parsed
		}
	}
	if value := os.Getenv("FILEBOT_LOG_LEVEL"); value != "" {
		cfg.LogLevel = value
	}
}

func applyOverrides(cfg *Config, ov Overrides) {
	if ov.Token != nil {
		cfg.Telegram.Token = *ov.Token
	}
	if len(ov.ChatIDs) > 0 {
		cfg.Telegram.ChatIDs = dedupeStrings(ov.ChatIDs)
	}
	if ov.APIBase != nil {
		cfg.Telegram.APIBase = *ov.APIBase
	}
	if ov.Proxy != nil {
		cfg.Proxy.URL = *ov.Proxy
	}
	if len(ov.Dirs) > 0 {
		cfg.Watch.Dirs = ov.Dirs
	}
	if ov.StateFile != nil {
		cfg.Watch.StateFile = *ov.StateFile
	}
	if ov.ScanExisting != nil {
		cfg.Watch.ScanExisting = *ov.ScanExisting
	}
	if ov.PollInterval != nil {
		cfg.Watch.PollInterval = *ov.PollInterval
	}
	if ov.SettleSeconds != nil {
		cfg.Watch.SettleSeconds = *ov.SettleSeconds
	}
	if ov.MinSize != nil {
		cfg.Watch.MinSize = *ov.MinSize
	}
	if ov.MaxUploadMB != nil {
		cfg.Upload.MaxUploadMB = *ov.MaxUploadMB
	}
	if ov.PhotoMaxMB != nil {
		cfg.Upload.PhotoMaxMB = *ov.PhotoMaxMB
	}
	if ov.VideoMaxMB != nil {
		cfg.Upload.VideoMaxMB = *ov.VideoMaxMB
	}
	if ov.Workers != nil {
		cfg.Workers = *ov.Workers
	}
	if ov.LogLevel != nil {
		cfg.LogLevel = *ov.LogLevel
	}
	if ov.SendOriginal != nil {
		cfg.Upload.SendOriginal = *ov.SendOriginal
	}
	if ov.SendCompressed != nil {
		cfg.Upload.SendCompressed = *ov.SendCompressed
	}
	if ov.CaptionTemplate != nil {
		cfg.Upload.CaptionTemplate = *ov.CaptionTemplate
	}
}

// splitList 把逗号/分号/空白分隔的字符串拆成列表（每一项都再 trim）。
func splitList(value string) []string {
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\t' || r == ' '
	})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if text := strings.TrimSpace(part); text != "" {
			out = append(out, text)
		}
	}
	return out
}

// dedupeStrings 去重并保持顺序。
func dedupeStrings(items []string) []string {
	seen := make(map[string]bool, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		text := strings.TrimSpace(item)
		if text == "" || seen[text] {
			continue
		}
		seen[text] = true
		out = append(out, text)
	}
	return out
}
