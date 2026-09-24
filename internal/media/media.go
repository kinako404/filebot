// Package media 判断文件类型，并生成 Telegram 能直接点开查看的副本。
//
// 转换工具按 ffmpeg → ImageMagick 顺序探测；都没有时返回 ErrNoConverter，
// 调用方降级为"只发原文件"，不报错。
package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Kind 是文件大类。
type Kind string

const (
	KindImage Kind = "image"
	KindVideo Kind = "video"
)

// 可点开副本的发送方式。
const (
	InlinePhoto     = "photo"
	InlineVideo     = "video"
	InlineAnimation = "animation"
)

// ErrNoConverter 表示本机没有可用的转换工具。
var ErrNoConverter = errors.New("没有可用的转换工具")

var (
	imageExts = map[string]bool{
		".jpg": true, ".jpeg": true, ".jfif": true, ".png": true, ".gif": true,
		".bmp": true, ".webp": true, ".tif": true, ".tiff": true, ".heic": true,
		".heif": true, ".avif": true, ".ico": true,
	}
	videoExts = map[string]bool{
		".mp4": true, ".m4v": true, ".mov": true, ".mkv": true, ".avi": true,
		".webm": true, ".flv": true, ".wmv": true, ".mpg": true, ".mpeg": true,
		".3gp": true, ".3g2": true, ".ts": true, ".m2ts": true, ".ogv": true,
		".rmvb": true, ".vob": true,
	}
	photoPassthrough = map[string]bool{".jpg": true, ".jpeg": true, ".jfif": true, ".png": true}
	videoPassthrough = map[string]bool{".mp4": true, ".m4v": true, ".mov": true, ".webm": true}
	animationExts    = map[string]bool{".gif": true, ".mp4": true}
)

const (
	maxPhotoSide = 2560
	maxVideoSide = 1280
	convertLimit = time.Hour
)

// Classify 按扩展名判断是图片还是视频；其它返回空。
func Classify(path string) Kind {
	switch ext := strings.ToLower(filepath.Ext(path)); {
	case imageExts[ext]:
		return KindImage
	case videoExts[ext]:
		return KindVideo
	default:
		return ""
	}
}

// Prepared 是可点开副本。
type Prepared struct {
	Kind string // photo / video / animation
	Path string
	Temp bool // 需要发送后删除
	Note string
}

// Preparer 负责生成可点开副本。
type Preparer struct {
	photoMax int64
	videoMax int64
	workDir  string
	ownDir   bool
	ffmpeg   string
	magick   string
}

// NewPreparer 创建 Preparer；workDir 为空时用系统临时目录。
func NewPreparer(photoMax, videoMax int64, workDir string) (*Preparer, error) {
	p := &Preparer{photoMax: photoMax, videoMax: videoMax, workDir: workDir}
	if p.workDir == "" {
		dir, err := os.MkdirTemp("", "filebot-")
		if err != nil {
			return nil, err
		}
		p.workDir = dir
		p.ownDir = true
	}
	p.ffmpeg, _ = exec.LookPath("ffmpeg")
	if path, err := exec.LookPath("magick"); err == nil {
		p.magick = path
	} else if path, err := exec.LookPath("convert"); err == nil {
		p.magick = path
	}
	return p, nil
}

// WorkDir 返回临时目录。
func (p *Preparer) WorkDir() string { return p.workDir }

// Capabilities 描述本机可用的转换工具。
func (p *Preparer) Capabilities() string {
	var tools []string
	if p.ffmpeg != "" {
		tools = append(tools, "ffmpeg")
	}
	if p.magick != "" {
		tools = append(tools, filepath.Base(p.magick))
	}
	if len(tools) == 0 {
		return "无（只会发送原文件）"
	}
	return strings.Join(tools, ", ")
}

// Close 清理临时目录。
func (p *Preparer) Close() error {
	if p.ownDir && p.workDir != "" {
		err := os.RemoveAll(p.workDir)
		p.workDir = ""
		return err
	}
	return nil
}

