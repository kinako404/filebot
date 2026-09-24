package media

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// stubTool 写一个假的转换工具（把输入复制到输出），用来验证调用链路。
func stubTool(t *testing.T, dir, name string) string {
	t.Helper()
	script := `#!/bin/sh
in=""
out=""
first="$1"
while [ $# -gt 0 ]; do
  if [ "$1" = "-i" ]; then in="$2"; shift 2; continue; fi
  out="$1"; shift
done
if [ -z "$in" ]; then in="$first"; fi
cp "$in" "$out"
`
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFile(t *testing.T, dir, name string, size int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func newPreparer(t *testing.T, photoMax, videoMax int64, ffmpeg, magick string) *Preparer {
	t.Helper()
	p, err := NewPreparer(photoMax, videoMax, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p.ffmpeg = ffmpeg
	p.magick = magick
	t.Cleanup(func() { p.Close() })
	return p
}

func TestClassify(t *testing.T) {
	cases := map[string]Kind{
		"/a/b.JPG":  KindImage,
		"/a/b.png":  KindImage,
		"/a/b.heic": KindImage,
		"/a/b.mp4":  KindVideo,
		"/a/b.MKV":  KindVideo,
		"/a/b.txt":  "",
		"/a/b":      "",
	}
	for path, want := range cases {
		if got := Classify(path); got != want {
			t.Errorf("Classify(%q) = %q，期望 %q", path, got, want)
		}
	}
}

func TestPassthrough(t *testing.T) {
	dir := t.TempDir()
	p := newPreparer(t, 10<<20, 50<<20, "", "")

	jpeg := writeFile(t, dir, "photo.jpg", 2048)
	got, err := p.Prepare(jpeg, KindImage)
	if err != nil || got.Kind != InlinePhoto || got.Temp || got.Path != jpeg {
		t.Fatalf("jpg: %+v err=%v", got, err)
	}

	png := writeFile(t, dir, "shot.png", 1000)
	if got, err = p.Prepare(png, KindImage); err != nil || got.Kind != InlinePhoto || got.Temp {
		t.Fatalf("png: %+v err=%v", got, err)
	}

	gif := writeFile(t, dir, "anim.gif", 1000)
	if got, err = p.Prepare(gif, KindImage); err != nil || got.Kind != InlineAnimation || got.Temp {
		t.Fatalf("gif: %+v err=%v", got, err)
	}

	mp4 := writeFile(t, dir, "clip.mp4", 5000)
	if got, err = p.Prepare(mp4, KindVideo); err != nil || got.Kind != InlineVideo || got.Temp {
		t.Fatalf("mp4: %+v err=%v", got, err)
	}
}

func TestNoConverterFallsBack(t *testing.T) {
	dir := t.TempDir()
	p := newPreparer(t, 10<<20, 50<<20, "", "")

	if _, err := p.Prepare(writeFile(t, dir, "a.bmp", 2048), KindImage); !errors.Is(err, ErrNoConverter) {
		t.Fatalf("bmp 应当返回 ErrNoConverter，实际 %v", err)
	}
	if _, err := p.Prepare(writeFile(t, dir, "a.mkv", 2048), KindVideo); !errors.Is(err, ErrNoConverter) {
		t.Fatalf("mkv 应当返回 ErrNoConverter，实际 %v", err)
	}
	// 超过图片上限的 jpg 也要转换（没有工具时同样返回 ErrNoConverter）
	small := newPreparer(t, 1000, 50<<20, "", "")
	if _, err := small.Prepare(writeFile(t, dir, "big.jpg", 4096), KindImage); !errors.Is(err, ErrNoConverter) {
		t.Fatalf("超限 jpg 应当返回 ErrNoConverter，实际 %v", err)
	}
}

func TestImageConvertedWithFFmpeg(t *testing.T) {
	dir := t.TempDir()
	stubDir := t.TempDir()
	ffmpeg := stubTool(t, stubDir, "ffmpeg")
	p := newPreparer(t, 10<<20, 50<<20, ffmpeg, "")

	source := writeFile(t, dir, "scan.bmp", 3000)
	got, err := p.Prepare(source, KindImage)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != InlinePhoto || !got.Temp || got.Note != "ffmpeg" {
		t.Fatalf("got = %+v", got)
	}
	if filepath.Ext(got.Path) != ".jpg" {
		t.Fatalf("扩展名 = %q", filepath.Ext(got.Path))
	}
	if data, err := os.ReadFile(got.Path); err != nil || len(data) != 3000 {
		t.Fatalf("转换结果不对：%d %v", len(data), err)
	}
	got.Cleanup()
	if _, err := os.Stat(got.Path); !os.IsNotExist(err) {
		t.Fatal("Cleanup 之后临时文件应当删除")
	}
}

func TestMagickUsedWhenNoFFmpeg(t *testing.T) {
	dir := t.TempDir()
	stubDir := t.TempDir()
	magick := stubTool(t, stubDir, "magick")
	p := newPreparer(t, 10<<20, 50<<20, "", magick)

	got, err := p.Prepare(writeFile(t, dir, "a.bmp", 2048), KindImage)
	if err != nil {
		t.Fatal(err)
	}
	if got.Note != "imagemagick" {
		t.Fatalf("note = %q", got.Note)
	}
}

func TestVideoTranscodedWithFFmpeg(t *testing.T) {
	dir := t.TempDir()
	stubDir := t.TempDir()
	ffmpeg := stubTool(t, stubDir, "ffmpeg")
	p := newPreparer(t, 10<<20, 50<<20, ffmpeg, "")

	got, err := p.Prepare(writeFile(t, dir, "movie.mkv", 3000), KindVideo)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != InlineVideo || !got.Temp || filepath.Ext(got.Path) != ".mp4" {
		t.Fatalf("got = %+v", got)
	}
}

func TestVideoGivesUpWhenStillTooLarge(t *testing.T) {
	dir := t.TempDir()
	stubDir := t.TempDir()
	ffmpeg := stubTool(t, stubDir, "ffmpeg")
	p := newPreparer(t, 10<<20, 10, ffmpeg, "")

	if _, err := p.Prepare(writeFile(t, dir, "huge.mkv", 3000), KindVideo); !errors.Is(err, ErrNoConverter) {
		t.Fatalf("应当放弃，实际 %v", err)
	}
}

func TestCapabilities(t *testing.T) {
	stubDir := t.TempDir()
	ffmpeg := stubTool(t, stubDir, "ffmpeg")
	if got := newPreparer(t, 1, 1, ffmpeg, "").Capabilities(); got != "ffmpeg" {
		t.Fatalf("capabilities = %q", got)
	}
	if got := newPreparer(t, 1, 1, "", "").Capabilities(); got == "" {
		t.Fatal("capabilities 不应为空")
	}
}

func TestCloseRemovesOwnWorkDir(t *testing.T) {
	p, err := NewPreparer(1, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	dir := p.WorkDir()
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("Close 应当删除临时目录")
	}
}

// 图片转换产物必须真的能塞进内联上限，否则应当降质重试、最终放弃而不是把超限副本递出去。
func TestConvertedImageMustFitPhotoLimit(t *testing.T) {
	dir := t.TempDir()
	stubDir := t.TempDir()
	// 桩工具无视输入，直接写 20MB，模拟"转出来还是太大"
	script := `#!/bin/sh
out=""
while [ $# -gt 0 ]; do out="$1"; shift; done
dd if=/dev/zero of="$out" bs=1M count=20 2>/dev/null
`
	tool := filepath.Join(stubDir, "ffmpeg")
	if err := os.WriteFile(tool, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	p := newPreparer(t, 1<<20, 50<<20, tool, "")
	source := writeFile(t, dir, "scan.bmp", 2048)

	if _, err := p.Prepare(source, KindImage); !errors.Is(err, ErrNoConverter) {
		t.Fatalf("产超限时应当放弃并返回 ErrNoConverter，实际 %v", err)
	}
	// 不能留下临时垃圾
	leftovers, err := os.ReadDir(p.WorkDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("放弃后不该留下临时文件：%v", leftovers)
	}
}

// 产物在限额内时正常返回
func TestConvertedImageWithinLimitIsReturned(t *testing.T) {
	dir := t.TempDir()
	stubDir := t.TempDir()
	ffmpeg := stubTool(t, stubDir, "ffmpeg")
	p := newPreparer(t, 1<<20, 50<<20, ffmpeg, "")
	got, err := p.Prepare(writeFile(t, dir, "scan.bmp", 2048), KindImage)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(got.Path)
	if err != nil || info.Size() == 0 || info.Size() > 1<<20 {
		t.Fatalf("产物不合法：%v %d", err, info.Size())
	}
}

// Close 与 Prepare 并发调用不应发生竞态，也不该把临时目录回落到系统临时目录
func TestCloseIsSafeWithConcurrentPrepare(t *testing.T) {
	dir := t.TempDir()
	stubDir := t.TempDir()
	ffmpeg := stubTool(t, stubDir, "ffmpeg")
	p := newPreparer(t, 10<<20, 50<<20, ffmpeg, "")
	source := writeFile(t, dir, "scan.bmp", 2048)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = p.Prepare(source, KindImage)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = p.Close()
	}()
	wg.Wait()
	if p.WorkDir() != "" {
		t.Fatalf("Close 之后 WorkDir 应当为空，实际 %q", p.WorkDir())
	}
	// 关闭后再 Prepare 必须明确失败，而不是偷偷用系统临时目录
	if _, err := p.Prepare(source, KindImage); err == nil {
		t.Fatal("关闭后 Prepare 应当报错")
	}
}
