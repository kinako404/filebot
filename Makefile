BINARY  := filebot
VERSION := 2.1.1
LDFLAGS := -s -w -X filebot/internal/cli.Version=$(VERSION)

.PHONY: all build linux-amd64 test vet fmt clean install-service

all: linux-amd64

## 本机构建
build:
	mkdir -p dist
	go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY) .

## 目标平台：linux/amd64 静态二进制
linux-amd64:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" \
		-o dist/$(BINARY)-$(VERSION)-linux-amd64 .

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

## 装成 systemd 服务（Debian 12/13，需要 root）
install-service: linux-amd64
	install -m 0755 dist/$(BINARY)-$(VERSION)-linux-amd64 /usr/local/bin/$(BINARY)
	/usr/local/bin/$(BINARY) install-service -c /etc/filebot/config.toml

clean:
	rm -rf dist
