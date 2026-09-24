// Package app 把监控、分类、上传串起来。
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"filebot/internal/config"
	"filebot/internal/media"
	"filebot/internal/proxy"
	"filebot/internal/state"
	"filebot/internal/tg"
	"filebot/internal/watch"
)

const (
	sendAttempts = 2
	retryDelay   = 5 * time.Second
	stopGrace    = 60 * time.Second
)

// App 是一次文件转发任务的运行时。
type App struct {
	cfg      config.Config
	log      *slog.Logger
	client   *tg.Client
	preparer *media.Preparer
	store    *state.Store

	queue   chan watch.File
	workers sync.WaitGroup
}

// New 构造 App（含代理、客户端、转换工具与状态文件）。
func New(cfg config.Config, logger *slog.Logger) (*App, error) {
	if logger == nil {
		logger = slog.Default()
	}
	transport, err := proxy.New(cfg.Proxy.URL, time.Duration(cfg.Telegram.Timeout*float64(time.Second)))
	if err != nil {
		return nil, err
	}
	client, err := tg.New(tg.Options{
		Token:           cfg.Telegram.Token,
		APIBase:         cfg.Telegram.APIBase,
		Timeout:         time.Duration(cfg.Telegram.Timeout * float64(time.Second)),
		MaxRetries:      cfg.Telegram.MaxRetries,
		MinSendInterval: time.Duration(cfg.Telegram.MinSendInterval * float64(time.Second)),
		Transport:       transport,
	})
	if err != nil {
		return nil, err
	}
	preparer, err := media.NewPreparer(cfg.PhotoMaxBytes(), cfg.VideoMaxBytes(), "")
	if err != nil {
		return nil, fmt.Errorf("创建临时目录失败：%w", err)
	}
	store, err := state.Open(cfg.Watch.StateFile)
	if err != nil {
		preparer.Close()
		return nil, err
	}
	if cfg.Proxy.URL == "" {
		logger.Info("未配置代理，直连 Telegram")
	} else {
		scheme := cfg.Proxy.URL
		if idx := strings.Index(scheme, "://"); idx > 0 {
			scheme = scheme[:idx]
		}
		logger.Info("已启用代理", "scheme", scheme)
	}
	logger.Info("可用的转换工具", "tools", preparer.Capabilities())
	return &App{
		cfg: cfg, log: logger, client: client, preparer: preparer, store: store,
		queue: make(chan watch.File, 1000),
	}, nil
}

// Close 释放资源。
func (a *App) Close() {
	if err := a.store.Save(); err != nil {
		a.log.Warn("保存状态文件失败", "err", err)
	}
	if err := a.preparer.Close(); err != nil {
		a.log.Warn("清理临时目录失败", "err", err)
	}
}

// Check 走一遍 getMe 自检。
func (a *App) Check(ctx context.Context) error {
	me, err := a.client.GetMe(ctx)
	if err != nil {
		return err
	}
	a.log.Info("Bot 已就绪", "username", fmt.Sprint(me["username"]), "id", me["id"])
	return nil
}

// RunOnce 处理目录里现有的文件后返回（补发/测试用）。
func (a *App) RunOnce(ctx context.Context) error {
	watcher := watch.New(config.Watch{
		Dirs:          a.cfg.Watch.Dirs,
		PollInterval:  a.cfg.Watch.PollInterval,
		SettleSeconds: 0,
		MinSize:       a.cfg.Watch.MinSize,
		Exclude:       a.cfg.Watch.Exclude,
		ScanExisting:  true,
	})
	ready := watcher.Poll(time.Now())
	pending := make([]watch.File, 0, len(ready))
	for _, file := range ready {
		if !a.relevant(file) {
			continue
		}
		pending = append(pending, file)
	}
	a.log.Info("扫描到待发送文件", "count", len(pending))
	if len(pending) == 0 {
		return nil
	}
	workerCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startWorkers(workerCtx, a.cfg.Workers)
	for _, file := range pending {
		a.queue <- file
	}
	close(a.queue)
	a.waitWorkers(stopGrace)
	return nil
}

