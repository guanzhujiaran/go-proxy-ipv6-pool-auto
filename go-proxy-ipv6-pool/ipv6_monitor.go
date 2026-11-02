package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// poolCheckDefaultSeconds 常规复查间隔（秒），可用 POOL_CHECK_INTERVAL 覆盖（compose 传 300）
	poolCheckDefaultSeconds = 300
	// poolCheckFailSeconds 暂时取不到可用池前缀时的快速重试间隔（秒）：
	// 光猫刚重启时新前缀可能还没下发，此时不能傻等一整个复查周期
	poolCheckFailSeconds = 15
	// poolRecheckMinGap 拨号失败触发的复查最小间隔：避免失败风暴把复查打成忙循环
	poolRecheckMinGap = 10 * time.Second
)

// recheckCh 拨号全失败时通知 monitor 立即复查池前缀。
// 光猫重新拨号后公网前缀会变，而网卡上旧前缀的地址还会以 deprecated 状态残留一段时间；
// 只靠定时复查要等满一个周期，这段时间所有请求都是 i/o timeout。
var recheckCh = make(chan struct{}, 1)

// requestPoolRecheck 请求立即复查池前缀（非阻塞，重复请求合并成一次）
func requestPoolRecheck() {
	select {
	case recheckCh <- struct{}{}:
	default:
	}
}

// ipv6AddrInfo 一条 `ip -6 -o addr show` 记录（只保留挑池前缀 / 残留清理需要的字段）
type ipv6AddrInfo struct {
	iface      string
	ip         net.IP
	prefixLen  int
	deprecated bool
	temporary  bool
	tentative  bool
	dadFailed  bool
	nodad      bool
	dynamic    bool
}

// parseIPv6Addrs 解析 `ip -6 -o addr show` 的输出，形如：
//
//	2: eth0    inet6 2409:8a1e:2e93:4fe0:5cbb:f6ff:fe9e:eefa/64 scope global dynamic mngtmpaddr \
//	              valid_lft 2591968sec preferred_lft 604768sec
//
// 生命周期/标志必须解析出来：光猫换前缀后旧地址会先转成 deprecated 继续留在网卡上，
// 只看地址本身无法区分新旧。
func parseIPv6Addrs(out string) []ipv6AddrInfo {
	var list []ipv6AddrInfo
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		idx := -1
		for i, field := range fields {
			if field == "inet6" {
				idx = i
				break
			}
		}
		if idx < 0 || idx+1 >= len(fields) {
			continue
		}
		addrCidr := fields[idx+1]
		slash := strings.IndexByte(addrCidr, '/')
		if slash <= 0 {
			continue
		}
		ip := net.ParseIP(addrCidr[:slash])
		ones, err := strconv.Atoi(addrCidr[slash+1:])
		if ip == nil || err != nil {
			continue
		}

		info := ipv6AddrInfo{
			iface:     strings.SplitN(fields[1], "@", 2)[0],
			ip:        ip,
			prefixLen: ones,
		}
		for _, flag := range fields[idx+2:] {
			switch flag {
			case "deprecated":
				info.deprecated = true
			case "temporary":
				info.temporary = true
			case "tentative":
				info.tentative = true
			case "dadfailed":
				info.dadFailed = true
			case "nodad":
				info.nodad = true
			case "dynamic":
				info.dynamic = true
			}
		}
		list = append(list, info)
	}
	return list
}

// listIPv6Addrs 读取网卡（iface 为空表示全部网卡）的全局 IPv6 地址记录
func listIPv6Addrs(iface string) []ipv6AddrInfo {
	args := []string{"-6", "-o", "addr", "show", "scope", "global"}
	if iface != "" {
		args = append(args, "dev", iface)
	}
	out, err := runCmd("ip", args...)
	if err != nil {
		log.Printf("读取网卡 %s 的 IPv6 地址失败: %v", iface, err)
		return nil
	}
	return parseIPv6Addrs(out)
}

// selectPoolAddress 从候选里挑一个可用于池前缀的地址。
// 排除项（换前缀后继续用旧地址就是「全部出口 i/o timeout」的直接原因）：
//   - deprecated：前缀正在作废，CPE 已经不再为它转发回程；
//   - tentative / dadfailed：地址还在做重复地址检测 / 检测失败，绑上去也拨不通。
//
// 稳定地址（非 temporary）优先，只有临时地址时退回临时地址——两者前缀相同。
func selectPoolAddress(list []ipv6AddrInfo, match func(net.IP) bool) (ipv6AddrInfo, bool) {
	var fallback *ipv6AddrInfo
	for i := range list {
		info := list[i]
		if !match(info.ip) || info.deprecated || info.tentative || info.dadFailed {
			continue
		}
		if info.temporary {
			if fallback == nil {
				fallback = &list[i]
			}
			continue
		}
		return info, true
	}
	if fallback != nil {
		return *fallback, true
	}
	return ipv6AddrInfo{}, false
}

// maskCIDR 把地址 + 前缀长度转成掩码后的网络串（如 2409:8a1e:2e93:4fe0::/64）。
// 必须掩码：未掩码的串既不能当池前缀（主机位会残留），也没法拿去和路由表比对
// （`ip route` 显示的一定是掩码后的网络地址）。
func maskCIDR(addr string, ones int) (string, error) {
	_, ipnet, err := net.ParseCIDR(fmt.Sprintf("%s/%d", addr, ones))
	if err != nil {
		return "", err
	}
	return ipnet.String(), nil
}

