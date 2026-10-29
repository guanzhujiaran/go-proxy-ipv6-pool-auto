package main

import (
	"log"
	"os"
	"strings"
	"sync"
)

// 内核 NDP 代理（proxy_ndp）兜底：
// ndppd 是用户态代答，在虚拟化网络下会偶发漏答，导致随机出口的回程包被丢弃
// （表现为 dial i/o timeout，而不是 refused）。内核 proxy_ndp 在 netdevice 层应答，
// 只要给出口地址加一条 proxy 邻居即可，比用户态代答稳定。
// 开关 net.ipv6.conf.<if>.proxy_ndp 是宿主机 sysctl，容器内通常不可写，因此读不到时只提示。
var (
	proxyNdpOnce    sync.Once
	proxyNdpEnabled bool
)

// proxyNdpProcPath 当前网卡对应的 proxy_ndp 开关路径
func proxyNdpProcPath() string {
	if netIf == "" {
		return ""
	}
	return "/proc/sys/net/ipv6/conf/" + netIf + "/proxy_ndp"
}

// ensureProxyNdp 启动时检查一次内核 NDP 代理开关，未开启时尝试打开并给出宿主机侧的处理办法
func ensureProxyNdp() {
	if proxyNdpAvailable() {
		log.Printf("内核 NDP 代理已开启（%s=1），随机出口回程由内核兜底应答", proxyNdpProcPath())
	}
}

// proxyNdpAvailable 检测（并尝试开启一次）内核 NDP 代理，结果缓存
func proxyNdpAvailable() bool {
	proxyNdpOnce.Do(func() {
		path := proxyNdpProcPath()
		if path == "" {
			return
		}
		b, err := os.ReadFile(path)
		if err != nil {
			log.Printf("读取 %s 失败: %v", path, err)
			return
		}
		if strings.TrimSpace(string(b)) == "1" {
			proxyNdpEnabled = true
			return
		}

		// 值为 0：宿主机以 root 直接运行时可以自己开；容器内 /proc/sys 只读，必然失败
		key := "net.ipv6.conf." + netIf + ".proxy_ndp"
		if _, err := runCmd("sysctl", key+"=1"); err == nil {
			proxyNdpEnabled = true
			log.Printf("已开启 %s，随机出口回程由内核兜底应答", key)
			return
		}

		log.Printf("警告：%s 未开启，随机出口回程只能靠 ndppd 用户态代答（偶发漏答会导致 dial 超时）", key)
		log.Printf("警告：请在宿主机执行 sudo sysctl -w %s=1 并写入 /etc/sysctl.d/99-ipv6-pool.conf 后重启容器", key)
	})
	return proxyNdpEnabled
}

// addNdpProxyEntry 让内核为该出口地址代答 NS；内核代理未开启时直接跳过。
// ip 传裸地址（不带方括号）。
func addNdpProxyEntry(ip string) bool {
	if !proxyNdpAvailable() {
		return false
	}
	// replace 保证幂等：同一地址重复添加不会报 File exists
	if _, err := runCmd("ip", "-6", "neigh", "replace", "proxy", ip, "dev", netIf); err != nil {
		return false
	}
	return true
}

// delNdpProxyEntry 清理临时 proxy 条目，避免邻居表随随机地址无限增长。
// CPE 侧学到的邻居条目不受影响（有自身缓存），已建立的连接照常收发。
func delNdpProxyEntry(ip string) {
	if !proxyNdpAvailable() {
		return
	}
	// 条目可能已被内核回收，删除失败无需处理
	_, _ = runCmd("ip", "-6", "neigh", "del", "proxy", ip, "dev", netIf)
}
