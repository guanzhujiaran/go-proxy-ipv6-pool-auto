package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 随机出口地址不在网卡上，回程包要靠邻居解析把 CPE 引回来。
// 同一台光猫、同一前缀下的实测结论（三选一，别再来回改）：
//
//  1. 只加 `local <前缀>/64 dev lo` + 用户态 NA 代答（ndppd / 内置代答器）：
//     CPE 不采纳，NS 会一遍遍重发，dial 几乎 100% i/o timeout；
//  2. 把随机地址 `ip -6 addr add` 到网卡：CPE 立刻采纳（~1s 建连），
//     但 netlink 地址事件会让 Chromium 判定「网络已变化」，在途请求全部 ERR_NETWORK_CHANGED；
//  3. 内核 proxy_ndp（本方案）：CPE 采纳内核发的 NA（实测冷地址 0.6s 建连），
//     而且只产生 neigh 事件、不产生地址事件 → Chromium 无感。
//
// 所以出口地址改成：拨号前 `ip -6 neigh add proxy <出口地址> dev <网卡>`，
// 交给内核代答 NS；请求路径上照旧只 bind、不改网卡地址。
//
// 但内核 proxy_ndp 不是免费的：**每条 pneigh 都会让网卡维护一个对应的 solicited-node
// 组播组（ff02::1:ffXX:XXXX）**，组播组本身还会被 MLDv2 report 周期性重报。
// 「每请求一个全新随机地址」在几千 QPS 下会把 neigh 表与组播组表撑到几十万条：
//
//	ip -6 neigh show proxy           → 384540 条（proxyNeighborMax=4096 形同失效）
//	enp2s0 上 solicited-node 组播组   → ≥39789 个
//	kworker/*+mld                    → 常驻 100% CPU（每个组的 periodic report + leave 遍历）
//	ip -6 neigh show proxy / maddr    → 十几秒~8 分钟才返回
//
// 根因是「地址无限增长 + 删除跟不上」：ip 命令在几十万条的表上 30s 超时被 kill，
// 而旧清理逻辑是「先删账本、再执行 ip -batch」，kill 之后账实脱节，条目永久残留。
//
// 因此默认改走 userspace 后端（ndp_backend.go）：整段 /64 由内置代答器代答，内核一条都不写，
// 「/64 不可能一条条 add」的问题从根上消失。下面的池与账本逻辑是 kernel 后端
// （NDP_BACKEND=kernel）的场景，两者并存：
//
//	A. 出口地址池（pickExitIP）：kernel 后端下活跃出口地址数被 EXIT_POOL_SIZE 限死（默认 512），
//	   池内随机复用，不再每请求新建地址 → 组播组数量不再随请求数增长；
//	B. 账实一致（pruneProxyNeighbors）：只有 ip -batch 确认删掉才销账，
//	   连续失败到阈值就整表 flush 熔断，不再出现「账本没了、内核里还在」；
//	C. 启动残留清理（resetProxyNeighbors）：先数条数，大表只告警不同步 flush
//	   （`ip -6 neigh flush proxy` 在大表上近似 O(n²)，实测 38 万条 13 分钟只删 2072 条）。
const (
	// proxyNeighborTTL 代理邻居条目的保留时长：CPE 侧邻居缓存过期后会重新发 NS，
	// 期间条目还在就能继续被代答；已结束的连接不再需要，所以可以直接过期释放。
	proxyNeighborTTL = 10 * time.Minute
	// proxyNeighborMax 代理邻居条目上限（含池内地址），防止长期运行把内核 neigh 表堆满
	proxyNeighborMax = 4096
	// proxyNeighborReapSeconds 过期清理周期（秒）
	proxyNeighborReapSeconds = 60
	// proxyNeighborMaxDeletes 单轮最多删多少条：删除要跑一次 `ip -batch`，
	// 一次塞几千行会让命令跑进超时区，分几轮删完即可。
	proxyNeighborMaxDeletes = 500
	// proxyNeighborRefresh 刷新窗口：同一出口地址在窗口内被复用时不再 exec `ip`。
	// 池化后同一地址每秒可能被命中几十次，不跳过的话 `ip` 进程会被打满。
	// 窗口必须小于 TTL，否则条目会在「还热着」的时候被 TTL 回收掉。
	proxyNeighborRefresh = 5 * time.Minute
	// proxyNeighborFailThreshold 连续多少轮批量删除失败后触发整表 flush 熔断
	proxyNeighborFailThreshold = 3
	// proxyNeighborFlushSkipThreshold 启动时残留条目超过该数量就不同步清理：
	// 这种规模下 flush 要卡分钟级且几乎删不动，只能交给维护窗口。
	proxyNeighborFlushSkipThreshold = 5000
	// proxyNeighborOpTimeout 单条 neigh 操作（replace/del）的超时
	proxyNeighborOpTimeout = 30 * time.Second
	// proxyNeighborBatchTimeout `ip -batch` 批量删除的超时
	proxyNeighborBatchTimeout = 60 * time.Second
	// proxyNeighborFlushTimeout 整表 flush 的超时：小表（几条~几千条）也要留足余量
	proxyNeighborFlushTimeout = 180 * time.Second
	// proxyNeighborProbeTimeout 读表格（`ip -6 neigh show proxy`）的超时。
	// 大表上这个命令要十几秒，超时即视为「表很大」，转去告警而不是继续 flush。
	proxyNeighborProbeTimeout = 20 * time.Second
	// cmdOutputLimit 日志里单条命令输出最多保留多少字节：
	// 大表上 `ip -6 neigh show proxy` 一次能吐出几十万行，全打进日志会把磁盘写满。
	cmdOutputLimit = 512
)

