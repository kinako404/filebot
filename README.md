# filebot — 监控目录，把新增的图片/视频发到 Telegram

一个静态编译的 Go 二进制，拷到机器上就能跑，无需解释器、无需第三方运行时。

监控一个（或多个）Linux 目录（含所有子目录）。当里面出现新的或更新过的**图片/视频**时，
推送到 Telegram：

1. **原文件**：走 `sendDocument`，不压缩、不转码，点击后当文件下载；
2. **可直接点开的版本**：图片 `sendPhoto`、动图 `sendAnimation`、视频 `sendVideo`
   （开启 `supports_streaming`），在 Telegram 里点一下就能看/播，不是文件。

## 快速开始

```bash
# 1. 拿二进制（或自己编译：make linux-amd64）
sudo install -m 0755 dist/filebot-2.0.0-linux-amd64 /usr/local/bin/filebot

# 2. 先干跑看一眼它会做什么（不写任何文件）
sudo filebot install-service --dry-run

# 3. 自动装 systemd 服务（Debian 12/13）
sudo filebot install-service
#    会生成 /etc/filebot/config.toml（权限 600），但因为没有 token 而跳过启动
sudo vim /etc/filebot/config.toml        # 填 token、chat_id、监控目录、代理
sudo systemctl start filebot
journalctl -u filebot -f
```

不想装服务，手工跑也行：

```bash
cp config.example.toml config.toml && vim config.toml
./filebot check -c config.toml     # 自检：验证 token / 代理 / chat_id
./filebot run   -c config.toml     # 常驻监控
```

## 命令

| 命令 | 作用 |
|---|---|
| `filebot run [选项]` | 常驻监控（不带子命令时也是它） |
| `filebot check [选项]` | 只做连接自检后退出 |
| `filebot once [选项]` | 把目录里现有文件发一遍后退出（补发历史文件用） |
| `filebot install-service` | 生成并安装 systemd 服务（Debian 12/13） |
| `filebot uninstall-service` | 停止并移除服务（`--purge` 连配置/状态/用户一起删） |
| `filebot version` / `help` | 版本 / 帮助 |

常用参数：`-c/--config`、`--token`、`--chat-id`（可重复或用逗号分隔）、`--proxy`、
`-d/--dir`（可重复）、`--settle-seconds`、`--max-upload-mb`、`--no-original`、
`--no-compressed`、`--log-level`。全部见 `filebot help`。

**只发送运行中新增的文件**：常驻模式（`run`）只处理启动之后新出现或发生变化的文件，
启动时就存在、之后没再动过的文件一律不管。要补发目录里已有的文件，单独跑一次
`filebot once`。

## 配置

优先级：**命令行 > 环境变量 > 配置文件 > 默认值**。

```toml
workers = 1                 # 顶层选项必须写在任何 [section] 之前
log_level = "INFO"

[telegram]
token = "123456:AA..."      # 必填
chat_id = "-1001234567890"  # 必填，支持多个
# chat_id = ["-1001234567890", "@mychannel", -1009876543210]   # 字符串/数组都行
api_base = "https://api.telegram.org"

[proxy]
url = "socks5://127.0.0.1:1080"   # 或 http://127.0.0.1:3128；留空直连

[watch]
dirs = ["/data/media"]      # 必填，递归包含子目录
poll_interval = 2.0
settle_seconds = 3.0
min_size = 1024
exclude = ["*.part", "*.tmp", "*.crdownload"]
state_file = "/var/lib/filebot/state.json"

[upload]
send_original = true
send_compressed = true
max_upload_mb = 50
photo_max_mb = 10
video_max_mb = 50
caption_template = "{name}\n{size} · {path}"
```

环境变量：`FILEBOT_TG_TOKEN`、`FILEBOT_TG_CHAT_ID`（多个用逗号分隔）、`FILEBOT_TG_API_BASE`、
`FILEBOT_PROXY`、`FILEBOT_WATCH_DIRS`（冒号分隔）、`FILEBOT_STATE_FILE`、`FILEBOT_LOG_LEVEL`。

```bash
FILEBOT_TG_TOKEN=123:abc FILEBOT_TG_CHAT_ID=@mychannel,-1009876543210 \
FILEBOT_WATCH_DIRS=/data/media FILEBOT_PROXY=socks5://127.0.0.1:1080 \
filebot once
```

