# go-proxy-ipv6-pool-auto

自动 IPv6 代理池：以宿主机公网 IPv6 前缀（通常 /64）里的随机地址作为源地址出站，HTTP 代理 `3128`、SOCKS5 代理 `3129`。

> 基于 [XiaoMiku01/go-proxy-ipv6-pool](https://github.com/XiaoMiku01/go-proxy-ipv6-pool) 的扩展。

## 前置条件（宿主机）

程序会写 `/etc/ndppd.conf` 并重启 ndppd，宿主机需满足：

```bash
sudo sysctl -w net.ipv6.ip_nonlocal_bind=1        # 允许绑定非本机地址作为源地址
apt install ndppd -y                              # NDP 应答，回程流量才回得来
```

持久化：`echo 'net.ipv6.ip_nonlocal_bind=1' | sudo tee /etc/sysctl.d/99-ipv6-pool.conf`

## 随机出口地址为什么要临时挂到网卡上

程序对每个随机出口地址做的是：`ip -6 addr add <随机地址>/64 dev <网卡> nodad` → 发起连接 → `ip -6 addr del`。

原因（实测结论，不是推测）：

- 只加 `ip route replace local <前缀>/64 dev lo` + `ip_nonlocal_bind=1` 时，内核只是把整个 /64 当成本机地址，
  邻居解析只能由 ndppd 用原始套接字代答。抓包能看到光猫一遍遍发 NS、ndppd 也回了规范的 NA
  （源 MAC、TLLAO、solicited 标志都正确），但光猫始终把该地址当成 INCOMPLETE、回程包不转发，
  表现就是 dial 静默超时、几乎 100% 失败。
- 把地址真正挂到接口上后，内核会自动加入对应的 solicited-node 组播组，并用带 override 标志的 NA 应答，
  光猫立刻接受：同一地址 ping/HTTP 立即通（首包因触发邻居解析会丢一次，TCP SYN 重传即可，dial 约 1.1s）。

因此 `NET_ADMIN`（容器，compose 已配）/ root（宿主机直接跑）是硬性要求；
ndppd 与内核 `proxy_ndp` 只在地址挂不上时作为兜底。

### 出口地址池

不可能把整个 /64（2^64 个地址）都绑上去，所以程序预先挂一批随机地址做**常驻池**（默认 128 个），
请求时从池里随机取，并按 TTL 整批轮换：

- 地址常驻后，光猫邻居表命中缓存，**同一地址第二次使用 dial 从 ~1.1s 降到 ~165ms**（首包不再丢）；
- 光猫邻居表条目数稳定在池大小，不会随历史地址不断膨胀；
- 并发请求互不干扰（不必再动接口地址）；
- 进程重启时自动清理上一次残留的池地址（host netns 里的地址不会随容器重启消失），接口地址数不会堆积。

池大小受光猫邻居表容量（家用通常 128~512 条）和内核路由/组播组数量限制，别盲目调大。
想要「每次都是全新地址」，把 `EXIT_POOL_MAX_USES` 设小、`EXIT_POOL_TTL` 设短即可，代价是首包丢失重新出现。

## 启动方式

### Docker（host 网络，推荐）

```bash
cd go-proxy-ipv6-pool-auto
./build.sh                     # 用 golang 容器编译二进制到 ./bin/（宿主机不用装 Go）
docker compose up -d           # compose 把 ./bin 挂载到容器的 /opt/bin 并直接运行它
docker compose logs -f         # 看日志
docker compose down            # 停止
```

- **改 Go 代码不用重建镜像**：`./build.sh && docker compose restart` 即可生效（二进制是挂载进去的）。
- `./bin` 里没有二进制时（例如首次直接 `docker compose up -d --build`），容器自动回落到镜像内置的那份。
- 必须 `network_mode: host`：bridge 模式下容器只有内网 ULA 前缀，出口会被 NAT 成同一个地址；ndppd 也需要看到宿主机真实网卡才能应答 NS。
- host 模式下 compose 的 `sysctls:` 会被 runc 拒绝，`ip_nonlocal_bind` 只能按上文在宿主机设置；容器内 `/proc/sys` 只读，程序只在检测到未开启时给提示，不会阻断启动。
- 端口直接监听宿主机：`3128`（HTTP）/ `3129`（SOCKS5）。
- 仅构建镜像时才会拉 `golang:1.27.3-alpine` 作为构建层（不进入最终镜像），国内默认走 `goproxy.cn`；海外机器可：
  `GOPROXY=https://proxy.golang.org,direct docker compose build --build-arg GOPROXY=https://proxy.golang.org,direct`
- 生产部署若不挂载，用 `docker compose up -d --build` 走镜像内置二进制即可。

### 宿主机直接跑（不经 Docker）

```bash
cd go-proxy-ipv6-pool-auto
./build.sh                             # 产物：bin/go-proxy-ipv6-pool
sudo ./bin/go-proxy-ipv6-pool --port 3128 --prefix 64   # 需要 root：改路由/接口地址 + 起 ndppd
```

### 本地编译运行

```bash
cd go-proxy-ipv6-pool-auto/go-proxy-ipv6-pool
printf 'NET_IF=eth0\nRUN_ENV=prod\n' > .env          # 启动时会 godotenv.Load(".env")，缺了直接退出
go build -o go-proxy-ipv6-pool .
sudo ./go-proxy-ipv6-pool --port 3128 --prefix 64    # 需要 root：改路由 + 起 ndppd
```

## 使用

```bash
curl -x http://<host>:3128 https://v6.mypublicipnow.com/     # HTTP 代理
curl -x socks5h://<host>:3129 https://v6.mypublicipnow.com/  # SOCKS5 代理
```

失败时返回 502/504（`X-Proxy-Error` 头带原因），不会静默回落到宿主机默认出口。

## 出口连通性诊断脚本

`analyze_exit_ipv6.py` 抽样随机出口地址，对比成功/失败的地址特征，并抓包判定失败环节：

```bash
./analyze_exit_ipv6.py                          # 默认先挂地址再探测（与程序一致），20 个样本
./analyze_exit_ipv6.py -n 40 --interval 1 -o /tmp/exit.csv
./analyze_exit_ipv6.py --no-add-addr            # 只测 local 路由 + ndppd 的代答路径（旧行为）
./analyze_exit_ipv6.py --no-capture             # 不抓包，只看连通性与地址规律
```

- 非 root 时会自动起一个 `ipv6-exit-probe-shell` 容器（host 网络 + NET_ADMIN）来挂地址和 ping，结束时删除；
- `--capture`（默认开启）再起一个抓包容器统计 `echo request / echo reply / NS / NA`，直接给出失败环节：
  `CPE 丢弃（无邻居解析）` / `无 NDP 应答` / `CPE 收到 NA 仍未转发回程`。

## 配置

| 配置 | 说明 |
| --- | --- |
| `NET_IF`（环境变量 / `.env`） | 用于推断池前缀的网卡，默认 `eth0` |
| `--port` | HTTP 代理端口，SOCKS5 自动用 `端口+1`，默认 `3128` |
| `--prefix` | 前缀长度，默认 `64` |
| `EXIT_POOL_SIZE` | 常驻出口地址池大小，默认 `128`（受光猫邻居表容量限制，建议 64~512） |
| `EXIT_POOL_TTL` | 池整批轮换周期（秒），默认 `600` |
| `EXIT_POOL_MAX_USES` | 单个地址最多被使用次数，用满即剔除并补新，默认 `50` |
| `DIAL_TIMEOUT_SECONDS` | 单个随机出口的拨号超时，默认 `5`（坏出口直接换，不必长等） |
| `DIAL_MAX_ATTEMPTS` | 单次请求最多尝试的随机出口数量，默认 `5` |
