# filebot (Go 重写) — 设计文档

日期：2026-09-24
状态：已实现
前身：`legacy-python/`（Python 版，功能等价，本版替换其部署方式）

## 1. 为什么重写

Python 版运行时依赖解释器，部署要带源码 + 解释器。Go 版编译成单个静态二进制，
拷过去就能跑；同时去掉自研的 Shadowsocks 客户端——用户明确只要求 socks5/http 代理，
于是代理层缩到最小。

## 2. 目标（与 Python 版保持一致的对外行为）

监控一个或多个目录（递归），图片/视频新增或更新时推送到 Telegram：

1. 原文件 → `sendDocument`（不压缩、不转码）
2. 可点开的版本 → `sendPhoto` / `sendAnimation` / `sendVideo(supports_streaming)`

配置项：`telegram.token`、`telegram.chat_id`（字符串/数组/整数都接受，支持多个接收方）、
`proxy.url`、`watch.dirs`（+ 其它可选）。

常驻模式只处理启动之后新增/变化的文件，启动时就存在的文件一律不管；
补发历史文件走 `filebot once`（配置里的 `scan_existing` 常驻模式会忽略并警告）。
环境变量与命令行覆盖，优先级：命令行 > 环境变量 > 配置文件 > 默认值。

## 3. 与 Python 版的差异

| 项 | Python 版 | Go 版 |
|---|---|---|
| 部署 | 需要 python3 + 源码 | 单个静态二进制（linux/amd64） |
| 代理 | 自研 ss 客户端 + socks5/http | 只用 socks5/http（stdlib + 自研 socks5 握手） |
| 依赖 | 零（可选 cryptography） | 零运行时依赖；仅构建期一个 TOML 库（已 vendor） |
| 依赖探测 | pillow/ffmpeg/imagemagick | ffmpeg / imagemagick |
| 服务安装 | 手写 systemd 单元 | `filebot install-service` 自动生成、校验并启用 |

保留不变：轮询 + 去抖的监控策略、状态文件格式（JSON，可与 Python 版共用）、
重试/限流策略、captcha 模板、大小上限语义。

## 4. 结构

```
main.go                     入口，转交 internal/cli
internal/cli/               子命令解析：run(默认) / check / once / install-service / uninstall-service / version
internal/config/            TOML + 环境变量 + 命令行 → Config；未知键报错
internal/watch/             递归轮询、去抖、过滤
internal/media/             扩展名分类 + 生成可点开的副本
internal/tg/                Bot API 客户端（流式 multipart、重试、限流）
internal/proxy/             socks5 / http 代理 → http.Transport
internal/state/             已发送记录（JSON，原子写）
internal/app/               编排：监控 → 队列 → worker
internal/service/           systemd 单元的生成、校验、安装/卸载
internal/testsupport/       测试用假服务端（假 Telegram / SOCKS5 / HTTP 代理）
```

## 5. 关键实现点

- **代理**：`http://`/`https://` 走 `http.Transport.Proxy`（net/http 自己处理 CONNECT 与
  绝对 URI）；`socks5://` 用自写的 `DialContext`（RFC 1928 + RFC 1929，域名直接交给
  代理解析，等于 socks5h）。不支持的 scheme 直接报错，绝不静默直连。
- **上传**：multipart 请求体是自定义 `io.Reader`，顺序流式读取文件，预先算好
  `Content-Length`（不缓冲整个文件）。
- **监控**：轮询快照 `path → (mtime,size)`，变化进 pending，稳定 `settle_seconds` 后产出；
  隐藏文件/临时文件/小于 `min_size`/命中 exclude 的一律忽略。
- **转码**：图片非 jpg/png 用 ffmpeg 或 imagemagick 转 JPEG；视频非 mp4/webm/mov 或超限用
  ffmpeg 转 H.264 MP4；工具缺失时降级为只发原文件（不报错）。
- **状态**：`{"version":1,"files":{"<path>":[mtimeNS,size]}}`，与 Python 版格式一致。

## 6. install-service

`filebot install-service` 面向 Debian 12/13（systemd）：

1. 检查 root 权限、`systemctl` 是否存在、`/etc/os-release` 是否 Debian 系（非 Debian 只警告）；
2. 解析二进制路径（`os.Executable` + `EvalSymlinks`）、配置文件路径（默认 `/etc/filebot/config.toml`）、
   状态目录（`/var/lib/filebot`，由单元里的 `StateDirectory=` 交给 systemd 创建）；
3. 配置文件不存在时自动写入一份模板（权限 0600）并**跳过启动**（否则会因缺 token 崩溃重启）；
4. 必要时创建系统用户（`useradd --system`）；
5. 渲染单元文件 → `systemd-analyze verify` 校验 → 写入 `/etc/systemd/system/<name>.service`；
6. `systemctl daemon-reload`，按需 `enable --now`。

配套 `uninstall-service`（stop+disable+删单元，`--purge` 再删配置/状态/用户）。

`--root` 用于离线镜像或测试：所有文件路径基于该前缀，且默认不 enable/start
（除非显式传 `--enable`/`--start`）。

## 7. 测试

`go test ./...`：配置优先级与未知键、监控去抖与过滤、媒体分类与（桩程序）转码路径、
multipart 字节与重试/限流、socks5 与 http 代理链路、Bot API 端到端（假服务端 + 状态去重）、
systemd 单元渲染与安装流程（假 systemctl + 临时 root）、以及编译真实二进制跑
`--once`/`--check`/`install-service --dry-run`。

另有一组可选联调测试（`FILEBOT_LIVE_TEST=1`）用假 token 打真实 Telegram API，逐个方法确认
请求格式被接受（真实 API 对格式正确的请求回 JSON 401）。**这条是在修掉一个线上事故后补上的**：
`getMe` 之前发的是"没有任何 part 的 multipart"，真实 API 会以 400 + 空 body 拒绝，
而假服务端当时太宽容，测试全绿。现在假服务端同样会拒绝这种请求。

仍未验证：真实代理节点、真实 ffmpeg 输出质量（用桩程序覆盖调用链路）。
未在用户机器上真正安装 systemd 服务（会写入 /etc 并启用一个需要 token 的服务），
仅用 `--dry-run`、临时 root 全流程与 `systemd-analyze verify` 验证。