写错的配置项会直接报错（不会静默忽略），例如把 `log_level` 写进 `[upload]` 段：

```
配置错误：配置文件里有无法识别的配置项：upload.log_level
```

## 代理

只支持两种，留空表示直连：

- `socks5://[user:pass@]host:port`（也接受 `socks5h://`）——自写 RFC 1928/1929 握手，
  域名直接交给代理解析（等于 socks5h）；
- `http://[user:pass@]host:port` —— 用 Go 标准库的代理支持，HTTPS 走 CONNECT、
  明文 HTTP 走绝对 URI。

其它协议（比如 `ss://`）会**明确报错**，不会静默直连：

```
不支持的代理协议 "ss"：只支持 http:// 与 socks5://，留空表示直连
```

## 关于"压缩版"

Bot API 没有客户端那种"压缩开关"。这里用的是 Telegram 的**媒体消息**：

| 文件 | 发送方式 | 效果 |
|---|---|---|
| jpg/jpeg/png ≤ 10MB | `sendPhoto`（原样） | 直接显示，点开看原图 |
| gif ≤ 50MB | `sendAnimation` | 自动播放 |
| mp4/m4v/mov/webm ≤ 50MB | `sendVideo(supports_streaming)` | 点开即播，可拖进度 |
| 其它图片（bmp/webp/heic/tiff…） | 转 JPEG 后 `sendPhoto` | 需要本机有 ffmpeg 或 ImageMagick |
| 其它视频（mkv/avi/flv…）或超限视频 | ffmpeg 转 H.264 MP4 后 `sendVideo` | 需要本机有 ffmpeg |
| 原文件 > `max_upload_mb` | 跳过原文件 | 压缩版仍会发，可开 `notify_skipped` 提醒 |

转换工具按 `ffmpeg` → `magick/convert` 顺序探测，都没有时只发原文件（不报错）。
启动日志会打印 `可用的转换工具`。

装一下就行：`sudo apt install ffmpeg`（或 `imagemagick`）。

## 工作原理

```
轮询扫描目录（快照 mtime+size）
   └─ 新出现/变化 → 等 settle_seconds 不再变化 → 排队
         └─ worker（默认 1 个）：
              ├─ 原文件   → sendDocument
              └─ 可点开的 → 按扩展名分类 → 必要时转码 → sendPhoto/sendVideo/sendAnimation
                     └─ 成功 → 写 state_file（重启不重发同一版本）
```

- **轮询而不是 inotify**：对"正在拷贝的大文件"、网络挂载、容器绑定挂载都更稳；间隔可调。
- **去抖**：文件在 `settle_seconds` 内又变了就重新计时，不会把半个文件发出去。
- **只处理图片/视频**：其它文件在入队前就被过滤（按扩展名）。
- **自动忽略**：隐藏文件/目录、`*.part/*.tmp/*.crdownload` 等临时文件、小于 `min_size` 的。
- **失败不阻塞**：网络类错误退避重试；429 按 `retry_after` 退避；400/403 只记日志不重试；
  单个文件出错不影响后续文件。
- **退出时会把手头的队列发完**（`SIGINT`/`SIGTERM` 都行），不会把已发现的文件丢掉。
- **多个接收方**：同一文件按 `chat_id` 顺序逐个发送，每个接收方内部都是"原文件 → 可点开的
  版本"挨着发；转码只做一次、多个接收方复用同一份副本。某个接收方失败（比如机器人不在那个
  频道里）不影响其它接收方，日志里会明确指出是哪个 chat 失败。

## install-service 做了什么

面向 Debian 12/13（systemd）。`sudo filebot install-service` 会：

1. 检查 root 权限、`systemctl` 是否存在、是否 Debian 系（非 Debian 只警告）；
2. 解析二进制路径（当前可执行文件，跟随符号链接）、配置路径（默认 `/etc/filebot/config.toml`）、
   状态目录（默认 `/var/lib/filebot`）；
3. 配置不存在时写一份模板（权限 0600）并**跳过启动** —— 否则没有 token 会崩溃重启刷日志；
4. 需要时创建系统用户（`useradd --system --shell /usr/sbin/nologin filebot`）；
5. 渲染单元文件 → 用 `systemd-analyze verify` 校验 → 写入
   `/etc/systemd/system/filebot.service`；
