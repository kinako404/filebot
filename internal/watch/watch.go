// Package watch 递归监控目录，用轮询 + 去抖找出"稳定下来"的新文件。
//
// 不用 inotify：轮询对"正在拷贝的大文件"、网络挂载、容器绑定挂载都更稳，
// 而且没有平台依赖。
package watch

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"filebot/internal/config"
)

// minPollInterval 是轮询间隔的下限（0/负值会变成忙循环）。
const minPollInterval = 100 * time.Millisecond

var tempSuffixes = []string{
	".part", ".tmp", ".crdownload", ".download", ".partial", ".filepart", ".swp",
}

// File 是待发送的文件快照。
type File struct {
	Path  string
	MTime int64
	Size  int64
}

type signature struct {
	mtime int64
	size  int64
}

type pending struct {
	sig    signature
	seenAt time.Time
}

// Watcher 轮询目录并产出稳定文件。
type Watcher struct {
	dirs         []string
	pollInterval time.Duration
	settle       time.Duration
	minSize      int64
	excludes     []string
	scanExisting bool

	known   map[string]signature
	pending map[string]pending
}

// New 按配置创建 watcher。
func New(cfg config.Watch) *Watcher {
	// poll_interval 必须钳到合理下限：0/负值会让 timer.Reset 立刻到期，
	// 变成"全速递归扫盘"的忙循环
	poll := time.Duration(cfg.PollInterval * float64(time.Second))
	if poll < minPollInterval {
		poll = minPollInterval
	}
	settle := time.Duration(cfg.SettleSeconds * float64(time.Second))
	if settle < 0 {
		settle = 0
	}
	return &Watcher{
		dirs:         cfg.Dirs,
		pollInterval: poll,
		settle:       settle,
		minSize:      cfg.MinSize,
		excludes:     cfg.Exclude,
		scanExisting: cfg.ScanExisting,
		pending:      map[string]pending{},
	}
}

// Snapshot 递归扫描，返回 路径 → (mtime,size)。
func (w *Watcher) Snapshot() map[string]signature {
	found := map[string]signature{}
	for _, root := range w.dirs {
		// /media -> /mnt/disk/media 这类符号链接目录很常见；WalkDir 用 Lstat 看根条目，
		// 不解析的话会把整个目录当成"非目录"直接跳过（静默监控不到任何东西）
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			root = resolved
		}
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			name := d.Name()
			if d.IsDir() {
				if path != root && (isHidden(name) || shouldSkip(name)) {
					return fs.SkipDir
				}
				return nil
			}
			if shouldSkip(name) || w.matchesExclude(path, name) {
				return nil
			}
			info, err := d.Info()
			if err != nil || !info.Mode().IsRegular() {
				// 可能是符号链接/dev/fifo：跟随一次，指向普通文件才算数（与 Python 版一致）
				target, statErr := os.Stat(path)
				if statErr != nil || !target.Mode().IsRegular() {
					return nil
				}
				info = target
			}
			found[path] = signature{mtime: info.ModTime().UnixNano(), size: info.Size()}
			return nil
		})
	}
	return found
}

// Poll 扫描一次，返回已经稳定、可以发送的文件。
func (w *Watcher) Poll(now time.Time) []File {
	current := w.Snapshot()
	if w.known == nil {
		w.known = map[string]signature{}
		if !w.scanExisting {
			w.known = current
		}
	}
	for path, sig := range current {
		if old, ok := w.known[path]; !ok || old != sig {
			w.pending[path] = pending{sig: sig, seenAt: now}
		}
	}
	w.known = current

	ready := make([]File, 0, len(w.pending))
	for path, item := range w.pending {
		sig, still := current[path]
		if !still {
			delete(w.pending, path) // 被删除或改名
			continue
		}
		if sig != item.sig {
			continue // 本轮已经重新计时
		}
		if now.Sub(item.seenAt) < w.settle {
			continue
		}
		delete(w.pending, path)
		if sig.size < w.minSize {
			continue
		}
		ready = append(ready, File{Path: path, MTime: sig.mtime, Size: sig.size})
	}
	return ready
}

// Pending 返回还在去抖窗口里、尚未产出的文件路径。
func (w *Watcher) Pending() []string {
	paths := make([]string, 0, len(w.pending))
	for path := range w.pending {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// Run 持续轮询直到 ctx 结束。
func (w *Watcher) Run(ctx context.Context, onFiles func([]File)) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if files := w.Poll(time.Now()); len(files) > 0 {
			onFiles(files)
		}
		timer.Reset(w.pollInterval)
	}
}

func (w *Watcher) matchesExclude(path, name string) bool {
	for _, pattern := range w.excludes {
		if ok, _ := filepath.Match(pattern, name); ok {
			return true
		}
		if ok, _ := filepath.Match(pattern, path); ok {
			return true
		}
	}
	return false
}

func isHidden(name string) bool { return strings.HasPrefix(name, ".") }

func shouldSkip(name string) bool {
	if isHidden(name) || strings.HasSuffix(name, "~") {
		return true
	}
	lower := strings.ToLower(name)
	for _, suffix := range tempSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

// ScanOnce 递归列出目录下的文件（供 --once 使用）。
func ScanOnce(dirs []string, excludes []string) map[string]signature {
	w := &Watcher{dirs: dirs, excludes: excludes}
	return w.Snapshot()
}

// Stat 返回文件的 (mtime,size)；失败时 ok 为 false。
func Stat(path string) (int64, int64, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return 0, 0, false
	}
	return info.ModTime().UnixNano(), info.Size(), true
}