var proxyNdp struct {
	mu sync.Mutex
	// enabled 内核 proxy_ndp 是否已开启（未开启时出口地址没有回程保障）
	enabled bool
	// warned 只在第一次发现 proxy_ndp 未开启时告警，避免每请求刷屏
	warned bool
	// entries 内存账本：出口地址 → 最近一次成功写入内核表的时间
	entries map[string]time.Time
	// deleteFailures 连续删除失败的轮数，成功一轮即清零
	deleteFailures int
}

func init() {
	proxyNdp.entries = make(map[string]time.Time)
}

// proxyNdpNow 时间源，抽成变量便于单测模拟「过了刷新窗口 / 过了 TTL」
var proxyNdpNow = time.Now

// 命令执行入口抽成变量：单测替换成假实现，避免真的去改内核 neigh 表。
var (
	proxyNdpExec      = runCmdWithTimeout
	proxyNdpExecInput = runCmdInputWithTimeout
)

// poolAddr 池内一个出口地址：addedAt 决定它什么时候被轮换掉
type poolAddr struct {
	ip      string
	addedAt time.Time
}

// exitPool 出口地址池。
// 取样空间是整个 /64（每次扩建都在 /64 内随机取），池内地址复用，
// 并且按 EXIT_POOL_RENEW_SECONDS 滚动轮换：老地址退出池、新地址补进来。
// 这样带来的出口地址多样性远大于「固定几个地址」，同时任意时刻的活跃 pneigh
// 数量被 EXIT_POOL_SIZE 限住 → 网卡 solicited-node 组播组数量不随请求数增长，
// MLDv2 report 速率也就稳得住（组播组不新增，就没有新的 periodic report 源）。
var exitPool struct {
	mu   sync.Mutex
	cidr string
	// ips 按加入时间排序（下标 0 最旧），轮换时从头部弹出
	ips  []poolAddr
	seen map[string]struct{}
}

// exitPoolLimit 出口地址池大小；0 表示退回「每请求全新随机地址」的旧行为（不推荐）。
// 由 initExitPool 在加载 .env 之后从 EXIT_POOL_SIZE 读取。
var exitPoolLimit = defaultExitPoolSize

// exitPoolRenew 池内地址的轮换周期：0 表示不轮换（地址永久留在池里）。
// 由 initExitPool 从 EXIT_POOL_RENEW_SECONDS 读取（默认 600s）。
var exitPoolRenew = defaultPoolRenew

const (
	// defaultExitPoolSize 同时活跃的出口地址数默认值
	defaultExitPoolSize = 512
	// defaultPoolRenew 池内地址默认轮换周期：10 分钟后换一批新地址，
	// 出口地址因此持续在 /64 里滚动，而不是长期固定同一批。
	// 不能配得太短：每个新地址都要写一次 pneigh（冷地址首包会丢一次），
	// 轮换越快冷启动拨号越多。
	defaultPoolRenew = 10 * time.Minute
)

