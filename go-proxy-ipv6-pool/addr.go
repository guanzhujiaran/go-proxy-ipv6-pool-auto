package main

import (
	"log"
	"net"
	"strconv"
	"strings"
)

// 随机出口地址不挂到网卡上（与上游 go-proxy-ipv6-pool 一致）：
//
//   - `ip route replace local <前缀>/64 dev lo` 把整段池前缀声明为本机地址，
//     配合 net.ipv6.ip_nonlocal_bind=1，该 /64 内任意地址都能直接作为源地址 dial；
//   - 回程包的 NS 由内置 NDP 代答器应答（ndp_responder.go，NA 带 override）；
//   - 请求路径上不执行 `ip -6 addr add/del`：那会产生 netlink 地址事件，
//     Chromium 系的 AddressTrackerLinux 会判定「网络已变化」
//     （HttpNetworkSession::NetworkChanged → FlushSocketPoolsWithError），
//     正在进行的请求全部以 ERR_NETWORK_CHANGED 中断；Firefox 只清空闲连接，所以表现正常。
//
// 所以内核网络配置只在启动/前缀变化时改一次，请求路径只 bind 随机地址。

// delExitAddress 删除接口上的地址（仅用于清理历史版本残留在网卡上的出口地址）。
func delExitAddress(ip string) {
	if netIf == "" {
		return
	}
	_, _ = runCmd("ip", "-6", "addr", "del", ip+"/"+prefixLenStr(), "dev", netIf)
}

// prefixLenStr 前缀长度（与池前缀保持一致）
func prefixLenStr() string {
	return strconv.Itoa(prefixLen)
}

// cleanupStaleExitAddresses 启动时清理旧版本残留在网卡上的出口地址。
//
// 判定依据是地址标志：旧版本用 `ip addr add ... nodad` 逐个挂地址，这些地址带 nodad 且不是
// dynamic；SLAAC 生成的真实地址一定是 dynamic。这样不会把本机真实地址误删。
// 当前版本不再往接口加地址，这里的清理只是一次性迁移。
func cleanupStaleExitAddresses() {
	if cidr == "" || netIf == "" {
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
		nodad, dynamic := false, false
		for _, field := range fields {
			switch field {
			case "nodad":
				nodad = true
			case "dynamic", "mngtmpaddr":
				dynamic = true
			}
		}
		if !nodad || dynamic {
			continue
		}
		for i, field := range fields {
			if field != "inet6" || i+1 >= len(fields) {
				continue
			}
			addr := strings.SplitN(fields[i+1], "/", 2)[0]
			ip := net.ParseIP(addr)
			if ip == nil || ip.To4() != nil || !ipnet.Contains(ip) {
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
