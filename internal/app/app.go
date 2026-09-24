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
	"sync/atomic"
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
	// stopGrace 是常驻模式退出时等待在途发送的上限
	stopGrace = 60 * time.Second
	// maxDrainGrace 是退出前为"还在去抖窗口里"的文件额外等待的上限
	maxDrainGrace = 10 * time.Second
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
	// failed 统计"一条消息都没发出去"的文件数，供 once 判断退出码
	failed atomic.Int64
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
	// 补发不设硬上限：这是"把目录里现有文件发一遍"的一次性任务，
	// 到点就返回会让脚本以为成功了、实际只发了一部分
	a.workers.Wait()
	if failed := a.failed.Load(); failed > 0 {
		return fmt.Errorf("有 %d 个文件没能发送成功，请查看日志；可重新执行一次补发", failed)
	}
	return nil
}

// Run 常驻运行直到 ctx 结束。
//
// 只处理启动之后新出现或发生变化（含正在拷贝、写完后才稳定的）文件：
// 启动时就存在且没有再变化的文件一律不管。
func (a *App) Run(ctx context.Context) error {
	watchCfg := a.cfg.Watch
	watchCfg.ScanExisting = false
	watcher := watch.New(watchCfg)
	// 先拍基准快照，再去做网络自检：否则自检（含重试，可能几十秒）期间
	// 落进目录的文件会被当成"启动前就存在"，之后永远发不出去
	watcher.Poll(time.Now())

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

	watcher.Run(ctx, func(files []watch.File) { a.enqueue(files, ctx) })

	// 退出前再给"刚落地、还在去抖窗口里"的文件一点时间，
	// 否则它们会留在目录里再也发不出去（下次启动已算"启动前就存在"）
	a.drainPending(watcher)
	close(a.queue)
	a.waitWorkers(stopGrace)
	a.log.Info("已退出")
	return nil
}

// enqueue 过滤并投递文件，返回实际入队数量。ctx 为 nil 表示不响应取消。
func (a *App) enqueue(files []watch.File, ctx context.Context) int {
	fresh := make([]watch.File, 0, len(files))
	for _, file := range files {
		if !a.relevant(file) {
			continue
		}
		fresh = append(fresh, file)
	}
	if len(fresh) == 0 {
		return 0
	}
	a.log.Info("发现新文件", "count", len(fresh))
	queued := 0
	for _, file := range fresh {
		if ctx == nil {
			a.queue <- file
			queued++
			continue
		}
		select {
		case a.queue <- file:
			queued++
		case <-ctx.Done():
			return queued
		}
	}
	return queued
}

// drainPending 在退出前等一小段时间，把去抖窗口里的文件也发出去。
func (a *App) drainPending(w *watch.Watcher) {
	grace := a.cfg.Watch.SettleSeconds
	if grace <= 0 {
		grace = 0.1
	}
	if grace > maxDrainGrace.Seconds() {
		grace = maxDrainGrace.Seconds()
	}
	deadline := time.Now().Add(time.Duration(grace * float64(time.Second)))
	queued := 0
	for {
		queued += a.enqueue(w.Poll(time.Now()), nil)
		if len(w.Pending()) == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if queued > 0 {
		a.log.Info("退出前补发去抖中的文件", "count", queued)
	}
	if left := w.Pending(); len(left) > 0 {
		a.log.Warn("以下文件退出前仍未稳定，本次不发送；需要补发请运行 `filebot once`", "files", left)
	}
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
	delivered := 0
	for attempt := 1; attempt <= sendAttempts; attempt++ {
		sent, err := a.handle(ctx, file, done)
		delivered += sent
		if err == nil {
			break
		}
		// 只有当所有失败都是"Bot API 明确拒绝"时才不再重试；
		// 混着网络类错误就还有救，值得再试一次
		if permanentOnly(err) {
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
	if delivered == 0 {
		// 一条消息都没发出去（比如同时关了原文件、又没有可用的转换工具），
		// 绝不能记成"已发送"，否则这个文件以后永远不会再尝试
		a.failed.Add(1)
		a.log.Error("文件没有任何内容发送成功，不记为已发送", "path", file.Path)
		return
	}
	// 至少有一个接收方拿到了，记为已发送，避免之后文件变化时重复轰炸
	a.store.Mark(file.Path, file.MTime, file.Size)
	if err := a.store.Save(); err != nil {
		a.log.Warn("保存状态文件失败", "err", err)
	}
}

// permanentOnly 判断错误树里是否全部都是不可重试的 Bot API 业务错误。
func permanentOnly(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		items := joined.Unwrap()
		if len(items) == 0 {
			return false
		}
		for _, item := range items {
			if !permanentOnly(item) {
				return false
			}
		}
		return true
	}
	var apiErr *tg.APIError
	return errors.As(err, &apiErr)
}

// handle 把文件发给所有接收方，返回实际成功发出的消息条数。
// done 记录已经成功的 (接收方, 内容) 组合，重试时不会重复发送。
func (a *App) handle(ctx context.Context, file watch.File, done map[string]bool) (int, error) {
	kind := media.Classify(file.Path)
	if kind == "" {
		a.log.Debug("不是图片/视频，忽略", "path", file.Path)
		return 0, nil
	}
	upload := a.cfg.Upload
	caption := RenderCaption(upload.CaptionTemplate, file.Path, file.Size, string(kind))

	// 转码/抓取只做一次，多个接收方复用同一份副本
	var prepared *media.Prepared
	if upload.SendCompressed {
		var preparerErr error
		prepared, preparerErr = a.preparer.Prepare(file.Path, kind)
		if prepared != nil {
			defer prepared.Cleanup()
		}
		switch {
		case errors.Is(preparerErr, media.ErrNoConverter):
			a.log.Info("无法生成可点开的副本（缺少转换工具或超出上限）",
				"path", file.Path, "send_original", upload.SendOriginal)
			preparerErr = nil
		case preparerErr != nil:
			a.log.Warn("生成可点开的副本失败", "path", file.Path, "err", preparerErr)
			preparerErr = nil
		}
	}

	delivered := 0
	var failures []error

	for _, chat := range a.cfg.Telegram.ChatIDs {
		if upload.SendOriginal {
			key := "original|" + chat
			if !done[key] {
				if file.Size <= a.cfg.MaxUploadBytes() {
					if err := a.client.SendDocument(ctx, chat, file.Path, caption); err != nil {
						a.log.Error("发送原文件失败", "chat", chat, "path", file.Path, "err", err)
						failures = append(failures, err)
					} else {
						done[key] = true
						delivered++
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
			failures = append(failures, sendErr)
			continue
		}
		done[key] = true
		delivered++
		a.log.Info("已发送可点开的版本", "chat", chat, "path", file.Path,
			"kind", prepared.Kind, "note", prepared.Note)
	}
	return delivered, errors.Join(failures...)
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
