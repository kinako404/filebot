// filebot 监控目录，把新增/更新的图片/视频转发到 Telegram。
package main

import (
	"os"

	"filebot/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
