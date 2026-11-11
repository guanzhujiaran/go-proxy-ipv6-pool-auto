# 不要写 `# syntax=docker/dockerfile:1.7`：那会让 BuildKit 去 Docker Hub 拉 frontend 镜像，
# 而 registry-1.docker.io 在多数国内机器上直接超时（实测 8s 不通），构建会卡在第一步。
# 用 BuildKit 内置前端即可，`RUN --mount=type=cache` 在 docker 23+ 是原生支持的。
#
# ---------- 构建阶段：只在构建层里编译，最终镜像不包含 Go 工具链 ----------
# 用 alpine 版构建层：只有 musl + Go 工具链，包面比 debian 小，且这一层不会进入最终镜像；
# 产物是 CGO_ENABLED=0 的静态二进制，跑在 glibc 的 debian 运行阶段没有问题。
#
# 构建层镜像用 ARG：默认固定到具体版本，本机已有该 tag 时构建不再向 registry 查 manifest
# （移动 tag 每次都多一次网络往返，Docker Hub 在国内还会直接超时）。要跟进 Go 新版本/安全补丁时：
#   docker compose build --build-arg GO_IMAGE=golang:alpine
# Docker Hub 拉不动时可以走国内镜像仓库：
#   docker pull docker.m.daocloud.io/library/golang:1.27.3-alpine
#   docker tag  docker.m.daocloud.io/library/golang:1.27.3-alpine golang:1.27.3-alpine
ARG GO_IMAGE=golang:1.27.3-alpine
# 运行阶段的基础镜像。注意：**跨阶段的 FROM 只能读到第一个 FROM 之前声明的 ARG**，
# 所以它必须写在文件最前面（不能写在运行阶段那段注释后面）。
ARG DEBIAN_IMAGE=debian:bookworm-slim
FROM ${GO_IMAGE} AS builder

# 国内环境加速拉依赖；海外机器可 --build-arg GOPROXY=https://proxy.golang.org,direct
ARG GOPROXY=https://goproxy.cn,direct

ENV GO111MODULE=on \
    CGO_ENABLED=0 \
    GOOS=linux \
    GOPROXY=${GOPROXY}

WORKDIR /src

# 先只拷依赖清单，利用层缓存：源码变动时不必重新 go mod download
COPY go-proxy-ipv6-pool/go.mod go-proxy-ipv6-pool/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY go-proxy-ipv6-pool/ ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/go-proxy-ipv6-pool .

# ---------- 运行阶段：静态二进制 + 运行时依赖 ----------
# 运行时依赖：
#   ndppd         -> 回程 NS 代答：整段 /64 由 ndppd 负责，内核 neigh 表一条都不写
#   iproute2      -> 程序执行的 ip route / ip addr / ip neigh / ip link 命令
#   procps        -> sysctl 与 pkill
#   ca-certificates/tzdata -> 出站 HTTPS 与本地时区
#
# 为什么运行阶段用 debian 而不是 alpine：
#   **alpine 源里没有 ndppd 包**（`apk add ndppd` 直接报 no such package），
#   而 debian bookworm 官方源自带 `ndppd`（0.2.x）。构建产物是 CGO_ENABLED=0 静态二进制，
#   跑在 glibc 基线上没有任何依赖问题，所以只有运行阶段换基线，构建阶段仍是 golang:alpine。
#
# 基础镜像同样用 ARG：Docker Hub 在国内常常直接超时，
#   docker pull docker.m.daocloud.io/library/debian:bookworm-slim
#   docker tag  docker.m.daocloud.io/library/debian:bookworm-slim debian:bookworm-slim
# 或直接 `--build-arg DEBIAN_IMAGE=docker.m.daocloud.io/library/debian:bookworm-slim`。
# （ARG DEBIAN_IMAGE 声明在文件顶部，见那里的说明。）
FROM ${DEBIAN_IMAGE}

ENV TZ=Asia/Shanghai

# apt 源：默认换成国内镜像（deb.debian.org 在国内经常几十 KB/s）。
#   - debian bookworm 用 deb822 格式的 /etc/apt/sources.list.d/debian.sources，
#     老镜像可能还是 /etc/apt/sources.list，两处都替换一次，存在哪个改哪个；
#   - 海外机器用 --build-arg APT_MIRROR=deb.debian.org 还原官方源，传空值则不动。
ARG APT_MIRROR=mirrors.aliyun.com

# apt 索引与 deb 包都走 cache mount：反复重建镜像时不再每次重新下载包索引（这是之前构建慢的主因）。
# 注意：用 cache mount 后不要再 `rm -rf /var/lib/apt/lists/*`，否则缓存被清空、下次照样重下。
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    set -eux; \
    if [ -n "${APT_MIRROR}" ]; then \
        for f in /etc/apt/sources.list /etc/apt/sources.list.d/debian.sources; do \
            [ -f "$f" ] && sed -i "s|deb.debian.org|${APT_MIRROR}|g" "$f" || true; \
        done; \
    fi; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        ndppd \
        iproute2 \
        procps \
        ca-certificates \
        tzdata; \
    ln -snf /usr/share/zoneinfo/${TZ} /etc/localtime; \
    echo "${TZ}" > /etc/timezone; \
    ndppd -c /dev/null 2>&1 | head -2 || true

WORKDIR /app

COPY --from=builder /out/go-proxy-ipv6-pool /app/go-proxy-ipv6-pool

# 程序启动时会 godotenv.Load(".env")，读不到会直接退出；
# 这里创建一个空 .env，实际配置通过容器环境变量注入（docker-compose environment / --env-file）
RUN : > /app/.env

# 3128: HTTP 代理，3129: SOCKS5 代理
EXPOSE 3128 3129

CMD ["/app/go-proxy-ipv6-pool"]