// exitPoolSize 读取配置的出口地址池大小：
//   - 缺省 512；
//   - 0 = 不走池，退回「每请求全新随机地址」（= 每次都在整个 /64 里取，但 pneigh 随请求增长）；
//   - 大于 0 时不设上限：池的取样空间本来就是整个 /64，想同时铺开更多地址就调大它。
func exitPoolSize() int {
	raw := strings.TrimSpace(os.Getenv("EXIT_POOL_SIZE"))
	if raw == "" {
		return defaultExitPoolSize
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		log.Printf("EXIT_POOL_SIZE=%q 非法（应为 >=0 的整数），回退默认值 %d", raw, defaultExitPoolSize)
		return defaultExitPoolSize
	}
	if n > proxyNeighborMax {
		// 不压制：出口地址空间是整个 /64，池大小由使用方决定。
		// 但每条 pneigh 都会让网卡多维护一个 solicited-node 组播组，并参与 MLDv2 周期重报，
		// 活跃地址越多 kworker/*+mld 越吃 CPU（现场 3.9 万个组时已打满），这里只告警不拦。
		log.Printf("警告：EXIT_POOL_SIZE=%d 大于 proxyNeighborMax=%d："+
			"每条 proxy_ndp 条目都会让网卡多维护一个 solicited-node 组播组（ff02::1:ffXX:XXXX）并参与 MLDv2 周期重报，"+
			"同时活跃的出口地址越多，kworker/*+mld 与 `ip -6 maddr show` 的开销越高，请按本机 CPU 余量评估", n, proxyNeighborMax)
	}
	return n
}

// exitPoolRenewSeconds 读取池内地址轮换周期（秒）：缺省 600，0 = 不轮换
func exitPoolRenewSeconds() int {
	raw := strings.TrimSpace(os.Getenv("EXIT_POOL_RENEW_SECONDS"))
	if raw == "" {
		return int(defaultPoolRenew / time.Second)
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		log.Printf("EXIT_POOL_RENEW_SECONDS=%q 非法（应为 >=0 的整数），回退默认值 %d",
			raw, int(defaultPoolRenew/time.Second))
		return int(defaultPoolRenew / time.Second)
	}
	return n
}

// initExitPool 从环境变量初始化出口地址池，需在 godotenv 与 initNdpBackend 之后调用
func initExitPool() {
	exitPoolRenew = time.Duration(exitPoolRenewSeconds()) * time.Second
	// 留空 = 按后端取默认值：ndppd/userspace 为 0（全 /64），kernel 为 512（必须池化）
	if ndpNoKernelEntries() && strings.TrimSpace(os.Getenv("EXIT_POOL_SIZE")) == "" {
		exitPoolLimit = 0
	} else {
		exitPoolLimit = exitPoolSize()
	}

	// ndppd / userspace：不写内核条目，池化「为了少写 pneigh」的前提不存在，
	// 0（默认）就是每个请求都在整个 /64 里取全新随机地址，地址空间不打折。
	if ndpNoKernelEntries() {
		if exitPoolLimit <= 0 {
			log.Printf("出口地址：每个请求都在整个 /64（2^64 个地址）内取全新随机地址，" +
				"由 ndppd 整段代答，不产生任何内核 neigh 条目")
			return
		}
		log.Printf("出口地址池：同时 %d 个地址（EXIT_POOL_SIZE 显式收窄），取样空间仍是整个 /64",
			exitPoolLimit)
		return
	}

	// kernel 后端：每个地址都要一条 pneigh，必须池化，0 会把内核表撑爆
	if exitPoolLimit <= 0 {
		log.Printf("警告：EXIT_POOL_SIZE=0 且 NDP_BACKEND=kernel：内核 pneigh / solicited-node 组播组" +
			"会随请求数无限增长（曾堆到 38 万条并打满 mld 工作队列）；" +
			"整段代答请用 NDP_BACKEND=ndppd，或把 EXIT_POOL_SIZE 配成 >0")
		return
	}
	if exitPoolRenew > 0 {
		log.Printf("出口地址池：同时 %d 个地址（EXIT_POOL_SIZE），每 %s 轮换一批新地址（EXIT_POOL_RENEW_SECONDS），"+
			"取样空间为整个 /64，池内随机复用", exitPoolLimit, exitPoolRenew)
		return
	}
	log.Printf("出口地址池：同时 %d 个地址（EXIT_POOL_SIZE），不轮换（EXIT_POOL_RENEW_SECONDS=0），取样空间为整个 /64",
		exitPoolLimit)
}

// rotateExitPool 把超过轮换周期的地址从池里弹出（调用方必须已持有 exitPool.mu）。
// 弹出后这些地址不再受池保护，等账本 TTL 到期后由 reaper 分批删掉其 pneigh 条目；
// 下一次 pickExitIP 会在 /64 里补新的随机地址进来，出口地址因此持续滚动。
func rotateExitPool(now time.Time) int {
	if exitPoolRenew <= 0 {
		return 0
	}
	rotated := 0
	for len(exitPool.ips) > 0 && now.Sub(exitPool.ips[0].addedAt) >= exitPoolRenew {
		delete(exitPool.seen, exitPool.ips[0].ip)
		exitPool.ips = exitPool.ips[1:]
		rotated++
	}
	return rotated
}

