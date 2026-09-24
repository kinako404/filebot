# Telegram 目录监控转发 Bot — 设计文档

日期：2026-09-24
状态：已实现（本次会话无法与用户交互确认，按用户原始需求 + 合理默认值实现；如有偏差请指出）

## 1. 目标

监控一个（或多个）Linux 目录，包含所有子目录。当其中有图片 / 视频文件新增或更新时，
把文件推送到 Telegram：

1. **原文件**：以 `sendDocument` 发送，不压缩、不转码，保留原始画质与容器格式，点击后在
   Telegram 内以"文件"形式下载。
2. **压缩版（可直接点开查看）**：图片走 `sendPhoto`，动图走 `sendAnimation`，视频走
   `sendVideo`（`supports_streaming=true`），在 Telegram 里是点击即播放/查看的媒体，而不是文件。

## 2. 配置项（用户明确要求的三个 + 必要补充）

用户要求：`tgbot token`、`ss 代理链接`、`监控目录`。

| 配置 | 位置 | 说明 |
|---|---|---|
| token | `[telegram] token` | BotFather 给的 token |
| ss 代理链接 | `[proxy] url` | `ss://...`；同时兼容 `socks5://`、`http://`、留空直连 |
| 监控目录 | `[watch] dirs` | 列表，递归生效 |

补充项（都有默认值）：`chat_id`（必填）、`api_base`、`send_original`、`send_compressed`、
`poll_interval`、`settle_seconds`、`exclude`、`min_size`、`max_upload_mb`、`state_file`。
环境变量可覆盖：`FILEBOT_TG_TOKEN` / `FILEBOT_TG_CHAT_ID` / `FILEBOT_PROXY` /
`FILEBOT_WATCH_DIRS` / `FILEBOT_TG_API_BASE`。

## 3. 关键约束（Telegram Bot API）

- **上传上限 50 MB**（官方 API）。自建 local Bot API server 可到 2000 MB，因此 `api_base`
  可配置。超过上限时：原文件跳过并记日志，压缩版尽力用 ffmpeg 压到上限内。
- 只有 `sendPhoto` / `sendVideo` / `sendAnimation` 是"可点开"的；`sendDocument` 一定是文件。
- 视频要能在客户端内联播放，容器基本得是 mp4/webm(+m4v/mov remux 友好)；其它容器若本机
  有 ffmpeg 就转 mp4，没有则只发原文件。
- 限流：约 20 条/分钟每群、30 条/秒全局。程序串行发送 + 最小间隔 + 429 `retry_after` 退避。

## 4. 架构

单进程、多线程、纯标准库（可选 `cryptography`）：

```
bot.py  (CLI 入口)
└── filebot.app       编排：代理 → Bot API 客户端 → 监控线程 → 发送队列 → worker
    ├── filebot.config     TOML + 环境变量 → Config
    ├── filebot.ssproto    ss:// 解析、EVP_BytesToKey、HKDF-SHA1、AEAD-2017 分块封帧
    ├── filebot.proxy      本地 HTTP CONNECT 代理，上游 = ss / socks5 / http / 直连
    ├── filebot.botapi     Bot API 客户端（http.client + 手写 multipart + 重试）
    ├── filebot.media      扩展名分类 + 生成可内联的压缩副本（Pillow/ffmpeg/ImageMagick）
    ├── filebot.watcher    轮询快照 → 去抖/稳定判定 → 产出待发送文件
    └── filebot.state      JSON 状态文件，已发送记录（防重启重发）
```

### 4.1 代理层
`socat`/`sslocal` 之类的依赖不想强加，因此自己实现 Shadowsocks AEAD-2017 客户端：
解析 `ss://` → 本地起一个 HTTP CONNECT 代理（127.0.0.1 随机端口）→ 每个 CONNECT 请求
建一条到 SS 服务器的 TCP，按 AEAD 分块封帧转发。`socks5://` / `http://` 上游同样先落到
这个本地 CONNECT 代理，出口统一。这样 `http.client` 只要 `set_tunnel()` 即可。

支持的加密：`aes-128/192/256-gcm`、`chacha20-ietf-poly1305`（AEAD-2017）。
纯 Python ChaCha20-Poly1305 后备实现（RFC 8439 向量验证），使无 `cryptography` 的机器
也能用 chacha20 节点；AES-GCM 无 `cryptography` 时报错并给出安装提示。
不支持 `2022-blake3-*`、流加密（`aes-256-cfb` 等）与插件混淆，遇到会明确报错。

### 4.2 监控层
轮询（默认 2s）递归快照 `{路径: (mtime_ns, size)}`：
新增或变化的路径进入 pending；pending 中连续 `settle_seconds`（默认 3s）未再变化且大小 > 0
才产出 → 天然兼容"正在拷贝的大文件"和重命名。忽略隐藏文件、`*.tmp/*.part/*.crdownload`、
小于 `min_size` 的文件。可选 `--scan-existing` 在启动时把已有文件当作新文件处理。

### 4.3 发送流程（每个文件）
1. 组装 caption（`{name}/{size}/{path}/{dir}` 模板）。
2. `send_original` 且大小未超限 → `sendDocument`。
3. `send_compressed` → 生成内联副本 → `sendPhoto` / `sendVideo` / `sendAnimation`。
4. 成功后写状态文件；失败按重试策略处理，最终失败只记日志不阻塞后续文件。

## 5. 错误处理
- 上传失败重试：连接/超时错误指数退避（默认 3 次）；429 按 `retry_after` 休眠；5xx 重试；
  400/403 不重试（token/chat_id/权限问题），直接记日志。
- 单个文件出错不影响队列；worker 捕获所有异常。
- 代理启动失败 → 启动即报错退出，不静默直连（避免误泄露流量）。

## 6. 测试与验证
`python3 -m unittest discover -s tests -t .`（101 个用例）：
- RFC 8439 §2.8.2 / §A.5 ChaCha20-Poly1305 官方向量；RFC 5869 HKDF 向量（SHA-1/SHA-256）。
- `ss://` 三种 URL 形态解析、KDF 长度、地址头编解码。
- SS 回环：本地极简 SS 服务端 + 本地 CONNECT 代理，端到端把 HTTP 请求穿过 SS 隧道。
- multipart 组装正确性、429/5xx 重试、400 不重试。
- watcher：子目录新增、修改、拷贝中途不触发、稳定后触发、隐藏/临时文件与 min_size/exclude 过滤。
- 配置：TOML、环境变量、命令行优先级与未知键报错。
- e2e：假 Telegram API 服务器（HTTP）接收 `sendDocument`+`sendPhoto`/`sendVideo`，
  校验原文件字节与压缩副本；直连、走 ss、走 socks5 三条链路；常驻模式实时发送与优雅退出。
- CLI：真正子进程跑 `--once` / `--check` / 常驻 + SIGTERM。

未能验证的部分（本机无相应环境）：真实 Telegram API、真实 SS 节点、真实 ffmpeg/Pillow
转码输出质量（测试用桩程序覆盖了调用链路与降级逻辑）。
