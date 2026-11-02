# ---------- 构建阶段：只在构建层里编译，最终镜像不包含 Go 工具链 ----------
# 用 alpine 版构建层：只有 musl + Go 工具链，包面比 debian 小，且这一层不会进入最终镜像；
# 产物是 CGO_ENABLED=0 的静态二进制，所以运行阶段也直接用 alpine（musl 对 musl，无 libc 兼容问题）。
#
# 基础镜像一律用移动 tag（不写死大版本/patch）：
#   - 构建层用 golang:alpine 而不是 golang:latest：两者当前都是 go1.27.3，但 golang:latest
#     是 Debian 味（bookworm）的，会把一整层 Debian 包带进构建阶段，而扫描器是**逐个 FROM**
#     上报 CVE 的，白白多出一堆基线漏洞；golang:alpine 拿到的是同样最新的 Go + 最新 alpine，
#     包面最小。
#   - go.mod 要求 go >= 1.27.3，移动 tag 只会往前进，不会低于该版本。
#   - 跨大版本升级会随 tag 自动发生，构建时即可暴露；行为敏感的地方（如 iproute2 输出格式）
#     由构建后冒烟 + 上线前实跑把关。
FROM golang:alpine AS builder

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
# 运行时依赖：
#   iproute2      -> 程序执行的 ip route / ip addr / ip neigh 命令
#   procps-ng     -> 程序执行的 sysctl，以及启动时 pkill -x ndppd（清掉可能残留的 ndppd）
#   ca-certificates/tzdata -> 出站 HTTPS 与本地时区
#
# 注意：不再安装 ndppd。NDP 应答已由内置代答器实现（ndp_responder.go：AF_PACKET 抓 NS、
# 回带 override 的 NA，ndppd 回的 NA 没有 override，CPE 不采纳），main.go 启动时反而会
# pkill 掉 ndppd；镜像里留着它只是多一个常驻/可执行的攻击面。若要用回旧方案，
# 把 ndppd 加回下面的 add 列表即可（alpine 源里没有 ndppd，得回到 debian 基线）。
#
# 安全基线：
#   - 基线用 alpine:latest（移动 tag，跨大版本自动跟进）：运行阶段只要 4 个小包，
#     包面比 debian-slim 小一个数量级，基线镜像自身的 CVE 也随之少。
#     产物是 CGO_ENABLED=0 静态二进制，musl 运行没有任何问题；
#   - `apk upgrade` 先把基线自带包的补丁打齐，避免旧版本包留在运行镜像里；
#   - `--no-cache` + 删 /var/cache/apk：不把包索引和缓存留在层里；
#   - 移动 tag 意味着跨大版本（3.24 → 3.25 …）会随重建自动发生：下面的镜像替换只改主机名，
#     新版 alpine 的 /etc/apk/repositories 结构不变，不用跟着改；
#     升级后跑一次构建 + 冒烟（ip/sysctl/pkill + 二进制起监听）即可确认没被行为变更伤到。
FROM alpine:latest

ENV TZ=Asia/Shanghai

# apk 源：默认换成国内镜像（alpine 官方 CDN 在国内经常几十 KB/s）。
#   - /etc/apk/repositories 里是 dl-cdn.alpinelinux.org 的 main/community 两行，
#     只替换主机名即可一次覆盖；
#   - alpine 自带 ca-certificates-bundle，所以保持 https 没问题；
#   - 海外机器用 --build-arg APK_MIRROR=dl-cdn.alpinelinux.org 还原官方源，传空值则不动。
ARG APK_MIRROR=mirrors.aliyun.com

RUN set -eux; \
    if [ -n "${APK_MIRROR}" ]; then \
        sed -i "s|dl-cdn.alpinelinux.org|${APK_MIRROR}|g" /etc/apk/repositories; \
    fi; \
    apk upgrade --no-cache \
    && apk add --no-cache \
        iproute2 \
        procps-ng \
        ca-certificates \
        tzdata \
    && ln -snf /usr/share/zoneinfo/${TZ} /etc/localtime \
    && echo "${TZ}" > /etc/timezone \
    && rm -rf /var/cache/apk/*

WORKDIR /app

COPY --from=builder /out/go-proxy-ipv6-pool /app/go-proxy-ipv6-pool

# 程序启动时会 godotenv.Load(".env")，读不到会直接退出；
# 这里创建一个空 .env，实际配置通过容器环境变量注入（docker-compose environment / --env-file）
RUN : > /app/.env

# 3128: HTTP 代理，3129: SOCKS5 代理
EXPOSE 3128 3129

CMD ["/app/go-proxy-ipv6-pool"]