// Prepare 生成可点开副本。转换不可行时返回 ErrNoConverter。
func (p *Preparer) Prepare(path string, kind Kind) (*Prepared, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	ext := strings.ToLower(filepath.Ext(path))
	switch kind {
	case KindImage:
		if animationExts[ext] && info.Size() <= p.videoMax {
			return &Prepared{Kind: InlineAnimation, Path: path}, nil
		}
		if photoPassthrough[ext] && info.Size() <= p.photoMax {
			return &Prepared{Kind: InlinePhoto, Path: path}, nil
		}
		if ext == ".gif" {
			return nil, ErrNoConverter
		}
		return p.convertImage(path)
	case KindVideo:
		if videoPassthrough[ext] && info.Size() <= p.videoMax {
			return &Prepared{Kind: InlineVideo, Path: path}, nil
		}
		return p.convertVideo(path)
	default:
		return nil, ErrNoConverter
	}
}

// Cleanup 删除临时副本。
func (p *Prepared) Cleanup() {
	if p != nil && p.Temp && p.Path != "" {
		_ = os.Remove(p.Path)
	}
}

func (p *Preparer) convertImage(source string) (*Prepared, error) {
	if p.ffmpeg == "" && p.magick == "" {
		return nil, ErrNoConverter
	}
	target, err := p.tempFile(source, ".jpg")
	if err != nil {
		return nil, err
	}
	if p.ffmpeg != "" {
		args := []string{
			"-y", "-loglevel", "error", "-i", source,
			"-vf", fmt.Sprintf("scale='min(%d,iw)':-2", maxPhotoSide),
			"-frames:v", "1", "-q:v", "3", target,
		}
		if err := run(p.ffmpeg, args); err == nil {
			return &Prepared{Kind: InlinePhoto, Path: target, Temp: true, Note: "ffmpeg"}, nil
		}
	}
	if p.magick != "" {
		args := []string{
			source, "-resize", fmt.Sprintf("%dx%d>", maxPhotoSide, maxPhotoSide),
			"-quality", "88", target,
		}
		if err := run(p.magick, args); err == nil {
			return &Prepared{Kind: InlinePhoto, Path: target, Temp: true, Note: "imagemagick"}, nil
		}
	}
	_ = os.Remove(target)
	return nil, ErrNoConverter
}

func (p *Preparer) convertVideo(source string) (*Prepared, error) {
	if p.ffmpeg == "" {
		return nil, ErrNoConverter
	}
	target, err := p.tempFile(source, ".mp4")
	if err != nil {
		return nil, err
	}
	attempts := []struct {
		side int
		crf  int
	}{{maxVideoSide, 28}, {854, 32}, {640, 34}}
	for _, attempt := range attempts {
		args := []string{
			"-y", "-loglevel", "error", "-i", source,
			"-vf", fmt.Sprintf("scale='min(%d,iw)':-2", attempt.side),
			"-c:v", "libx264", "-preset", "veryfast", "-crf", strconv.Itoa(attempt.crf),
			"-c:a", "aac", "-b:a", "96k", "-movflags", "+faststart", target,
		}
		if err := run(p.ffmpeg, args); err != nil {
			continue
		}
		if info, err := os.Stat(target); err == nil && info.Size() > 0 && info.Size() <= p.videoMax {
			return &Prepared{
				Kind: InlineVideo, Path: target, Temp: true,
				Note: fmt.Sprintf("ffmpeg crf%d", attempt.crf),
			}, nil
		}
	}
	_ = os.Remove(target)
	return nil, ErrNoConverter
}

func (p *Preparer) tempFile(source, suffix string) (string, error) {
	stem := strings.TrimSuffix(filepath.Base(source), filepath.Ext(source))
	if len(stem) > 48 {
		stem = stem[:48]
	}
	handle, err := os.CreateTemp(p.workDir, stem+"-*"+suffix)
	if err != nil {
		return "", err
	}
	name := handle.Name()
	handle.Close()
	return name, nil
}

func run(name string, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), convertLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return fmt.Errorf("%s 失败：%w（%s）", filepath.Base(name), err, detail)
		}
		return fmt.Errorf("%s 失败：%w", filepath.Base(name), err)
	}
	return nil
}