// pickExitIP 取一个出口源地址：池未满就新建一个，池满了随机复用池内地址；
// 池内地址超过轮换周期会被换掉，可用地址因此覆盖整个 /64。
// EXIT_POOL_SIZE=0 时退回「每请求生成全新随机地址」。
func pickExitIP(cidr string) (string, error) {
	if exitPoolLimit <= 0 {
		return generateRandomIPv6(cidr)
	}
	if cidr == "" {
		return "", fmt.Errorf("池前缀为空，无法选择出口地址")
	}

	now := proxyNdpNow()
	exitPool.mu.Lock()
	defer exitPool.mu.Unlock()

	if exitPool.cidr != cidr || exitPool.seen == nil {
		if exitPool.cidr != "" && exitPool.cidr != cidr {
			log.Printf("出口地址池：前缀 %s -> %s，重建（旧池 %d 个地址落在已作废的 /64 里）",
				exitPool.cidr, cidr, len(exitPool.ips))
		}
		exitPool.cidr = cidr
		exitPool.ips = nil
		exitPool.seen = make(map[string]struct{}, exitPoolLimit)
	}

	// 先把过期的老地址换出去，再由下面的逻辑补新地址进来
	if rotated := rotateExitPool(now); rotated > 0 {
		log.Printf("出口地址池轮换：%d 个地址已用满 %s，退出池并在 /64 内补新地址（当前池 %d/%d）",
			rotated, exitPoolRenew, len(exitPool.ips), exitPoolLimit)
	}

	if len(exitPool.ips) < exitPoolLimit {
		// 生成不重复的地址（/64 里撞重几乎不可能，但重复地址会让两条连接共用一个出口）
		for attempt := 0; attempt < 8; attempt++ {
			ip, err := generateRandomIPv6(cidr)
			if err != nil {
				return "", err
			}
			if _, dup := exitPool.seen[ip]; dup {
				continue
			}
			exitPool.seen[ip] = struct{}{}
			exitPool.ips = append(exitPool.ips, poolAddr{ip: ip, addedAt: now})
			if len(exitPool.ips) == exitPoolLimit {
				log.Printf("出口地址池已建满 %d 个地址（前缀 %s），后续请求复用池内地址", exitPoolLimit, cidr)
			}
			return ip, nil
		}
		if len(exitPool.ips) == 0 {
			// 池一个都没建起来时不能死循环，退回不记池的随机地址
			return generateRandomIPv6(cidr)
		}
	}
	return exitPool.ips[rand.IntN(len(exitPool.ips))].ip, nil
}

// exitPoolHas 判断地址是否属于当前出口地址池。
// 池内地址会被反复复用，因此不参与 TTL 回收与上限淘汰：删掉下次请求还要重建，
// 而 CPE 侧邻居缓存早已过期，重建后的首个 NS/首包必丢。
//
// 加锁顺序固定为 proxyNdp.mu → exitPool.mu（本函数的调用方都先拿 proxyNdp.mu），
// 不要写出反向顺序。
func exitPoolHas(ip string) bool {
	if exitPoolLimit <= 0 {
		return false
	}
	exitPool.mu.Lock()
	defer exitPool.mu.Unlock()
	_, ok := exitPool.seen[ip]
	return ok
}

// proxyNdpProcPath proxy_ndp 开关对应的 /proc 路径（读不需要特权）
func proxyNdpProcPath() string {
	return fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/proxy_ndp", netIf)
}

// ensureProxyNdp 打开网卡的 proxy_ndp，让内核代替用户态代答 NS。
// host 网络模式下该开关与宿主机共享 netns，容器内写不动时降级为上宿主配置，不阻断启动。
func ensureProxyNdp() bool {
	if netIf == "" {
		log.Printf("警告：未探测到网卡，proxy_ndp 无法开启（回程 NS 只能靠内置代答器，CPE 多半不采纳）")
		return false
	}
	if data, err := os.ReadFile(proxyNdpProcPath()); err == nil && strings.TrimSpace(string(data)) == "1" {
		proxyNdpSetEnabled(true)
		log.Printf("proxy_ndp 已开启（网卡 %s）：随机出口的 NS 由内核代答", netIf)
		return true
	}

	key := fmt.Sprintf("net.ipv6.conf.%s.proxy_ndp", netIf)
	output, err := runCmd("sysctl", "-w", key+"=1")
	if err == nil {
		if data, rerr := os.ReadFile(proxyNdpProcPath()); rerr == nil && strings.TrimSpace(string(data)) == "1" {
			proxyNdpSetEnabled(true)
			log.Printf("proxy_ndp 已开启（网卡 %s）：随机出口的 NS 由内核代答", netIf)
			return true
		}
	}
	log.Printf("警告：%s 开启失败（%v）：%s", key, err, strings.TrimSpace(output))
	log.Printf("警告：请在宿主机执行 sudo sysctl -w %s=1 后重启容器；否则随机出口回程包会被 CPE 丢弃（dial i/o timeout）", key)
	proxyNdpSetEnabled(false)
	return false
}

