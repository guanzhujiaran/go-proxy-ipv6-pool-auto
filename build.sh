#!/usr/bin/env bash
# 用 golang 容器把二进制编译到 ./bin/，供 docker compose 挂载使用。
# 宿主机不需要安装 Go：编译在容器里做，产物以 bind mount 形式落到 ./bin。
#
# 用法：
#   ./build.sh                                  # 默认 linux/amd64、GOPROXY=https://goproxy.cn,direct
#   GOARCH=arm64 ./build.sh                     # 交叉编译
#   GOPROXY=https://proxy.golang.org,direct ./build.sh
#   GO_IMAGE=golang:1.27.3-alpine ./build.sh
set -euo pipefail

cd "$(dirname "$0")"

GO_IMAGE="${GO_IMAGE:-golang:1.27.3-alpine}"
GOARCH="${GOARCH:-amd64}"
GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"

mkdir -p bin

docker run --rm \
  -e GOPROXY="$GOPROXY" \
  -e GOOS=linux \
  -e GOARCH="$GOARCH" \
  -e CGO_ENABLED=0 \
  -v "$PWD/go-proxy-ipv6-pool:/src" \
  -v "$PWD/bin:/out" \
  -w /src \
  "$GO_IMAGE" \
  go build -trimpath -ldflags="-s -w" -o /out/go-proxy-ipv6-pool .

echo "已生成 bin/go-proxy-ipv6-pool（linux/$GOARCH）"
