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

## 8. CR 修复记录（2026-09-25，v2.1.2）

一次 5 个 reviewer 的并行只读审查找出并修掉了这些真问题（每条都有能真正失败的新测试，
并用"回退修复 → 测试必须变红"的变异验证确认过）：

| 问题 | 影响 | 修法 |
|---|---|---|
| 常驻模式的基准快照在 `getMe` 自检之后才拍 | 自检（含重试，可能几十秒）期间新增的文件被当成"启动前就存在"，永远不发 | 先拍基准快照再自检 |
| `watch.dirs` 指向符号链接目录 | 静默监控不到任何文件（`WalkDir` 用 Lstat 看根条目） | `EvalSymlinks` 解析根目录 + 跟随文件级符号链接 |
| `send_original=false` 且无可用转换工具 | 一条消息都没发却被记为"已发送"，`once` 也永远跳过 | 用 `delivered` 计数，只有真发出去才写状态 |
| `poll_interval=0/负数` | `timer.Reset(0)` 变成全速扫盘的忙循环 | 钳到 100ms 下限 |
| 退出时还在去抖窗口里的文件 | 静默丢失，重启后也不会再发 | 退出前最多再等 `settle`（上限 10s）补发，来不及的记日志 |
| `once` 有 60 秒硬上限 | 大批量补发静默只发一部分却返回 0 | 不设上限，且有文件失败时返回非 0 |
| `Content-Length` 与实际字节来自两次 stat | 上传期间文件变化必然失败，重试还复用过期长度 | 每次尝试重建请求体（重新 stat） |
| `http.Client` 跟随重定向 | `api_base` 为 http 时 POST 被降级成无 body 的 GET，文件没上传 | `CheckRedirect` 返回 `ErrUseLastResponse`，3xx 报明确错误 |
| 429 + 非 JSON body | 接入层限流页被当成永久错误 | 按状态码分类，429/408/425/5xx 仍可重试 |
| token / 代理口令进错误信息 | 写进 stderr 与持久化 journal | 错误文本脱敏，`Unwrap` 链保持以不破坏错误分类 |
| `proxy.New` 的 `timeout` 被忽略 | 配置项不生效，黑洞代理下卡满 `telegram.timeout` | 真正用于拨号与握手 deadline |
| 图片转码产物不校验 | `photo_max_mb` 形同虚设，超限副本被递出去后被 API 拒绝 | 校验产物大小并降质重试，最终放弃 |
| `Preparer.Close` 与并发 `Prepare` | 关停时对 `workDir` 的数据竞态、静默回落到系统临时目录 | 加锁 + `closed` 标志 |
| `uninstall-service --purge` | 会整目录删掉用户用 `-c/--state-dir` 指定的路径 | 只删配置文件/状态文件；目录只在本包默认路径上删 |
| 生成的配置 0600 属 root，服务以 `User=filebot` 跑 | 默认安装路径下服务读不到配置，反复重启 | 安装时把配置 chown 给服务用户 |
| 配置存在但仍是空模板 | 二次安装会 enable/start 一个必然崩溃的服务 | 用 `config.Load+Validate` 判断"可用"才 enable/start |
| `ExecStart` 路径不含引号 | 带空格的路径被 systemd 切词，静默截断 | 按 systemd 规则加引号转义 |

已知未修（记录在 README 的"已知限制"里）：多接收方时只要有任一接收方成功即记为已发送，
因此当时失败的那个接收方不会再收到该版本；彻底修需要把状态按 `路径+接收方` 维度记录。