func proxyNdpSetEnabled(v bool) {
	proxyNdp.mu.Lock()
	proxyNdp.enabled = v
	proxyNdp.mu.Unlock()
}

func proxyNdpEnabled() bool {
	proxyNdp.mu.Lock()
	defer proxyNdp.mu.Unlock()
	return proxyNdp.enabled
}

// proxyNeighborAdd 让内核为出口地址代答 NS；返回 false 表示该地址没有回程保障。
// 用 replace 保证幂等：同一地址被重复使用时 add 会报 File exists。
// 刷新窗口（proxyNeighborRefresh）内重复命中同一地址直接跳过 exec：
// 池化后同一地址每秒会被命中几十次，每次都 exec 一个 `ip` 进程会把 CPU 打满。
func proxyNeighborAdd(ip string) bool {
	if netIf == "" || ip == "" {
		return false
	}
	if ndpNoKernelEntries() {
		// 整段 /64 由内置代答器覆盖，不需要为单个地址写内核条目：
		// 这正是「一个 /64 不可能一条条 add」的解法，内核 neigh 表保持为空。
		return true
	}
	if !proxyNdpEnabled() {
		proxyNdp.mu.Lock()
		warned := proxyNdp.warned
		proxyNdp.warned = true
		proxyNdp.mu.Unlock()
		if !warned {
			log.Printf("警告：proxy_ndp 未开启，随机出口回程包大概率被 CPE 丢弃（dial i/o timeout）")
		}
		return false
	}

	proxyNdp.mu.Lock()
	last, known := proxyNdp.entries[ip]
	proxyNdp.mu.Unlock()
	if known && proxyNdpNow().Sub(last) < proxyNeighborRefresh {
		return true
	}

	if _, err := proxyNdpExec(proxyNeighborOpTimeout, "ip", "-6", "neigh", "replace", "proxy", ip, "dev", netIf); err != nil {
		log.Printf("警告：为出口地址 %s 添加 proxy_ndp 条目失败: %v", ip, err)
		return false
	}

	proxyNdp.mu.Lock()
	proxyNdp.entries[ip] = proxyNdpNow()
	proxyNdp.mu.Unlock()
	return true
}

// proxyNeighborDel 删除单条代理邻居条目（仅在需要立即回收时使用）。
// 只有内核确认删掉才从账本销账，否则会留下「账本没了、内核里还在」的残留。
func proxyNeighborDel(ip string) error {
	if netIf == "" || ip == "" {
		return fmt.Errorf("网卡或地址未就绪")
	}
	if ndpNoKernelEntries() {
		return nil
	}
	if _, err := proxyNdpExec(proxyNeighborOpTimeout, "ip", "-6", "neigh", "del", "proxy", ip, "dev", netIf); err != nil {
		return err
	}
	proxyNdp.mu.Lock()
	delete(proxyNdp.entries, ip)
	proxyNdp.mu.Unlock()
	return nil
}

// startProxyNeighborReaper 定期清理过期的代理邻居条目：
// 条目不会自己消失，长期运行会一直堆在内核 neigh 表里。
func startProxyNeighborReaper() {
	for {
		time.Sleep(proxyNeighborReapSeconds * time.Second)
		pruneProxyNeighbors(proxyNdpNow())
	}
}

