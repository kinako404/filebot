package watch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"filebot/internal/config"
)

type harness struct {
	t    *testing.T
	root string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return &harness{t: t, root: t.TempDir()}
}

func (h *harness) write(relative string, size int) string {
	h.t.Helper()
	path := filepath.Join(h.root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		h.t.Fatal(err)
	}
	return path
}

func (h *harness) watcher(overrides func(*config.Watch)) *Watcher {
	h.t.Helper()
	cfg := config.Watch{
		Dirs:          []string{h.root},
		PollInterval:  0.01,
		SettleSeconds: 3,
	}
	if overrides != nil {
		overrides(&cfg)
	}
	return New(cfg)
}

func paths(files []File) []string {
	out := make([]string, 0, len(files))
	for _, file := range files {
		out = append(out, file.Path)
	}
	return out
}

func TestNewFileInSubdirectoryNeedsSettle(t *testing.T) {
	h := newHarness(t)
	w := h.watcher(nil)
	w.Poll(time.Unix(0, 0))
	path := h.write("sub/dir/clip.mp4", 2048)

	if got := w.Poll(time.Unix(1, 0)); len(got) != 0 {
		t.Fatalf("稳定前不应产出：%v", paths(got))
	}
	ready := w.Poll(time.Unix(10, 0))
	if len(ready) != 1 || ready[0].Path != path || ready[0].Size != 2048 {
		t.Fatalf("ready = %+v", ready)
	}
}

func TestGrowingFileIsNotEmitted(t *testing.T) {
	h := newHarness(t)
	w := h.watcher(nil)
	w.Poll(time.Unix(0, 0))
	path := h.write("growing.mp4", 100)

	// 每秒都在变大（settle 是 3 秒）：计时器必须每次重置
	w.Poll(time.Unix(1, 0))
	if err := os.WriteFile(path, make([]byte, 200), 0o644); err != nil {
		t.Fatal(err)
	}
	w.Poll(time.Unix(2, 0))
	if err := os.WriteFile(path, make([]byte, 300), 0o644); err != nil {
		t.Fatal(err)
	}
	w.Poll(time.Unix(3, 0))

	// 最后一次变化在 t=3；如果计时器没被重置，t=4 就会误判为稳定
	if got := w.Poll(time.Unix(4, 0)); len(got) != 0 {
		t.Fatalf("拷贝中不应产出：%v", paths(got))
	}
	if got := w.Poll(time.Unix(5, 0)); len(got) != 0 {
		t.Fatalf("拷贝中不应产出：%v", paths(got))
	}
	got := w.Poll(time.Unix(6, 0))
	if len(got) != 1 || got[0].Size != 300 {
		t.Fatalf("稳定后应当产出：%+v", got)
	}
}

