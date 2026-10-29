package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func changeNdppdConfig(currentIpv6 string, prevIpv6 string) {
	addAddr := fmt.Sprintf("%s/%d", currentIpv6, prefixLen)
	log.Printf("变更 ndppd 配置文件：%s", addAddr)
	//这里实际只要添加当前ip然后加一个/64的掩码就行了，ndppd的配置也是，不需要添加标准的cidr
	if prevIpv6 != "" {
		prevAddr := fmt.Sprintf("%s/%d", prevIpv6, prefixLen)
		execCmd("ip", "route", "del", "local", prevAddr, "dev", "lo") //删除上一个ip
	}
	// 用 replace 保证幂等：容器重启后同一条 local 路由已存在时 add 会报 File exists
	execCmd("ip", "route", "replace", "local", addAddr, "dev", "lo")

	confContent := fmt.Sprintf(`
route-ttl 30000
proxy %s {
	router no
	timeout 500
	ttl 30000
    rule %s {
        static
    }
}
`, netIf, addAddr)

	if err := os.WriteFile("/etc/ndppd.conf", []byte(confContent), 0644); err != nil {
		log.Printf("写入 ndppd.conf 失败: %v", err)
		return
	}

	restartNdppd()
}

// restartNdppd 异步重启 ndppd。
// 有些发行版的 init 脚本（或 systemd 对 sysv 脚本的猜测）会让 `service ndppd restart`
// 阻塞几十秒，放在监控循环里会把整个流程卡住，因此后台执行并单独记录耗时。
func restartNdppd() {
	go func() {
		start := time.Now()
		// service restart 不会清理残留进程，出现两个 ndppd 同时持有 raw socket 时会互相漏答 NS，
		// 导致随机出口回程不通；这里先杀掉旧实例，保证单实例。
		_, _ = runCmd("pkill", "ndppd")
		execCmd("service", "ndppd", "restart")
		log.Printf("ndppd 重启流程结束，耗时 %s", time.Since(start))
	}()
}

func getLocalIPv6() (string, error) {
	// 优先用 NET_IF 指定的网卡上的公网 IPv6：代理池的前缀必须是能对外路由的公网段，
	// 若误选到 Docker 内网 ULA（fc00::/7）前缀，出口地址会被 NAT 成同一个地址，池就形同虚设。
	if netIf != "" {
		if ip, ok := findIPv6OnInterfaceName(netIf, isPublicIPv6); ok {
			log.Printf("从网卡 %s 获取到公网 IPv6 地址: %s", netIf, ip)
			return ip, nil
		}
	}

	if ip, iface, ok := findIPv6InAllInterfaces(isPublicIPv6); ok {
		log.Printf("从网卡 %s 获取到公网 IPv6 地址: %s", iface, ip)
		return ip, nil
	}

	// 实在没有公网地址时退回内网 ULA 前缀：网络可通，但所有出口会被 NAT 成同一地址
	if ip, iface, ok := findIPv6InAllInterfaces(isPrivateIPv6); ok {
		log.Printf("警告：网卡 %s 上只有内网 IPv6 地址 %s，出口地址会被 NAT 成同一地址（需要宿主机具备公网 IPv6 前缀）", iface, ip)
		return ip, nil
	}

	log.Println("本地网卡未找到公网 IPv6 地址，尝试从外部服务获取...")
	return getIPv6FromService()
}

// isPublicIPv6 判断是否为可用于出站的公网 IPv6（排除 IPv4、链路本地、ULA fc00::/7）
func isPublicIPv6(ip net.IP) bool {
	if ip.To4() != nil || !ip.IsGlobalUnicast() {
		return false
	}
	return !ip.IsPrivate()
}

// isPrivateIPv6 判断是否为 IPv6 内网地址（ULA fc00::/7）
func isPrivateIPv6(ip net.IP) bool {
	return ip.To4() == nil && ip.IsGlobalUnicast() && ip.IsPrivate()
}

// findIPv6OnInterfaceName 在指定网卡上查找满足条件的 IPv6 地址
func findIPv6OnInterfaceName(ifaceName string, match func(net.IP) bool) (string, bool) {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		log.Printf("获取网卡 %s 失败: %v", ifaceName, err)
		return "", false
	}
	return matchIPv6OnInterface(*iface, match)
}