// resetProxyNeighbors 启动时清理上一次运行留在内核 neigh 表里的代理条目。
// 条目在 neigh 表里是永久的，进程重启不会回收，所以必须处理，但方式要看规模：
//   - 小表（<= proxyNeighborFlushSkipThreshold）：一次 `ip -6 neigh flush proxy` 删干净；
//   - 大表：flush 是近似 O(n²)（每条删除都会触发 solicited-node 组播 leave 与表遍历，
//     实测 38 万条 13 分钟只删 2072 条），同步做只会把启动卡死几分钟且删不掉几条。
//     这种情况只打醒目告警并跳过，交给维护窗口的接口级 IPv6 重建 / 重启宿主一次性解决。
func resetProxyNeighbors() {
	// userspace 后端自己不再写条目，但历史遗留（旧版本 kernel 后端堆下的）仍在内核表里，
	// 而且每一轮 MLD 重报都会把它们算进去，所以启动时照样清一次（大表只告警）。
	if netIf == "" || (!proxyNdpEnabled() && !ndpNoKernelEntries()) {
		return
	}
	ips, err := listProxyNeighbors()
	if err != nil {
		// 读不出来通常就是「表太大、命令超时被 kill」：不能盲目 flush
		log.Printf("警告：读取 proxy_ndp 残留条目失败: %v；跳过启动清理（大表上盲目 flush 会卡分钟级）", err)
		return
	}
	if len(ips) == 0 {
		return
	}
	if len(ips) > proxyNeighborFlushSkipThreshold {
		log.Printf("==================================================================")
		log.Printf("严重警告：网卡 %s 上残留 %d 条 proxy_ndp 条目（阈值 %d），已跳过启动清理。",
			netIf, len(ips), proxyNeighborFlushSkipThreshold)
		log.Printf("  这种规模下 `ip -6 neigh flush proxy` 近似 O(n²)：实测 38 万条约 13 分钟只删 2072 条。")
		log.Printf("  程序同步清理只会卡住启动且几乎无效，必须在维护窗口处理：")
		log.Printf("    1) 备份：ip -6 addr > /root/ipv6-addr.bak; ip -6 route show default > /root/ipv6-route.bak")
		log.Printf("    2) 接口级重建 IPv6：sysctl -w net.ipv6.conf.%s.disable_ipv6=1 然后 =0", netIf)
		log.Printf("    3) 或直接重启宿主；确认 SSH 走 IPv4 或有带外/本地终端再操作。")
		log.Printf("  重建后实测 pneigh=0、/proc/net/igmp6 只剩 29 行、MLD report 归零，地址与默认路由自动恢复。")
		log.Printf("==================================================================")
		return
	}
	if err := flushProxyNeighbors(); err != nil {
		log.Printf("警告：清理 %d 条残留 proxy_ndp 条目失败: %v；降级为接管，交由 TTL 回收", len(ips), err)
		adoptProxyNeighbors(ips)
		return
	}
	log.Printf("proxy_ndp 残留清理完成：删除上次运行遗留的 %d 条代理邻居条目", len(ips))
}

// adoptProxyNeighbors 接管启动时读到的残留条目（flush 失败时的降级路径）。
// 时间戳记为当下，由 reaper 按 TTL 分批（ip -batch，一次 exec 删一批）清掉。
// 本程序是这些条目的唯一写入方（同机只跑一个实例），直接接管不会误伤别人。
func adoptProxyNeighbors(ips []string) {
	if len(ips) == 0 {
		return
	}
	now := proxyNdpNow()
	adopted := 0
	proxyNdp.mu.Lock()
	for _, ip := range ips {
		if _, ok := proxyNdp.entries[ip]; ok {
			continue
		}
		proxyNdp.entries[ip] = now
		adopted++
	}
	proxyNdp.mu.Unlock()
	if adopted > 0 {
		log.Printf("proxy_ndp 残留接管：上次运行遗留的 %d 条代理邻居，交由 TTL 回收", adopted)
	}
}

// listProxyNeighbors 读出网卡上现有的代理邻居条目地址（用于启动残留统计与降级接管）
func listProxyNeighbors() ([]string, error) {
	if netIf == "" {
		return nil, fmt.Errorf("网卡未就绪")
	}
	out, err := proxyNdpExec(proxyNeighborProbeTimeout, "ip", "-6", "neigh", "show", "proxy", "dev", netIf)
	if err != nil {
		return nil, err
	}
	ips := make([]string, 0, 16)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || net.ParseIP(fields[0]) == nil {
			continue
		}
		ips = append(ips, fields[0])
	}
	return ips, nil
}

