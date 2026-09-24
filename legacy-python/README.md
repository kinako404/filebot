# filebot — 监控目录，把新增的图片/视频发到 Telegram

监控一个（或多个）Linux 目录（含所有子目录）。当里面出现新的或更新过的**图片/视频**时，
推送到 Telegram：

1. **原文件**：走 `sendDocument`，不压缩、不转码，点击后当文件下载；
2. **可直接点开的版本**：图片 `sendPhoto`、动图 `sendAnimation`、视频 `sendVideo`
   （开了 `supports_streaming`），在 Telegram 里点一下就能看/播，不是文件。

纯 Python 标准库实现，零第三方依赖即可运行（`cryptography` 可选，仅用于 AES-GCM 硬件加速）。

## 快速开始

```bash
cp config.example.toml config.toml
vim config.toml          # 填 token、chat_id、监控目录、ss 代理链接

python3 bot.py -c config.toml --check    # 自检：验证 token / 代理 / chat_id
python3 bot.py -c config.toml            # 正式运行
```

`--check` 输出的 `Bot 已就绪：@xxx` 说明链路通了。

## 三种配置方式

优先级：**命令行 > 环境变量 > 配置文件 > 默认值**。

```bash
# 1) 配置文件（推荐）
python3 bot.py -c config.toml

# 2) 环境变量
export FILEBOT_TG_TOKEN="123:abc"
export FILEBOT_TG_CHAT_ID="@mychannel"
export FILEBOT_PROXY="ss://YWVzLTI1Ni1nY206cHc@1.2.3.4:8388"
export FILEBOT_WATCH_DIRS="/data/media:/srv/incoming"   # 冒号分隔
python3 bot.py

# 3) 命令行
python3 bot.py --token "123:abc" --chat-id "@mychannel" \
  --proxy "socks5://127.0.0.1:1080" -d /data/media -d /srv/incoming
```

支持的变量：`FILEBOT_TG_TOKEN`、`FILEBOT_TG_CHAT_ID`、`FILEBOT_TG_API_BASE`、
`FILEBOT_PROXY`、`FILEBOT_WATCH_DIRS`、`FILEBOT_STATE_FILE`、`FILEBOT_SCAN_EXISTING`、
`FILEBOT_LOG_LEVEL`。

## 常用参数

| 参数 | 作用 |
|---|---|
| `-c, --config PATH` | TOML 配置文件 |
| `--check` | 只做连接自检后退出 |
| `--once` | 把目录里现有文件处理一遍后退出（补发/测试用） |
| `--scan-existing` | 常驻运行时也发送已存在的文件 |
| `--no-original` / `--no-compressed` | 只发其中一种 |
| `--settle-seconds N` | 文件稳定 N 秒后才发送（默认 3） |
| `--max-upload-mb N` | 原文件上传上限（默认 50） |
| `--poll-interval N` | 扫描间隔（默认 2 秒） |
| `--log-level DEBUG` | 看详细日志 |

## 工作原理

```
轮询扫描目录（快照 mtime+size）
   └─ 新出现/变化 → 等 settle_seconds 不再变化 → 排队
         └─ 单个发送线程：
              ├─ 原文件  → sendDocument
              └─ 压缩版  → 按扩展名分类 → 必要时转码 → sendPhoto/sendVideo/sendAnimation
                     └─ 成功 → 写 state_file（重启不重发同一版本）
```

- **不用 inotify 而是轮询**：拷贝中的大文件、网络文件系统、容器挂载都更稳；轮询间隔可调。
- **去抖**：文件在 `settle_seconds` 内又变了就重新计时，不会把半个文件发出去。
- **自动忽略**：隐藏文件/目录、`*.part` `*.tmp` `*.crdownload` 等临时文件、小于 `min_size` 的文件。
- **失败不阻塞**：单个文件出错只记日志；网络类错误自动重试；触发限流按 `retry_after` 退避。
- **发送顺序按 `send_original`**：两条消息（原文件在前，可点开的在后），压缩版不重复贴标题。

## 关于"压缩版"

Bot API 没有客户端那种"压缩开关"。这里用的是 Telegram 的**媒体消息**：

