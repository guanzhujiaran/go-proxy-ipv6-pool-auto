#!/usr/bin/env bash
# 用 golang 容器把二进制编译到 ./bin/，供 docker compose 挂载使用。
# 宿主机不需要安装 Go：编译在容器里做，产物以 bind mount 形式落到 ./bin。
#
# 用法：
#   ./build.sh                                  # 默认 linux/amd64、GOPROXY=https://goproxy.cn,direct
#   GOARCH=arm64 ./build.sh                     # 交叉编译
#   GOPROXY=https://proxy.golang.org,direct ./build.sh
#   GO_IMAGE=golang:1.27.3-alpine ./build.sh
#   NO_CACHE=1 ./build.sh                       # 不用缓存卷（排障用）
#
# 速度：每次 docker run 都是全新容器，容器内 /go/pkg/mod 与 /root/.cache/go-build 默认是空的，
# 于是每次都要「重新下载模块 + 全量编译」（实测约 15s）。这里挂两个命名卷复用这两层缓存，
# 第二次起通常 2~3s。
set -euo pipefail

cd "$(dirname "$0")"

GO_IMAGE="${GO_IMAGE:-golang:1.27.3-alpine}"
GOARCH="${GOARCH:-amd64}"
GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"

# 缓存卷名带上 Go 版本：换 Go 版本时旧缓存不再适用，避免偶发的编译缓存不兼容
GO_VERSION_TAG="${GO_IMAGE##*:}"
MOD_CACHE_VOLUME="go-proxy-modcache-${GO_VERSION_TAG}"
BUILD_CACHE_VOLUME="go-proxy-buildcache-${GO_VERSION_TAG}"

mkdir -p bin

CACHE_ARGS=()
if [[ "${NO_CACHE:-0}" != "1" ]]; then
  CACHE_ARGS=(
    -v "${MOD_CACHE_VOLUME}:/go/pkg/mod"
    -v "${BUILD_CACHE_VOLUME}:/root/.cache/go-build"
  )
fi

docker run --rm \
  "${CACHE_ARGS[@]}" \
  -e GOPROXY="$GOPROXY" \
  -e GOOS=linux \
  -e GOARCH="$GOARCH" \
  -e CGO_ENABLED=0 \
  -e GOMODCACHE=/go/pkg/mod \
  -e GOCACHE=/root/.cache/go-build \
  -v "$PWD/go-proxy-ipv6-pool:/src" \
  -v "$PWD/bin:/out" \
  -w /src \
  "$GO_IMAGE" \
  go build -trimpath -ldflags="-s -w" -o /out/go-proxy-ipv6-pool .

echo "已生成 bin/go-proxy-ipv6-pool（linux/$GOARCH）"
