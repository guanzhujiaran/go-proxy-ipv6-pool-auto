package main

import (
	"log"
	"net"
	"os"
	"strconv"
	"strings"
)

// 网卡选择：每台机器的网卡名都不一样（eth0 / enp2s0 / ens18 / eno1 …），
// 容器又是 host 网络（看得到宿主的全部网卡），所以这里不写死任何名字，
// 由程序自己按「默认路由 → 带公网 IPv6 的物理网卡」判断；
// .env 里的 NET_IF 只是可选覆盖项（多网卡机器想精确指定时才需要填）。

// virtualIfacePrefixes 虚拟 / 隧道网卡名前缀。
// 它们即便出现 IPv6 也不能当池前缀：那段地址不对上层路由，出口会被 NAT 成同一个地址，池就形同虚设。
var virtualIfacePrefixes = []string{
	"lo", "docker", "br-", "veth", "virbr", "tun", "tap", "dummy",
	"cni", "flannel", "kube", "zt", "tailscale", "wg", "ifb", "vnet",
}

func isVirtualIface(name string) bool {
	for _, prefix := range virtualIfacePrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// defaultRouteIface 默认路由（family=4 走 IPv4，family=6 走 IPv6）的出口网卡名；取不到返回空串。
func defaultRouteIface(family int) string {
	out, err := runCmd("ip", "-"+strconv.Itoa(family), "route", "show", "default")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		for i, field := range fields {
			if field == "dev" && i+1 < len(fields) {
				return fields[i+1]
			}
		}
	}
	return ""
}

// firstIfaceWithPublicIPv6 第一个「非虚拟网卡 + 带公网 IPv6」的网卡名；没有返回空串。
func firstIfaceWithPublicIPv6() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		log.Printf("获取网卡列表失败: %v", err)
		return ""
	}
	for _, iface := range ifaces {
		if isVirtualIface(iface.Name) {
			continue
		}
		if _, ok := matchIPv6OnInterface(iface, isPublicIPv6); ok {
			return iface.Name
		}
	}
	return ""
}

// detectNetIf 自动确定出口网卡，按可信度依次尝试：
//  1. 默认 IPv6 路由的网卡，且该网卡带公网 IPv6 —— 这台机器真正对外的网卡；
//  2. 任意非虚拟网卡上的公网 IPv6（宿主多网卡 / 默认路由还没建好时）；
//  3. 默认 IPv4 路由的网卡（宿主刚开机、RA 还没下发 IPv6，先占位，monitor 下一轮会重判）。
//
// 都判断不出来时返回空串，由调用方（ipv6Monitor）在后续轮次重试。
func detectNetIf() string {
	if name := defaultRouteIface(6); name != "" && !isVirtualIface(name) {
		if _, ok := findIPv6OnInterfaceName(name, isPublicIPv6); ok {
			return name
		}
	}
	if name := firstIfaceWithPublicIPv6(); name != "" {
		return name
	}
	if name := defaultRouteIface(4); name != "" && !isVirtualIface(name) {
		return name
	}
	return ""
}

// resolveNetIf 决定本次运行使用哪块网卡：
//   - .env / 环境变量里的 NET_IF 只是可选覆盖：填了、本机真有这块网卡、且上面有公网 IPv6 才用它；
//   - 其余情况（没填、名字不存在、网卡上还没公网 IPv6）一律交给 detectNetIf 自动判断。
func resolveNetIf() string {
	configured := strings.TrimSpace(os.Getenv("NET_IF"))
	if configured != "" {
		if _, err := net.InterfaceByName(configured); err != nil {
			log.Printf("NET_IF=%s 在本机不存在（本机网卡可用 `ip -o link show` 查看），改为自动探测", configured)
		} else if _, ok := findIPv6OnInterfaceName(configured, isPublicIPv6); ok {
			log.Printf("使用 .env 指定的网卡 %s", configured)
			return configured
		} else if name := detectNetIf(); name != "" {
			log.Printf("NET_IF=%s 上还没有公网 IPv6，自动探测到网卡 %s，改用后者", configured, name)
			return name
		} else {
			log.Printf("NET_IF=%s 上还没有公网 IPv6，且自动探测失败，暂按配置使用", configured)
			return configured
		}
	}

	name := detectNetIf()
	if name == "" {
		log.Printf("自动探测网卡失败（没有默认路由，也没有带公网 IPv6 的网卡），下一轮重试")
		return ""
	}
	log.Printf("自动探测到出口网卡：%s", name)
	return name
}
