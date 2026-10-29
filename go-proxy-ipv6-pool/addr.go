package main

import (
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 随机出口地址为什么要「挂到网卡上」：
// 仅靠 net.ipv6.ip_nonlocal_bind=1 + `ip route replace local <prefix>/64 dev lo`，
// 内核只是在路由表里把整个 /64 当成本机地址，邻居解析只能由 ndppd 用原始套接字代答。
// 实测该路径下 CPE 会把这些地址一直当成 INCOMPLETE（一遍遍发 NS、收到 NA 也不采纳），
// 回程包永远不转发，表现就是 dial 静默超时。
// 把地址真正加到接口上（nodad 立即生效）后，内核会自动加入对应的 solicited-node 组播组，
// 并用带 override 标志的 NA 应答，CPE 立刻接受，实测 ping/HTTP 都能通。
var addrAddFailOnce sync.Once

// addExitAddress 把随机地址临时挂到网卡上，成功后由内核负责 NDP 应答。
// 失败（例如容器缺 NET_ADMIN）时返回 false，由调用方回落到 ndppd / 内核 proxy_ndp 路线。
func addExitAddress(ip string) bool {
	if netIf == "" {
		return false
	}
	if _, err := runCmd("ip", "-6", "addr", "add", ip+"/"+prefixLenStr(), "dev", netIf, "nodad"); err != nil {
		addrAddFailOnce.Do(func() {
			log.Printf("警告：无法把随机出口地址挂到网卡（%v），回落到 ndppd 代答；"+
				"容器需要 NET_ADMIN，宿主机直接运行需要 root", err)
		})
		return false
	}
	return true
}

// delExitAddress 用完即删：每个请求都是一个全新的随机地址，接口上不该堆积地址。
// 池前缀的 local 路由仍在，已建立连接的回程包照常投递到本机。
func delExitAddress(ip string) {
	if netIf == "" {
		return
	}
	_, _ = runCmd("ip", "-6", "addr", "del", ip+"/"+prefixLenStr(), "dev", netIf)
}

// prefixLenStr 地址挂到接口时用的前缀长度，与池前缀保持一致
func prefixLenStr() string {
	return strconv.Itoa(prefixLen)
}

// cleanupStaleExitAddresses 启动时清理上次运行残留在网卡上的出口地址。
// host 网络下地址挂在宿主机 netns 上，容器重启不会自动消失，进程异常退出时会残留。
func cleanupStaleExitAddresses() {
	for i := 0; i < 60 && cidr == ""; i++ {
		time.Sleep(time.Second)
	}
	if cidr == "" || netIf == "" {
		return
	}

	real, err := getLocalIPv6()
	if err != nil {
		log.Printf("残留清理：读取本机地址失败，跳过: %v", err)
		return
	}
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		log.Printf("残留清理：解析池前缀失败: %v", err)
		return
	}

	out, err := runCmd("ip", "-6", "-o", "addr", "show", "dev", netIf)
	if err != nil {
		log.Printf("残留清理：读取网卡地址失败: %v", err)
		return
	}

	removed := 0
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		for i, field := range fields {
			if field != "inet6" || i+1 >= len(fields) {
				continue
			}
			addr := strings.SplitN(fields[i+1], "/", 2)[0]
			ip := net.ParseIP(addr)
			if ip == nil || !ipnet.Contains(ip) || addr == real {
				continue
			}
			delExitAddress(addr)
			removed++
		}
	}
	if removed > 0 {
		log.Printf("残留清理：删除上次运行遗留的 %d 个出口地址", removed)
	}
}