// deleteProxyNeighbors 批量删除代理邻居条目：`ip -batch -` 一次进程搞定几百条。
// 返回 nil 表示这批条目在内核表里确实都不存在了，调用方才能销账；
// 超时被杀（signal: killed）等失败一律返回 error，账本保留等下一轮重试。
func deleteProxyNeighbors(ips []string) error {
	if len(ips) == 0 {
		return nil
	}
	if netIf == "" {
		return fmt.Errorf("网卡未就绪")
	}
	var b strings.Builder
	for _, ip := range ips {
		b.WriteString("neigh del proxy ")
		b.WriteString(ip)
		b.WriteString(" dev ")
		b.WriteString(netIf)
		b.WriteString("\n")
	}
	// -force：某条已经不在了（被外部清掉）时继续删剩下的，否则一条报错就整批中止
	out, err := proxyNdpExecInput(b.String(), proxyNeighborBatchTimeout, "ip", "-force", "-batch", "-")
	if err == nil {
		return nil
	}
	if batchErrorsAreAllMissing(err, out) {
		// 「条目本来就没了」= 内核表里确实没有，账本可以安全销账。
		// 不这么做的话，一次超时被 kill 产生的幽灵条目会让之后每一轮批量删除都失败，
		// 真条目永远删不掉。
		return nil
	}
	return err
}

// batchErrorsAreAllMissing 判断一次 `ip -force -batch -` 的失败是否全部是「条目已不存在」。
// 这类失败在内核侧已经满足预期（表里没有该条目），可以照常销账；
// 真正的失败（超时被杀、权限不足、网卡不在）必须原样上抛，让账本保留下来重试。
func batchErrorsAreAllMissing(err error, out string) bool {
	if err == nil {
		return false
	}
	// 命令被超时 kill（signal: killed）时输出可能为空或残缺，一律按真失败处理
	if errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	failed := strings.Count(out, "Command failed")
	if failed == 0 {
		return false
	}
	missing := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "No such file or directory") || strings.Contains(line, "Cannot find") {
			missing++
		}
	}
	return missing == failed
}

// pruneProxyNeighbors 删掉超过 TTL 的条目，并按上限淘汰最旧的条目。
// 抽出来便于单测：now 作为参数注入。
//
// 销账时机是这次改造的关键：**只有 ip -batch 确认删除成功才从账本移除**。
// 旧实现先删账本再执行 ip，命令被超时 kill 后账本清空、内核条目永久残留，
// 正是 pneigh 堆到 38 万条的直接原因。账本「只多不少」是安全方向：
// 多做一次 replace 无害，漏删则永久残留。
func pruneProxyNeighbors(now time.Time) {
	if ndpNoKernelEntries() || !proxyNdpEnabled() || netIf == "" {
		return
	}

	type entry struct {
		ip string
		at time.Time
	}

	proxyNdp.mu.Lock()
	expired := make([]string, 0, 16)
	live := make([]entry, 0, len(proxyNdp.entries))
	for ip, at := range proxyNdp.entries {
		// 池内地址会被反复复用：删掉下次请求还要重建，而 CPE 邻居缓存已过期，
		// 重建后的首个 NS/首包必丢。池大小本身受 EXIT_POOL_SIZE 限制，不会失控。
		if exitPoolHas(ip) {
			continue
		}
		if now.Sub(at) > proxyNeighborTTL {
			expired = append(expired, ip)
			continue
		}
		live = append(live, entry{ip, at})
	}
	total := len(proxyNdp.entries)
	proxyNdp.mu.Unlock()

	// 超出上限时按时间从旧到新淘汰
	var oldest []string
	if over := total - proxyNeighborMax; over > 0 {
		sort.Slice(live, func(i, j int) bool { return live[i].at.Before(live[j].at) })
		if over > len(live) {
			over = len(live) // 池内地址受保护，只能淘汰非池条目
		}
		for i := 0; i < over; i++ {
			oldest = append(oldest, live[i].ip)
		}
	}

	// 单轮删除量封顶，剩下的留到下一轮
	toDelete := append(expired, oldest...)
	if len(toDelete) > proxyNeighborMaxDeletes {
		toDelete = toDelete[:proxyNeighborMaxDeletes]
	}
	if len(toDelete) == 0 {
		return
	}

	if err := deleteProxyNeighbors(toDelete); err != nil {
		noteProxyNeighborDeleteFailure(err)
		return
	}

	proxyNdp.mu.Lock()
	for _, ip := range toDelete {
		delete(proxyNdp.entries, ip)
	}
	proxyNdp.deleteFailures = 0
	remaining := len(proxyNdp.entries)
	proxyNdp.mu.Unlock()

	expiredN := min(len(expired), len(toDelete))
	log.Printf("proxy_ndp 条目清理：过期 %d 条，超限淘汰 %d 条（已确认删除，账本剩 %d 条）",
		expiredN, len(toDelete)-expiredN, remaining)
}

