package main

import (
	"log"
	"net"
	"strconv"
	"strings"
)

// 随机出口地址不挂到网卡上（与上游 go-proxy-ipv6-pool 一致）：
//
//   - `ip -6 route replace local <前缀>::/64 dev lo` 把整段池前缀声明为本机地址，
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

// applyPoolPrefix 池前缀变化时切换 local 路由：删掉旧前缀、加上新前缀，并清理历史遗留前缀。
// 整段 /64 声明为本机地址后，该前缀内任意地址都能作为源地址 dial（配合
// net.ipv6.ip_nonlocal_bind=1）；回程包的 NS 由内置 NDP 代答器（ndp_responder.go）应答。
// 整个过程只在启动/前缀变化时执行一次，请求路径上不改任何网络配置。
func applyPoolPrefix(currentCidr, prevCidr string) {
	if currentCidr == "" {
		return
	}
	log.Printf("更新池前缀：%s（上一前缀：%s）", currentCidr, prevCidr)
	if prevCidr != "" && prevCidr != currentCidr {
		execCmd("ip", "-6", "route", "del", "local", prevCidr, "dev", "lo")
	}
	// 用 replace 保证幂等：容器重启后同一条 local 路由已存在时 add 会报 File exists
	execCmd("ip", "-6", "route", "replace", "local", currentCidr, "dev", "lo")
	pruneStalePoolRoutes(currentCidr)
}

// pruneStalePoolRoutes 清理历史遗留的池路由。
// 光猫重新拨号后前缀会换，旧前缀的 `local <旧前缀>::/64 dev lo` 不会自己消失：
// 留着它等于把整段旧 /64 继续声明为本机地址，既没必要，也会让排障时误判当前前缀。
// 只删「/64、公网、挂在 lo、且当前没有任何网卡持有该前缀」的那种，避免误删真实网段。
func pruneStalePoolRoutes(currentCidr string) {
	live := make(map[string]bool)
	for _, info := range listIPv6Addrs("") {
		if !isPublicIPv6(info.ip) {
			continue
		}
		cidr, err := maskCIDR(info.ip.String(), prefixLen)
		if err != nil {
			continue
		}
		live[cidr] = true
	}

	out, err := runCmd("ip", "-6", "route", "show", "table", "local")
	if err != nil {
		log.Printf("读取 local 路由表失败: %v", err)
		return
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "local" {
			continue
		}
		target := fields[1]
		if target == currentCidr || live[target] {
			continue
		}
		_, ipnet, err := net.ParseCIDR(target)
		if err != nil || !isPublicIPv6(ipnet.IP) {
			continue
		}
		if ones, bits := ipnet.Mask.Size(); ones != prefixLen || bits != 128 {
			continue
		}
		if !strings.Contains(line, "dev lo") {
			continue
		}
		log.Printf("清理旧前缀本地路由：%s（网卡上已不存在该前缀）", target)
		execCmd("ip", "-6", "route", "del", "local", target, "dev", "lo")
	}
}

// cleanupStaleExitAddresses 启动时清理旧版本残留在网卡上的出口地址。
// 判定依据是地址标志：旧版本用 `ip addr add ... nodad` 逐个挂地址，这些地址带 nodad 且不是
// dynamic；SLAAC 生成的真实地址一定是 dynamic。这样不会把本机真实地址误删。
// 当前版本不再往接口加地址，这里的清理只是一次性迁移。
func cleanupStaleExitAddresses() {
	poolCidr := pool.CIDR()
	if poolCidr == "" || netIf == "" {
		return
	}
	_, ipnet, err := net.ParseCIDR(poolCidr)
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
	for _, info := range parseIPv6Addrs(out) {
		// 只删旧版本留下的 nodad 地址；SLAAC 生成的真实地址一定带 dynamic，必须留着
		if !info.nodad || info.dynamic || !ipnet.Contains(info.ip) {
			continue
		}
		delExitAddress(info.ip.String())
		removed++
	}
	if removed > 0 {
		log.Printf("残留清理：删除上次运行遗留的 %d 个出口地址", removed)
	}
}