| 文件 | 发送方式 | 效果 |
|---|---|---|
| jpg/jpeg/png ≤ 10MB | `sendPhoto`（原样） | 直接显示，点开看原图 |
| gif ≤ 50MB | `sendAnimation` | 自动播放 |
| mp4/m4v/mov/webm ≤ 50MB | `sendVideo(supports_streaming)` | 点开即播，可拖进度 |
| 其它图片（bmp/webp/heic/tiff…） | 转 JPEG 后 `sendPhoto` | 需要本机有转换工具 |
| 其它视频（mkv/avi/flv…）或超限视频 | ffmpeg 转 H.264 MP4 后 `sendVideo` | 需要本机有 ffmpeg |
| 原文件 > `max_upload_mb` | 跳过原文件 | 压缩版仍会发，可用 `notify_skipped` 提醒 |

转换工具按 `Pillow` → `ffmpeg` → `ImageMagick` 顺序探测，都没有时只发原文件（不会报错）。
查看当前可用工具：启动日志会打印 `可用的转换工具：...`。

## 代理

`[proxy] url` 支持：

- `ss://...` —— 内置 Shadowsocks 客户端，无需安装 `sslocal`。支持三种链接写法：
  - `ss://base64(method:password)@host:port#备注`
  - `ss://base64(method:password@host:port)#备注`
  - `ss://method:password@host:port`
- `socks5://[user:pass@]host:port`（也接受 `socks5h://`）
- `http://[user:pass@]host:port`
- 留空 = 直连

**加密方式**：`aes-128-gcm` / `aes-192-gcm` / `aes-256-gcm` / `chacha20-ietf-poly1305`（AEAD-2017）。
**不支持**：`2022-blake3-*`、流加密（`aes-256-cfb` 等）、插件混淆（`obfs`/`v2ray-plugin`），
遇到会明确报错而不是静默直连。

实现方式：在本机 `127.0.0.1` 起一个 HTTP CONNECT 小代理，出口接上面配置的上游；
Bot API 流量全部经它转发。这样 ss / socks5 / http 三种上游共用一条代码路径。

若本机有 `cryptography`（`apt install python3-cryptography` 或 `pip install cryptography`），
AES-GCM 和 ChaCha20-Poly1305 都走它的快实现；没有时 chacha20 会退到内置的纯 Python
实现（RFC 8439，较慢但可用），AES 则需要 `cryptography`。

## 部署为 systemd 服务

```bash
sudo mkdir -p /opt/filebot
sudo cp -r . /opt/filebot/
sudo cp deploy/filebot.service /etc/systemd/system/
sudo vim /etc/systemd/system/filebot.service   # 按需改用户/路径
sudo systemctl daemon-reload
sudo systemctl enable --now filebot
journalctl -u filebot -f
```

## 测试

```bash
python3 -m unittest discover -s tests -t .
```

101 个用例，包含：RFC 8439 / RFC 5869 官方测试向量、`ss://` 解析、假 Shadowsocks 服务端
回环（隧道与分块封帧双向验证）、假 SOCKS5 上游、multipart 组装与重试策略、目录去抖规则、
以及**端到端**：真实临时目录 → 监控 → 分类/压缩 → 假 Bot API（直连与走 ss 两条链路），
外加真正子进程跑 `bot.py --once` / `--check` / 常驻模式 + SIGTERM 退出。

没有在真实 Telegram API 和真实 ss 节点上跑过（需要真实凭据）；转码路径用桩程序验证了调用链路。

## 目录结构

```
bot.py                    入口脚本
filebot/
  cli.py                  命令行参数
  config.py               配置读取与校验
  app.py                  编排：监控 → 发送队列 → worker
  watcher.py              递归轮询 + 去抖
  media.py                扩展名分类 + 生成可点开的副本
  botapi.py               Bot API 客户端（multipart / 重试）
  proxy.py                本地 CONNECT 代理 + ss/socks5/http 上游
  ssproto.py              Shadowsocks AEAD-2017 协议实现
  state.py                已发送记录
tests/                    单元测试 + 端到端测试
docs/superpowers/specs/   设计文档
```

## 已知限制

- Bot API 单文件上传上限 50MB（官方）。要发更大的视频，请自建
  [local Bot API server](https://github.com/tdlib/telegram-bot-api) 并把 `api_base` 指过去，
  再把 `max_upload_mb` 调大。
- 视频能否"点开即播"取决于 Telegram 对容器的支持：mp4/webm 最稳，其它容器靠 ffmpeg 转码。
- 只监控"文件内容变化"（mtime+size），改名会当成新文件发送。
- 单进程轮询，几万个文件的目录建议缩小监控范围或调大 `poll_interval`。