// findIPv6InAllInterfaces 遍历所有网卡查找满足条件的 IPv6 地址
func findIPv6InAllInterfaces(match func(net.IP) bool) (string, string, bool) {
	ifaces, err := net.Interfaces()
	if err != nil {
		log.Printf("获取网卡列表失败: %v", err)
		return "", "", false
	}
	for _, iface := range ifaces {
		if ip, ok := matchIPv6OnInterface(iface, match); ok {
			return ip, iface.Name, true
		}
	}
	return "", "", false
}

func matchIPv6OnInterface(iface net.Interface, match func(net.IP) bool) (string, bool) {
	addrs, err := iface.Addrs()
	if err != nil {
		log.Printf("获取网卡 %s 地址失败: %v", iface.Name, err)
		return "", false
	}
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err != nil {
			continue
		}
		if match(ip) {
			return ip.String(), true
		}
	}
	return "", false
}

func ipv6Monitor() {
	for {
		currentIpv6, err := getLocalIPv6()
		if err != nil {
			log.Println("获取 IPv6 地址失败:", err)
			time.Sleep(600 * time.Second)
			continue
		}

		if currentIpv6 != ipv6Addr {
			cidr = fmt.Sprintf("%s/%d", currentIpv6, prefixLen)
			if runEnv == "dev" {
				log.Printf("获取到 IPv6 地址变动 currentCidr:[%s], currentIPv6Addr:[%s]", currentIpv6, ipv6Addr)
			}
			changeNdppdConfig(currentIpv6, ipv6Addr)
			ipv6Addr = currentIpv6
		} else if cidr != "" {
			// 有些环境（如 WSL 镜像网络）会重置路由表，导致前缀的 local 路由消失，
			// 回程包就会被内核当成「不是发给本机的」丢掉；这里每轮幂等地补一次。
			ensureLocalRoute(cidr)
		}

		time.Sleep(600 * time.Second)
	}
}

// ensureLocalRoute 幂等地保证池前缀存在 local 路由（ip route replace 不会因已存在而报错）
func ensureLocalRoute(poolCidr string) {
	execCmd("ip", "route", "replace", "local", poolCidr, "dev", "lo")
}

// ipv6EchoURL 外部取址服务：纯文本返回请求方的 IPv6，仅在网卡上找不到 IPv6 时兜底使用
const ipv6EchoURL = "https://v6.mypublicipnow.com/"

func getIPv6FromService() (string, error) {
	log.Printf("正在通过外部服务 %s 获取 IPv6 地址...", ipv6EchoURL)

	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	var resp *http.Response
	var err error
	maxRetries := 3

	for i := 0; i < maxRetries; i++ {
		resp, err = client.Get(ipv6EchoURL)
		if err == nil {
			break
		}
		log.Printf("请求 %s 失败 (尝试 %d/%d): %v", ipv6EchoURL, i+1, maxRetries, err)
		time.Sleep(2 * time.Second)
	}

	if err != nil {
		log.Printf("请求 %s 最终失败: %v", ipv6EchoURL, err)
		return "", fmt.Errorf("请求 %s 失败: %v", ipv6EchoURL, err)
	}

	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			log.Printf("关闭响应体失败: %v", err)
		}
	}(resp.Body)

	if resp.StatusCode != http.StatusOK {
		log.Printf("%s 返回状态码: %d", ipv6EchoURL, resp.StatusCode)
		return "", fmt.Errorf("%s 返回状态码: %d", ipv6EchoURL, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("读取响应失败: %v", err)
		return "", fmt.Errorf("读取响应失败: %v", err)
	}

	// 回显服务返回的 body 可能带首尾空白/换行，先裁掉再校验
	ipv6 := strings.TrimSpace(string(body))
	if ipv6 == "" {
		log.Printf("从外部服务 %s 获取到的 IPv6 地址为空", ipv6EchoURL)
		return "", fmt.Errorf("从外部服务 %s 获取到的 IPv6 地址为空", ipv6EchoURL)
	}
	if net.ParseIP(ipv6) == nil {
		log.Printf("无效的 IPv6 地址: %s", ipv6)
		return "", fmt.Errorf("无效的 IPv6 地址: %s", ipv6)
	}
	result := ipv6

	log.Printf("从外部服务获取到 IPv6 地址: %s", result)
	return result, nil
}