// noteProxyNeighborDeleteFailure 记录一轮删除失败；连续失败到阈值就整表熔断。
// 熔断的意义：账本与内核表已经无法靠逐条删除对齐（典型是 ip 命令持续被超时 kill），
// 此时整表 flush 一次，让两侧都归零，比留着几十万条幽灵条目继续拖垮 mld 更划算。
func noteProxyNeighborDeleteFailure(cause error) {
	proxyNdp.mu.Lock()
	proxyNdp.deleteFailures++
	failures := proxyNdp.deleteFailures
	proxyNdp.mu.Unlock()
	log.Printf("警告：proxy_ndp 条目删除失败（连续第 %d 轮）: %v；账本保留，等下一轮重试", failures, cause)

	if failures < proxyNeighborFailThreshold {
		return
	}
	log.Printf("警告：proxy_ndp 条目连续 %d 轮删除失败，执行整表 flush 熔断", failures)
	if err := flushProxyNeighbors(); err != nil {
		log.Printf("警告：整表 flush 也失败: %v；账本保留，等下一轮重试", err)
		return
	}
	log.Printf("proxy_ndp 整表 flush 完成，账本已清空（后续按需重建）")
}

// flushProxyNeighbors 整表 flush 掉网卡上的全部代理邻居条目，并清空账本。
// 只用于小表场景：启动时的残留清理、以及连续删除失败后的熔断。
// 大表（几万条以上）不能用它——实测 38 万条的表上 13 分钟只删掉 2072 条。
func flushProxyNeighbors() error {
	if netIf == "" {
		return fmt.Errorf("网卡未就绪")
	}
	if _, err := proxyNdpExec(proxyNeighborFlushTimeout, "ip", "-6", "neigh", "flush", "proxy", "dev", netIf); err != nil {
		return err
	}
	proxyNdp.mu.Lock()
	proxyNdp.entries = make(map[string]time.Time)
	proxyNdp.deleteFailures = 0
	proxyNdp.mu.Unlock()
	return nil
}

// runCmdWithTimeout 带超时执行命令并返回合并输出。
// 与 runCmd 的区别是超时可配：普通条目操作 30s、批量删除 60s、整表 flush 180s。
// 超时时 exec 会用 SIGKILL 结束进程（日志里表现为 signal: killed），这里把它翻译成人话。
func runCmdWithTimeout(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c := exec.CommandContext(ctx, name, args...)
	// 命令若拉起守护进程继承了 stdout 管道，WaitDelay 到期强制关闭管道解除阻塞
	c.WaitDelay = 5 * time.Second
	out, err := c.CombinedOutput()
	return string(out), reportCmdFailure(ctx, timeout, name, args, out, err)
}

// runCmdInputWithTimeout 带 stdin 执行命令（批量删除靠 `ip -force -batch -` 一次收完，避免逐条 exec）
func runCmdInputWithTimeout(input string, timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c := exec.CommandContext(ctx, name, args...)
	c.Stdin = strings.NewReader(input)
	c.WaitDelay = 5 * time.Second
	out, err := c.CombinedOutput()
	return string(out), reportCmdFailure(ctx, timeout, name, args, out, err)
}

// reportCmdFailure 记日志并返回给调用方的错误。
// 超时被 kill 的错误会包上 context.DeadlineExceeded：调用方（deleteProxyNeighbors）靠它
// 区分「命令被杀、结果未知」与「命令报错、可逐条判断」——前者绝对不能销账。
func reportCmdFailure(ctx context.Context, timeout time.Duration, name string, args []string, out []byte, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		log.Printf("命令 %s %v 执行失败（超过 %s 被强制终止，signal: killed）: %v，输出：%s",
			name, args, timeout, err, truncateCmdOutput(out))
		return fmt.Errorf("%w: %v（超过 %s 被强制终止）", context.DeadlineExceeded, err, timeout)
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		// 只有守护进程型的命令会走到这里（ip 不会）：管道等待超时，结果不可信，按失败处理
		log.Printf("命令 %s %v 输出管道等待超时: %v", name, args, err)
		return err
	}
	log.Printf("命令 %s %v 执行失败: %v，输出：%s", name, args, err, truncateCmdOutput(out))
	return err
}

// truncateCmdOutput 截断命令输出，避免大表上的几十万行把日志写满
func truncateCmdOutput(out []byte) string {
	s := strings.TrimSpace(string(out))
	if len(s) <= cmdOutputLimit {
		return s
	}
	return s[:cmdOutputLimit] + fmt.Sprintf("…（共 %d 字节，已截断）", len(out))
}
