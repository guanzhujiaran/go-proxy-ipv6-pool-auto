package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// 随机出口地址不在网卡上，回程包要靠邻居解析把 CPE 引回来。
// 同一台光猫、同一前缀下的实测结论（三选一，别再来回改）：
//
//	1) 只加 `local <前缀>/64 dev lo` + 用户态 NA 代答（ndppd / 内置代答器）：
//	   CPE 不采纳，NS 会一遍遍重发，dial 几乎 100% i/o timeout；
//	2) 把随机地址 `ip -6 addr add` 到网卡：CPE 立刻采纳（~1s 建连），
//	   但 netlink 地址事件会让 Chromium 判定「网络已变化」，在途请求全部 ERR_NETWORK_CHANGED；
//	3) 内核 proxy_ndp（本方案）：CPE 采纳内核发的 NA（实测冷地址 0.6s 建连），
//	   而且只产生 neigh 事件、不产生地址事件 → Chromium 无感。
//
// 所以出口地址改成：拨号前 `ip -6 neigh add proxy <出口地址> dev <网卡>`，
// 交给内核代答 NS；请求路径上照旧只 bind、不改网卡地址。

const (
	// proxyNeighborTTL 代理邻居条目的保留时长：CPE 侧邻居缓存过期后会重新发 NS，
	// 期间条目还在就能继续被代答；已结束的连接不再需要，所以可以直接过期释放。
	proxyNeighborTTL = 10 * time.Minute
	// proxyNeighborMax 代理邻居条目上限，防止长期运行把内核 neigh 表堆满（超出按最旧淘汰）
	proxyNeighborMax = 4096
	// proxyNeighborReapSeconds 过期清理周期（秒）
	proxyNeighborReapSeconds = 60
	// proxyNeighborMaxDeletes 单轮最多删多少条：删除要逐条 exec `ip`，
	// 一次性删几千条会长时间占着 CPU，分几轮删完即可。
	proxyNeighborMaxDeletes = 500
)

var proxyNdp struct {
	mu      sync.Mutex
	enabled bool
	warned  bool
	entries map[string]time.Time
}

func init() {
	proxyNdp.entries = make(map[string]time.Time)
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
func proxyNeighborAdd(ip string) bool {
	if netIf == "" || ip == "" {
		return false
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
	if _, err := runCmd("ip", "-6", "neigh", "replace", "proxy", ip, "dev", netIf); err != nil {
		log.Printf("警告：为出口地址 %s 添加 proxy_ndp 条目失败: %v", ip, err)
		return false
	}
	proxyNdp.mu.Lock()
	proxyNdp.entries[ip] = time.Now()
	proxyNdp.mu.Unlock()
	return true
}

// proxyNeighborDel 删除代理邻居条目（仅在需要立即回收时使用）
func proxyNeighborDel(ip string) {
	if netIf == "" || ip == "" {
		return
	}
	if _, err := runCmd("ip", "-6", "neigh", "del", "proxy", ip, "dev", netIf); err != nil {
		return
	}
	proxyNdp.mu.Lock()
	delete(proxyNdp.entries, ip)
	proxyNdp.mu.Unlock()
}

// startProxyNeighborReaper 定期清理过期的代理邻居条目：
// 条目不会自己消失，长期运行会一直堆在内核 neigh 表里。
func startProxyNeighborReaper() {
	for {
		time.Sleep(proxyNeighborReapSeconds * time.Second)
		pruneProxyNeighbors(time.Now())
	}
}

// cleanupProxyNeighbors 启动时清掉上一次运行残留的代理邻居条目。
// 条目在内核 neigh 表里是永久的，进程重启不会回收，留着只会越堆越多；
// 本程序是这些条目的唯一写入方（同机只跑一个实例），所以启动时整表清空是安全的。
func cleanupProxyNeighbors() {
	if !proxyNdpEnabled() || netIf == "" {
		return
	}
	out, err := runCmd("ip", "-6", "neigh", "show", "proxy", "dev", netIf)
	if err != nil {
		return
	}
	removed := 0
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if net.ParseIP(fields[0]) == nil {
			continue
		}
		if _, err := runCmd("ip", "-6", "neigh", "del", "proxy", fields[0], "dev", netIf); err == nil {
			removed++
		}
	}
	if removed > 0 {
		log.Printf("proxy_ndp 残留清理：删除上次运行遗留的 %d 条代理邻居", removed)
	}
}

// pruneProxyNeighbors 删掉超过 TTL 的条目，并按上限淘汰最旧的条目。
// 抽出来便于单测：now 作为参数注入。
func pruneProxyNeighbors(now time.Time) {
	if !proxyNdpEnabled() || netIf == "" {
		return
	}
	proxyNdp.mu.Lock()
	var expired []string
	var oldest []string
	for ip, at := range proxyNdp.entries {
		if now.Sub(at) > proxyNeighborTTL {
			expired = append(expired, ip)
		}
	}
	if len(proxyNdp.entries)-len(expired) > proxyNeighborMax {
		// 超出上限时按时间从旧到新淘汰，留出余量，避免每个清理周期都触发一轮批量删除
		type kv struct {
			ip string
			at time.Time
		}
		all := make([]kv, 0, len(proxyNdp.entries))
		for ip, at := range proxyNdp.entries {
			if now.Sub(at) > proxyNeighborTTL {
				continue
			}
			all = append(all, kv{ip, at})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
		over := len(all) - proxyNeighborMax
		for i := 0; i < over; i++ {
			oldest = append(oldest, all[i].ip)
		}
	}
	// 单轮删除量封顶，剩下的留到下一轮
	if len(expired) > proxyNeighborMaxDeletes {
		expired = expired[:proxyNeighborMaxDeletes]
	}
	if room := proxyNeighborMaxDeletes - len(expired); room > 0 && len(oldest) > room {
		oldest = oldest[:room]
	}
	for _, ip := range expired {
		delete(proxyNdp.entries, ip)
	}
	for _, ip := range oldest {
		delete(proxyNdp.entries, ip)
	}
	proxyNdp.mu.Unlock()

	for _, ip := range expired {
		_, _ = runCmd("ip", "-6", "neigh", "del", "proxy", ip, "dev", netIf)
	}
	for _, ip := range oldest {
		_, _ = runCmd("ip", "-6", "neigh", "del", "proxy", ip, "dev", netIf)
	}
	if len(expired)+len(oldest) > 0 {
		log.Printf("proxy_ndp 条目清理：过期 %d 条，超限淘汰 %d 条", len(expired), len(oldest))
	}
}
