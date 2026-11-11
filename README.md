# go-proxy-ipv6-pool-auto

自动 IPv6 代理池：以宿主机公网 IPv6 前缀（通常 /64）里的随机地址作为源地址出站，HTTP 代理 `3128`、SOCKS5 代理 `3129`。

> 基于 [XiaoMiku01/go-proxy-ipv6-pool](https://github.com/XiaoMiku01/go-proxy-ipv6-pool) 的扩展。

## 前置条件（宿主机）

```bash
sudo sysctl -w net.ipv6.ip_nonlocal_bind=1        # 允许绑定本机网卡上不存在的地址作为源地址
```

持久化：`echo 'net.ipv6.ip_nonlocal_bind=1' | sudo tee /etc/sysctl.d/99-ipv6-pool.conf`

其余由程序自己准备（`NET_ADMIN` / `NET_RAW`、容器里 root 是硬性要求）：

- 默认后端 `ndppd`：加 `local <前缀>/64 dev lo` 路由，按当前前缀生成 `/etc/ndppd.conf`，
  把 ndppd 拉起来对**整段 /64** 代答 → 内核 neigh 表一条都不写；
  另外把网卡置为 `allmulticast`，否则收不到发往任意 solicited-node 组播组的 NS；
- 后端 `kernel`：额外把 `net.ipv6.conf.<网卡>.proxy_ndp` 置 1，用哪个出口地址就给它 `ip -6 neigh add proxy`。

## 回程 NS 由谁代答：ndppd（默认）/ userspace / kernel

出口地址不在网卡上，CPE 回程前要先 `NS` 解析它，**谁应答决定了能不能覆盖整个 `/64`**：

| 后端 | 覆盖范围 | 内核表开销 | 实测（本机，同一 `/64`） |
| --- | --- | --- | --- |
| `ndppd`（默认） | 整个 `/64`，任意地址 | **0 条** pneigh、网卡上只有本机真实地址的 2 个 solicited-node 组播组 | HTTP 3128 **10/10 且 10 个不同出口地址**、再加压 **30/30**；SOCKS5 **5/5**；`ip -6 neigh show proxy` = 0；`Icmp6OutMLDv2Reports` 10s 增量 = 0 |
| `userspace`（内置代答器，兜底） | 整个 `/64` | 同样 0 条 | 同一组测试也全通过，但多一套自己维护的原始套接字代码，非必要不用 |
| `kernel`（内核 proxy_ndp） | 只有已 add 的地址 | 用过的地址各一条 pneigh，每条占一个 solicited-node 组播组 | 只适合小规模；条目数=用过的地址数 |

**内核 `proxy_ndp` 没有「整段通配」**：它只对 pneigh 表里存在的地址回 NA，而一个 `/64` 不可能
一条条 add 进去（38 万条时 `ip -6 neigh show proxy` 要十几秒、`ip -6 neigh flush proxy` 13 分钟
只删 2072 条，插入/删除本身还会不停 join/leave 组播组，把 `kworker/*+mld` 打满）。
所以默认走 `ndppd`：整段代答，内核零条目。

`ndppd` 的配置由程序生成（前缀变化时会重写并重启 ndppd）：

```
route-ttl 30000

proxy <网卡> {
    router no
    timeout 500
    ttl 30000

    rule <前缀>/64 {
        static
    }
}
```

`static` 表示对该前缀内任意地址的 NS 直接回 NA、不做 per-address 状态跟踪 —— 出口地址数量
再怎么增长，ndppd 自己也不会变慢。ndppd 启动时会打一条
`(warning) Low prefix length (64 <= 120) when using 'static' method`，
这是它提示「static 通常配窄段」，对「整段 /64 代答」正是想要的行为，可以忽略。

前提是「代答器要能收到 NS」：NS 发往目标地址的 solicited-node 组播组（`ff02::1:ffXX:XXXX`），
没加入该组的帧会在网卡层被过滤。整段 `/64` 不可能逐个加组（那等于把组播组表撑爆），
因此程序会把网卡置为 `allmulticast`（`NDP_ALLMULTICAST=on`，默认）。
需要还原时：`ip link set dev <网卡> allmulticast off`。

## 为什么不用 `ip -6 addr add/del` 把出口地址挂到网卡上

早期版本对每个随机出口地址做 `ip -6 addr add <地址>/64 dev <网卡> nodad` → 连接 → `ip -6 addr del`。
它确实能通（内核会为该地址加入 solicited-node 组播组，并用带 override 的 NA 应答，CPE 立刻采纳，
dial 约 1.1s），但两个问题无法接受：

- netlink **地址事件**会让 Chromium 判定「网络已变化」，在途请求全部 `ERR_NETWORK_CHANGED`；
- 同样是一个地址一次 add/del，规模一大就卡死，和逐条 `ip -6 neigh add proxy` 是同一类问题。

所以现在出口地址**不落到网卡上**：只加 `local <前缀>/64 dev lo` 路由 + `ip_nonlocal_bind=1`，
回程 NS 交给 ndppd（默认，整段 `/64` 一条内核条目都不用写）。

`NET_ADMIN`（容器，compose 已配）/ root（宿主机直接跑）是硬性要求。

### 出口地址（默认每请求一个 /64 内全新地址）

地址的取样空间始终是整个 `/64`，各后端的默认行为不同：

| 后端 | `EXIT_POOL_SIZE` 留空时 | 说明 |
| --- | --- | --- |
| `ndppd`（默认）/ `userspace` | `0` = **每个请求都在 `/64` 内取全新随机地址** | 代答不产生内核条目，地址可以完全不重复，这才是「用满整个 /64」 |
| `kernel` | `512` | 每个地址要一条 pneigh，只能靠池复用把条目数压住 |

整段代答（`ndppd`/`userspace`）下如果配了 `EXIT_POOL_SIZE>0`，池内地址按 `EXIT_POOL_RENEW_SECONDS`（默认 600s）
滚动轮换（到期换出、在 `/64` 内补新地址），可用于需要「固定一批出口」的场景；
`EXIT_POOL_RENEW_SECONDS=0` 关闭轮换。

### kernel 后端的条目管理（仅 `NDP_BACKEND=kernel`）

为什么 kernel 后端必须池化：**每条 proxy_ndp 条目都会让网卡维护一个 solicited-node 组播组
（ff02::1:ffXX:XXXX）**，组播组还要被 MLDv2 report 周期性重报，删除时又要逐个 leave + 遍历内核表。
地址数无限增长 → 必然把 mld 工作队列和 neigh 表打满：

| 指标 | 每请求新地址（kernel） | 地址池 512 |
| --- | --- | --- |
| 内核 pneigh 条目 | 384540 条（`proxyNeighborMax=4096` 形同失效） | ≈512 |
| 网卡 solicited-node 组播组 | ≥39789 个 | 池大小时 |
| `kworker/*+mld` | 常驻 100% CPU | 接近 0 |
| `ip -6 maddr show` / `ip -6 neigh show proxy` | 8 分钟 / 十几秒 | 秒级 |

池内细节：

- 同一地址在 **5 分钟刷新窗口**内被复用时不再执行 `ip`（同一地址每秒可能命中几十次）；
- **池内地址不参与 TTL 回收**：它会被立刻复用，删掉再建只会让 CPE 邻居缓存过期后的首包丢掉；
- 换前缀（光猫重拨）时整池重建，旧池地址落在已作废的 `/64` 里没有意义；
- 轮换出去的地址立刻失去池保护，等账本 TTL（10 分钟）到期后由 reaper 分批删掉它的 pneigh 条目。

### 条目账实一致与熔断（仅 `NDP_BACKEND=kernel`）

条目不会自己消失，进程重启也不回收，所以后台按 TTL（10 分钟）分批删。删除的记账规则是
**「只有 `ip -batch` 确认删掉才从内存账本销账」**：

- 旧实现是「先删账本、再执行 `ip`」。`ip` 在几十万条的表上 30s 超时被 kill（日志里的
  `signal: killed`）之后账本已经清空，内核条目永久残留 —— 这就是 pneigh 堆到 38 万条的直接原因；
- 现在删除失败（超时/权限）一律保留账本等下一轮重试；
- 连续 3 轮删除失败 → 整表 `ip -6 neigh flush proxy` 熔断，让账本与内核表都归零；
- 「条目本来就不存在」（`No such file or directory`）视为删除成功，否则一次 kill 留下的幽灵
  条目会让之后每轮批量删除都失败。

### 存量几十万条怎么清（维护窗口，仅 kernel 后端）

`ndppd` / `userspace` 后端不写任何内核条目，**不会再产生存量**；但如果机器上已经堆积了几十万条历史 pneigh
（旧版本遗留），它们属于内核 neigh 表，切换后端也不会自动消失，仍需清理一次。
启动 kernel 后端时会先数条数（`ip -6 neigh show proxy dev <网卡>`）：

- ≤5000 条：直接整表 flush 清掉；
- \>5000 条：**只打醒目告警并跳过**。这种规模下 flush 近似 O(n²)（实测 38 万条 13 分钟只删
  2072 条，约 2.7 条/秒），让程序同步清理只会卡住启动，必须一次性重建接口的 IPv6：
  备份 `ip -6 addr` / `ip -6 route show default` → `sysctl -w net.ipv6.conf.<网卡>.disable_ipv6=1`
  → 改回 `0`（确认 SSH 走 IPv4 或有带外/本地终端），或直接重启宿主。
  联调环境实测重建后 pneigh=0、`/proc/net/igmp6` 只剩 29 行、MLD report 归零，地址与默认路由自动恢复。

### 光猫重拨 / 换前缀（旧前缀导致全部 i/o timeout）

公网前缀由运营商 RA 下发，光猫重启或重拨后会变（如 `2409:8a1e:2e94:7420::/64` → `2409:8a1e:2e93:4fe0::/64`）。
换前缀后网卡上旧前缀的 SLAAC 地址还会以 `deprecated` 状态残留一段时间，若继续拿它当池前缀，
随机出口全落在已经作废的 /64 里，表现就是所有请求 `dial ... i/o timeout`。

因此选前缀只采用「未 deprecated / 未 tentative / 未 dadfailed」的地址（稳定地址优先），并在这些时机切换：

- 定时复查（`POOL_CHECK_INTERVAL`，默认 300s）；
- 拨号全部失败时立刻触发一次复查（10s 冷却），不用等满一个周期；
- 切换后同步改写 local 路由、删掉旧前缀路由、更新 NDP 代答段，无需重启进程。

> 为什么不用 `ip -6 addr add` 把出口地址挂到网卡上：这样确实能通（约 1.1s），但 netlink
> **地址事件**会让 Chromium 判定「网络已变化」→ 在途请求全部 `ERR_NETWORK_CHANGED`，
> 而且同样是一条条加、一条条删，规模一大就卡。

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
- 必须 `network_mode: host`：bridge 模式下容器只有内网 ULA 前缀，出口会被 NAT 成同一个地址；内核 `proxy_ndp` / NDP 代答也必须作用在宿主机真实网卡上。
- host 模式下 compose 的 `sysctls:` 会被 runc 拒绝，`ip_nonlocal_bind` 只能按上文在宿主机设置；容器内 `/proc/sys` 只读，程序只在检测到未开启时给提示，不会阻断启动。
- 端口直接监听宿主机：`3128`（HTTP）/ `3129`（SOCKS5）。
- 仅构建镜像时才会拉 `golang:1.27.3-alpine` 作为构建层（不进入最终镜像），国内默认走 `goproxy.cn`；海外机器可：
  `GOPROXY=https://proxy.golang.org,direct docker compose build --build-arg GOPROXY=https://proxy.golang.org,direct`
- 生产部署若不挂载，用 `docker compose up -d --build` 走镜像内置二进制即可。

### 宿主机直接跑（不经 Docker）

```bash
cd go-proxy-ipv6-pool-auto
./build.sh                             # 产物：bin/go-proxy-ipv6-pool
sudo ./bin/go-proxy-ipv6-pool --port 3128 --prefix 64   # 需要 root：改 local 路由 + 写入内核 neigh 表
```

### 本地编译运行

```bash
cd go-proxy-ipv6-pool-auto/go-proxy-ipv6-pool
printf 'NET_IF=%s\nRUN_ENV=prod\n' "$(ip -6 route show default | awk '{for(i=1;i<NF;i++) if ($i=="dev"){print $(i+1); exit}}')" > .env
# ↑ 启动时会 godotenv.Load(".env")，缺了直接退出；NET_IF 必须按本机网卡名填（eth0 / enp2s0 / ens18 …）
go build -o go-proxy-ipv6-pool .
sudo ./go-proxy-ipv6-pool --port 3128 --prefix 64    # 需要 root：改 local 路由 + 起用户态代答器 + 置 allmulticast
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
./analyze_exit_ipv6.py --no-add-addr            # 只测 local 路由 + 用户态代答路径（旧行为）
./analyze_exit_ipv6.py --no-capture             # 不抓包，只看连通性与地址规律
```

- 非 root 时会自动起一个 `ipv6-exit-probe-shell` 容器（host 网络 + NET_ADMIN）来挂地址和 ping，结束时删除；
- `--capture`（默认开启）再起一个抓包容器统计 `echo request / echo reply / NS / NA`，直接给出失败环节：
  `CPE 丢弃（无邻居解析）` / `无 NDP 应答` / `CPE 收到 NA 仍未转发回程`。

## 配置

| 配置 | 说明 |
| --- | --- |
| `NET_IF`（环境变量 / `.env`，可选） | 用于推断池前缀的网卡。**一般不填**：程序自己判断（默认 IPv6 路由 → 带公网 IPv6 的物理网卡 → 默认 IPv4 路由，排除 lo/docker/br-/veth 等虚拟网卡），启动日志打印「自动探测到出口网卡：xxx」。多块物理网卡想固定走某一块时才填，查法：`ip -6 route show default` 里 `dev` 后面的名字 |
| `--port` | HTTP 代理端口，SOCKS5 自动用 `端口+1`，默认 `3128` |
| `--prefix` | 前缀长度，默认 `64` |
| `DIAL_TIMEOUT_SECONDS` | 单个随机出口的拨号超时，默认 `5`（坏出口直接换，不必长等） |
| `DIAL_MAX_ATTEMPTS` | 单次请求最多尝试的随机出口数量，默认 `5` |
| `NDP_BACKEND` | 回程 NS 代答后端：`ndppd`（默认，整段 `/64` 由 ndppd 代答、内核零条目）/ `userspace`（内置代答器兜底）/ `kernel`（内核 proxy_ndp，逐条 pneigh）/ `auto`（proxy_ndp 能开就用 kernel） |
| `NDP_ALLMULTICAST` | 整段代答（`ndppd`/`userspace`）是否把网卡置为 allmulticast，默认 `on`（必须能收到任意 solicited-node 组播组的 NS） |
| `NDPD_CONF` | ndppd 配置文件路径，默认 `/etc/ndppd.conf`（由程序按当前前缀自动生成/重写） |
| `EXIT_POOL_SIZE` | 同时活跃的出口地址数，留空 = 按后端取默认（`ndppd`/`userspace` 为 `0`，即每请求一个 `/64` 内全新地址；`kernel` 为 `512`）。填 `>0` 才收窄成固定池 |
| `EXIT_POOL_RENEW_SECONDS` | 池内地址轮换周期（秒），默认 `600`（仅在配了池时生效）；到期换出池并在 `/64` 内补新地址，`0` = 不轮换 |
| `NDP_RESPONDER` | 仅 kernel 后端：内置代答器 `auto`（默认，`proxy_ndp` 可用则不启）/ `on` / `off` |
| `POOL_CHECK_INTERVAL` | 池前缀复查间隔（秒），默认 `300`；拨号全部失败时还会额外触发一次即时复查（10s 冷却） |
| `RUN_ENV` / `DEBUG` | 日志级别。默认（`RUN_ENV=prod`）不打印每请求日志；`RUN_ENV=dev` 或 `DEBUG=true` 时打印每条 HTTP/CONNECT/SOCKS5 请求及每个随机出口的拨号明细，排障用 |