// Run 常驻运行直到 ctx 结束。
//
// 只处理启动之后新出现或发生变化（含正在拷贝、写完后才稳定的）文件：
// 启动时就存在且没有再变化的文件一律不管。
func (a *App) Run(ctx context.Context) error {
	if err := a.Check(ctx); err != nil {
		return err
	}
	if a.cfg.Watch.ScanExisting {
		a.log.Warn("常驻模式只发送启动后新增/变化的文件，已忽略 watch.scan_existing；" +
			"要补发已有文件请用 `filebot once`")
	}
	workerCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startWorkers(workerCtx, a.cfg.Workers)

	watchCfg := a.cfg.Watch
	watchCfg.ScanExisting = false
	watcher := watch.New(watchCfg)
	watcher.Run(ctx, func(files []watch.File) {
		fresh := make([]watch.File, 0, len(files))
		for _, file := range files {
			if !a.relevant(file) {
				continue
			}
			fresh = append(fresh, file)
		}
		if len(fresh) == 0 {
			return
		}
		a.log.Info("发现新文件", "count", len(fresh))
		for _, file := range fresh {
			select {
			case a.queue <- file:
			case <-ctx.Done():
				return
			}
		}
	})

	close(a.queue)
	a.waitWorkers(stopGrace)
	a.log.Info("已退出")
	return nil
}

func (a *App) startWorkers(ctx context.Context, count int) {
	if count < 1 {
		count = 1
	}
	for i := 0; i < count; i++ {
		a.workers.Add(1)
		go func() {
			defer a.workers.Done()
			for file := range a.queue {
				a.process(ctx, file)
			}
		}()
	}
}

func (a *App) waitWorkers(grace time.Duration) {
	done := make(chan struct{})
	go func() {
		a.workers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(grace):
		a.log.Warn("等待发送线程退出超时，直接结束", "grace", grace)
	}
}

func (a *App) process(ctx context.Context, file watch.File) {
	done := map[string]bool{}
	for attempt := 1; attempt <= sendAttempts; attempt++ {
		err := a.handle(ctx, file, done)
		if err == nil {
			break
		}
		var apiErr *tg.APIError
		if errors.As(err, &apiErr) {
			a.log.Error("发送失败（Bot API 拒绝，不再重试）",
				"path", file.Path, "err", err)
			break
		}
		a.log.Error("发送失败", "path", file.Path, "attempt", attempt, "err", err)
		if attempt < sendAttempts {
			select {
			case <-ctx.Done():
			case <-time.After(retryDelay):
			}
		}
	}
	// 只要有一个接收方拿到了，就记为已发送，避免之后文件变化时重复轰炸
	if len(done) > 0 {
		a.store.Mark(file.Path, file.MTime, file.Size)
		if err := a.store.Save(); err != nil {
			a.log.Warn("保存状态文件失败", "err", err)
		}
		return
	}
	a.log.Error("文件未能发送，稍后如有变化会重试", "path", file.Path)
}