func TestModifiedFileIsEmittedAgain(t *testing.T) {
	h := newHarness(t)
	w := h.watcher(nil)
	w.Poll(time.Unix(0, 0))
	path := h.write("a.jpg", 2048)
	w.Poll(time.Unix(1, 0))
	if len(w.Poll(time.Unix(10, 0))) != 1 {
		t.Fatal("第一次应当产出")
	}
	if err := os.WriteFile(path, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := w.Poll(time.Unix(11, 0)); len(got) != 0 {
		t.Fatalf("修改后要先重新计时：%v", paths(got))
	}
	if len(w.Poll(time.Unix(20, 0))) != 1 {
		t.Fatal("修改后应当再次产出")
	}
}

func TestDeletedFileIsDropped(t *testing.T) {
	h := newHarness(t)
	w := h.watcher(nil)
	w.Poll(time.Unix(0, 0))
	path := h.write("temp.mp4", 2048)
	w.Poll(time.Unix(1, 0))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := w.Poll(time.Unix(10, 0)); len(got) != 0 {
		t.Fatalf("删除后不应产出：%v", paths(got))
	}
}

func TestScanExistingBehaviour(t *testing.T) {
	h := newHarness(t)
	path := h.write("old.jpg", 2048)

	// 默认：启动前就存在的文件不发送
	w := h.watcher(nil)
	if got := w.Poll(time.Unix(0, 0)); len(got) != 0 {
		t.Fatalf("默认不应产出：%v", paths(got))
	}
	if got := w.Poll(time.Unix(100, 0)); len(got) != 0 {
		t.Fatalf("默认不应产出：%v", paths(got))
	}

	// scan_existing：等稳定后发送
	w2 := h.watcher(func(c *config.Watch) { c.ScanExisting = true })
	if got := w2.Poll(time.Unix(0, 0)); len(got) != 0 {
		t.Fatalf("首次不应立即产出：%v", paths(got))
	}
	got := w2.Poll(time.Unix(10, 0))
	if len(got) != 1 || got[0].Path != path {
		t.Fatalf("got = %v", paths(got))
	}
}

func TestScanExistingWithZeroSettleEmitsImmediately(t *testing.T) {
	h := newHarness(t)
	path := h.write("old.jpg", 2048)
	w := h.watcher(func(c *config.Watch) { c.ScanExisting = true; c.SettleSeconds = 0 })
	got := w.Poll(time.Unix(0, 0))
	if len(got) != 1 || got[0].Path != path {
		t.Fatalf("got = %v", paths(got))
	}
}

func TestHiddenAndTempFilesAreIgnored(t *testing.T) {
	h := newHarness(t)
	w := h.watcher(nil)
	w.Poll(time.Unix(0, 0))
	h.write(".hidden.jpg", 2048)
	h.write("movie.mp4.part", 2048)
	h.write("movie.mp4.crdownload", 2048)
	h.write("draft.jpg~", 2048)
	h.write(".cache/x.jpg", 2048)
	if got := w.Poll(time.Unix(10, 0)); len(got) != 0 {
		t.Fatalf("应全部忽略：%v", paths(got))
	}
}

func TestMinSizeFilter(t *testing.T) {
	h := newHarness(t)
	w := h.watcher(func(c *config.Watch) { c.MinSize = 1024 })
	w.Poll(time.Unix(0, 0))
	h.write("tiny.jpg", 100)
	big := h.write("ok.jpg", 5000)
	w.Poll(time.Unix(1, 0))
	got := w.Poll(time.Unix(10, 0))
	if len(got) != 1 || got[0].Path != big {
		t.Fatalf("got = %v", paths(got))
	}
}

func TestExcludePatterns(t *testing.T) {
	h := newHarness(t)
	w := h.watcher(func(c *config.Watch) { c.Exclude = []string{"*.tmp", "*thumbnail*"} })
	w.Poll(time.Unix(0, 0))
	h.write("a.tmp", 2048)
	h.write("thumbnail_1.jpg", 2048)
	keep := h.write("real.jpg", 2048)
	w.Poll(time.Unix(1, 0))
	got := w.Poll(time.Unix(10, 0))
	if len(got) != 1 || got[0].Path != keep {
		t.Fatalf("got = %v", paths(got))
	}
}

func TestRunProducesFiles(t *testing.T) {
	h := newHarness(t)
	w := h.watcher(func(c *config.Watch) {
		c.SettleSeconds = 0.05
		c.PollInterval = 0.05
		c.ScanExisting = true
	})
	h.write("live.jpg", 2048)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got := make(chan File, 4)
	go w.Run(ctx, func(files []File) {
		for _, file := range files {
			got <- file
		}
	})

	select {
	case file := <-got:
		if filepath.Base(file.Path) != "live.jpg" {
			t.Fatalf("file = %+v", file)
		}
	case <-ctx.Done():
		t.Fatal("超时：常驻模式没有产出文件")
	}
}

func TestStat(t *testing.T) {
	h := newHarness(t)
	path := h.write("a.jpg", 42)
	mtime, size, ok := Stat(path)
	if !ok || size != 42 || mtime == 0 {
		t.Fatalf("stat = %d %d %v", mtime, size, ok)
	}
	if _, _, ok := Stat(filepath.Join(h.root, "nope.jpg")); ok {
		t.Fatal("不存在的文件应当 ok=false")
	}
}
