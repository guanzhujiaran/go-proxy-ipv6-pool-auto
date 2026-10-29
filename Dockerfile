# ---------- 构建阶段：只在构建层里编译，最终镜像不包含 Go 工具链 ----------
# 用 alpine 版构建层：只有 musl + Go 工具链，OS 包面最小（扫描器报的漏洞基本都来自
# 构建层的 Debian 包，而这一层不会进入最终镜像）；产物是 CGO_ENABLED=0 的静态二进制，
# 换成 musl 构建不影响在 debian 运行阶段执行。
FROM golang:1.27.3-alpine AS builder

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

# ---------- 运行阶段：只有静态二进制 + 运行时依赖 ----------
FROM debian:bookworm-slim

ENV TZ=Asia/Shanghai \
    DEBIAN_FRONTEND=noninteractive

# 运行时依赖：
#   ndppd    -> IPv6 NDP 代理（程序会写 /etc/ndppd.conf 并执行 service ndppd restart）
#   iproute2 -> 程序执行的 ip route / ip addr / ip neigh 命令
#   procps   -> 程序执行的 sysctl 命令
#   ca-certificates/tzdata -> 出站 HTTPS 与本地时区
#
# 安全基线：
#   - 先 `apt-get upgrade`：slim 镜像自带的包版本往往落后于 bookworm-security，
#     构建时打上安全补丁，避免扫描器报出几十个已知 CVE（关键是别让旧包留在运行镜像里）；
#   - 删掉 apt 缓存、软件包列表、文档与 man，减少镜像体积与可探测面；
#   - 基础镜像建议始终用 `docker build --pull` / CI 定期重建，才能持续吃到上游补丁。
RUN apt-get update \
    && apt-get -y upgrade \
    && apt-get install -y --no-install-recommends \
        ndppd \
        iproute2 \
        procps \
        ca-certificates \
        tzdata \
    && ln -snf /usr/share/zoneinfo/$TZ /etc/localtime \
    && echo "$TZ" > /etc/timezone \
    && apt-get -y clean \
    && rm -rf /var/lib/apt/lists/* /var/cache/apt/archives/*.deb \
        /usr/share/doc/* /usr/share/man/*

WORKDIR /app

COPY --from=builder /out/go-proxy-ipv6-pool /app/go-proxy-ipv6-pool

# 程序启动时会 godotenv.Load(".env")，读不到会直接退出；
# 这里创建一个空 .env，实际配置通过容器环境变量注入（docker-compose environment / --env-file）
RUN : > /app/.env

# 3128: HTTP 代理，3129: SOCKS5 代理
EXPOSE 3128 3129

CMD ["/app/go-proxy-ipv6-pool"]