6. `systemctl daemon-reload` → `enable` → `restart`。

生成的单元文件：

```ini
[Unit]
Description=filebot - 监控目录并把图片/视频发送到 Telegram
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=filebot
Group=filebot
ExecStart=/usr/local/bin/filebot run -c /etc/filebot/config.toml
WorkingDirectory=/var/lib/filebot
Restart=always
RestartSec=5
StateDirectory=filebot
NoNewPrivileges=true
ProtectSystem=full
PrivateTmp=true
...
```

有用选项：`--user root`（不建新用户）、`--no-start` / `--no-enable`、`--config`、`--bin`、
`--name`、`--force`、`--dry-run`、`--root DIR`（离线镜像：所有路径加前缀，且默认不碰 systemd）。

卸载：`sudo filebot uninstall-service`（加 `--purge` 连配置、状态目录、系统用户一起删）。

## 构建与测试

```bash
make linux-amd64      # → dist/filebot-2.0.0-linux-amd64（CGO_ENABLED=0，静态）
make build            # 本机架构
make test             # go test ./...
make vet
```

测试覆盖：配置优先级与未知键报错、目录去抖与过滤、媒体分类与（桩程序）转码路径、
multipart 字节与重试/限流、**socks5 与 http 代理链路**（假代理服务端）、Bot API 端到端
（假服务端 + 状态去重 + 常驻模式 + 多接收方）、systemd 单元渲染与安装流程（假 systemctl +
临时 root + 真实 `systemd-analyze verify`），最后编译**真实二进制**跑
`once` / `check` / `run`(SIGINT) / `install-service`。

假 Telegram 服务端刻意与真实 API 保持一致的"脾气"（比如同样拒绝没有任何 part 的
multipart 请求），避免出现"测试全绿、线上必挂"。

还有一组可选的**联调测试**，用假 token 打真实 Telegram API，逐个方法确认请求格式被接受
（格式没问题时真实 API 会回 JSON 401）：

```bash
FILEBOT_LIVE_TEST=1 go test ./internal/tg/ -run Live -v
# 走代理：
FILEBOT_LIVE_TEST=1 FILEBOT_TEST_PROXY=socks5://127.0.0.1:1080 go test ./internal/tg/ -run Live -v
```

依赖只有构建期的一个 TOML 库（已 `go mod vendor`，可通过 `-mod=vendor` 完全离线构建）。

## 目录结构

```
main.go                    入口
internal/cli/              子命令与参数
internal/config/           TOML + 环境变量 + 命令行 → 配置
internal/app/              编排：监控 → 队列 → worker
internal/watch/            递归轮询 + 去抖
internal/media/            扩展名分类 + 生成可点开的副本
internal/tg/               Bot API 客户端（流式 multipart / 重试 / 限流）
internal/proxy/            socks5 / http 代理 → http.Transport
internal/state/            已发送记录（与 Python 版格式一致）
internal/service/          systemd 单元的生成、校验、安装/卸载
internal/testsupport/      测试用假服务端（假 Telegram / SOCKS5 / HTTP 代理）
legacy-python/             上一版 Python 实现（保留备查）
docs/superpowers/specs/    设计文档
```

## 已知限制

- Bot API 单文件上传上限 50MB（官方）。要发更大的视频需自建
  [local Bot API server](https://github.com/tdlib/telegram-bot-api)，把 `api_base` 指过去，
  并把 `max_upload_mb` 调大。
- 视频能否"点开即播"取决于 Telegram 对容器的支持：mp4/webm 最稳，其它容器靠 ffmpeg 转码。
- 只比较 mtime + size，文件改名会被当成新文件发送。
- 多接收方时每个文件要发 2×N 条消息，配合默认 1 秒节流会比单接收方慢 N 倍；接收方很多时
  可以把 `min_send_interval` 调小，或分多个进程跑。
- 单进程轮询；几万个文件的目录建议缩小监控范围或调大 `poll_interval`。
- 只支持 systemd 系统（Debian 12/13 默认就是）；OpenRC、Alpine 等需要自己写启动脚本。