// currentPoolPrefix 当前可用的池前缀（已掩码）与其来源地址。
// netIf 由 resolveNetIf 探测；探测不到时退化为遍历全部网卡，最后才用外部回显服务兜底。
func currentPoolPrefix() (string, string, error) {
	if netIf != "" {
		if info, ok := selectPoolAddress(listIPv6Addrs(netIf), isPublicIPv6); ok {
			cidr, err := maskCIDR(info.ip.String(), prefixLen)
			if err != nil {
				return "", "", err
			}
			log.Printf("从网卡 %s 获取到公网 IPv6 地址: %s（前缀 %s）", info.iface, info.ip, cidr)
			return cidr, info.ip.String(), nil
		}
		log.Printf("网卡 %s 上没有可用公网 IPv6（已排除 deprecated/tentative/dadfailed 地址）", netIf)
	}

	if info, ok := selectPoolAddress(listIPv6Addrs(""), isPublicIPv6); ok {
		cidr, err := maskCIDR(info.ip.String(), prefixLen)
		if err != nil {
			return "", "", err
		}
		log.Printf("从网卡 %s 获取到公网 IPv6 地址: %s（前缀 %s）", info.iface, info.ip, cidr)
		return cidr, info.ip.String(), nil
	}

	// 实在没有公网地址时退回内网 ULA 前缀：网络可通，但所有出口会被 NAT 成同一地址
	if info, ok := selectPoolAddress(listIPv6Addrs(""), isPrivateIPv6); ok {
		cidr, err := maskCIDR(info.ip.String(), prefixLen)
		if err != nil {
			return "", "", err
		}
		log.Printf("警告：网卡 %s 上只有内网 IPv6 地址 %s，出口地址会被 NAT 成同一地址（需要宿主机具备公网 IPv6 前缀）",
			info.iface, info.ip)
		return cidr, info.ip.String(), nil
	}

	log.Println("本地网卡未找到公网 IPv6 地址，尝试从外部服务获取...")
	addr, err := getIPv6FromService()
	if err != nil {
		return "", "", err
	}
	cidr, err := maskCIDR(addr, prefixLen)
	if err != nil {
		return "", "", err
	}
	return cidr, addr, nil
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

// findIPv6OnInterfaceName 在指定网卡上查找满足条件的 IPv6 地址（含已废弃/临时地址，
// 用于「网卡上有没有公网 IPv6」这类判断，选池前缀请走 selectPoolAddress）
func findIPv6OnInterfaceName(ifaceName string, match func(net.IP) bool) (string, bool) {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		log.Printf("获取网卡 %s 失败: %v", ifaceName, err)
		return "", false
	}
	return matchIPv6OnInterface(*iface, match)
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

// ipv6Monitor 周期复查池前缀，并按需修正内核态配置（local 路由 + NDP 代答段）。
// 前缀变化时只写一次；没变化时全部走只读探测，避免 netlink 变更通知把浏览器网络打断。
func ipv6Monitor() {
	interval := time.Duration(envInt("POOL_CHECK_INTERVAL", poolCheckDefaultSeconds)) * time.Second
	log.Printf("池前缀复查间隔：%s（可用 POOL_CHECK_INTERVAL 覆盖）", interval)

	for {
		// 启动时宿主可能还没有公网 IPv6（RA/DHCPv6 未就绪），网卡探测不出来；每轮重试一次
		if netIf == "" {
			netIf = resolveNetIf()
		}

		wait := interval
		nextCidr, addr, err := currentPoolPrefix()
		switch {
		case err != nil:
			log.Printf("获取 IPv6 地址失败: %v（%d 秒后重试）", err, poolCheckFailSeconds)
			wait = poolCheckFailSeconds * time.Second
		case nextCidr != pool.CIDR():
			applyPoolPrefix(nextCidr, pool.CIDR())
			pool.set(nextCidr, addr)
			updateNdpPool(nextCidr)
		default:
			// 有些环境（如 WSL 镜像网络）会重置路由表，导致前缀的 local 路由消失，
			// 回程包就会被内核当成「不是发给本机的」丢掉；这里每轮幂等地补一次。
			ensureLocalRoute(nextCidr)
		}

		select {
		case <-time.After(wait):
		case <-recheckCh:
			log.Printf("拨号全部失败，立即复查池前缀")
			// 拨号失败通常是出口不可用，先给一点冷却再探测，防止失败风暴空转
			time.Sleep(poolRecheckMinGap)
		}
	}
}

// ensureLocalRoute 幂等地保证池前缀存在 local 路由。
// 不能无脑 `ip route replace`：**即使内容完全一致，replace 也会发 netlink 路由变更
// 通知**，对网络事件敏感的客户端（浏览器网络监视器等）会据此判定「网络已变更」并中断
// 在途连接。本函数被周期性调用，必须在路由已存在时零副作用。
func ensureLocalRoute(poolCidr string) {
	if poolCidr == "" {
		return
	}
	out, err := runCmd("ip", "-6", "route", "show", "table", "local")
	if err == nil && strings.Contains(out, "local "+poolCidr) {
		return
	}
	execCmd("ip", "-6", "route", "replace", "local", poolCidr, "dev", "lo")
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

	log.Printf("从外部服务获取到 IPv6 地址: %s", ipv6)
	return ipv6, nil
}