// handle 把文件发给所有接收方。done 记录已经成功的 (接收方, 内容) 组合，重试时不会重复发送。
func (a *App) handle(ctx context.Context, file watch.File, done map[string]bool) error {
	kind := media.Classify(file.Path)
	if kind == "" {
		a.log.Debug("不是图片/视频，忽略", "path", file.Path)
		return nil
	}
	upload := a.cfg.Upload
	caption := RenderCaption(upload.CaptionTemplate, file.Path, file.Size, string(kind))

	// 转码/抓取只做一次，多个接收方复用同一份副本
	var prepared *media.Prepared
	var preparerErr error
	if upload.SendCompressed {
		prepared, preparerErr = a.preparer.Prepare(file.Path, kind)
		if prepared != nil {
			defer prepared.Cleanup()
		}
		switch {
		case errors.Is(preparerErr, media.ErrNoConverter):
			a.log.Info("无法生成可点开的副本（缺少转换工具或超出上限），只发原文件", "path", file.Path)
			preparerErr = nil
		case preparerErr != nil:
			a.log.Warn("生成可点开的副本失败", "path", file.Path, "err", preparerErr)
			preparerErr = nil
		}
	}

	var firstErr error
	fail := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}

	for _, chat := range a.cfg.Telegram.ChatIDs {
		if upload.SendOriginal {
			key := "original|" + chat
			if !done[key] {
				if file.Size <= a.cfg.MaxUploadBytes() {
					if err := a.client.SendDocument(ctx, chat, file.Path, caption); err != nil {
						a.log.Error("发送原文件失败", "chat", chat, "path", file.Path, "err", err)
						fail(err)
					} else {
						done[key] = true
						a.log.Info("已发送原文件", "chat", chat, "path", file.Path,
							"size", HumanSize(file.Size))
					}
				} else {
					a.log.Warn("原文件超过 Bot API 上传上限，跳过原文件",
						"chat", chat, "path", file.Path, "size", HumanSize(file.Size),
						"limit", HumanSize(a.cfg.MaxUploadBytes()))
					done[key] = true
					if upload.NotifySkipped {
						note := fmt.Sprintf("原文件超过 %s 上限，未发送：\n%s（%s）",
							HumanSize(a.cfg.MaxUploadBytes()), filepath.Base(file.Path), HumanSize(file.Size))
						if err := a.client.SendMessage(ctx, chat, note); err != nil {
							a.log.Warn("发送提醒失败", "chat", chat, "err", err)
						}
					}
				}
			}
		}
		if !upload.SendCompressed {
			continue
		}
		key := "compressed|" + chat
		if done[key] {
			continue
		}
		if prepared == nil {
			done[key] = true
			continue
		}
		inlineCaption := ""
		if !upload.SendOriginal {
			inlineCaption = caption
		}
		var sendErr error
		switch prepared.Kind {
		case media.InlinePhoto:
			sendErr = a.client.SendPhoto(ctx, chat, prepared.Path, inlineCaption)
		case media.InlineAnimation:
			sendErr = a.client.SendAnimation(ctx, chat, prepared.Path, inlineCaption)
		default:
			sendErr = a.client.SendVideo(ctx, chat, prepared.Path, inlineCaption)
		}
		if sendErr != nil {
			a.log.Error("发送可点开的版本失败", "chat", chat, "path", file.Path, "err", sendErr)
			fail(sendErr)
			continue
		}
		done[key] = true
		a.log.Info("已发送可点开的版本", "chat", chat, "path", file.Path,
			"kind", prepared.Kind, "note", prepared.Note)
	}
	return firstErr
}

// relevant 判断文件是否值得发送：是图片/视频，且这个版本还没发过。
func (a *App) relevant(file watch.File) bool {
	if media.Classify(file.Path) == "" {
		a.log.Debug("不是图片/视频，忽略", "path", file.Path)
		return false
	}
	return !a.store.Sent(file.Path, file.MTime, file.Size)
}

// HumanSize 把字节数格式化成易读字符串。
func HumanSize(size int64) string {
	value := float64(size)
	for _, unit := range []string{"B", "KB", "MB", "GB"} {
		if value < 1024 || unit == "GB" {
			if unit == "B" {
				return fmt.Sprintf("%d B", size)
			}
			return fmt.Sprintf("%.1f %s", value, unit)
		}
		value /= 1024
	}
	return fmt.Sprintf("%.1f GB", value)
}

// RenderCaption 套用 caption 模板，模板有问题时退化成文件名。
func RenderCaption(template, path string, size int64, kind string) string {
	replacer := strings.NewReplacer(
		"{name}", filepath.Base(path),
		"{path}", path,
		"{dir}", filepath.Dir(path),
		"{size}", HumanSize(size),
		"{kind}", kind,
	)
	text := strings.TrimSpace(replacer.Replace(template))
	if text == "" {
		text = filepath.Base(path)
	}
	return text
}
