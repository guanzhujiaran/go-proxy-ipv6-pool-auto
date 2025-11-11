package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"time"
)

func changeNdppdConfig(currentIpv6 string, prevIpv6 string) {
	addAddr := fmt.Sprintf("%s/%d", currentIpv6, prefixLen)
	prevAddr := fmt.Sprintf("%s/%d", prevIpv6, prefixLen)
	log.Printf("变更 ndppd 配置文件：%s", addAddr)
	//这里实际只要添加当前ip然后加一个/64的掩码就行了，ndppd的配置也是，不需要添加标准的cidr
	execCmd("ip", "route", "del", "local", prevAddr, "dev", "lo") //删除上一个ip
	execCmd("ip", "route", "add", "local", addAddr, "dev", "lo")

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

	execCmd("service", "ndppd", "restart")
}

func getLocalIPv6() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		log.Printf("获取网卡列表失败: %v", err)
		return getIPv6FromService()
	}

	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			log.Printf("获取网卡 %s 地址失败: %v", iface.Name, err)
			continue
		}

		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err != nil {
				continue
			}

			if ip.To4() == nil && ip.IsGlobalUnicast() {
				ipStr := ip.String()
				log.Printf("从网卡 %s 获取到 IPv6 地址: %s", iface.Name, ipStr)
				return ipStr, nil
			}
		}
	}

	log.Println("本地网卡未找到公网 IPv6 地址，尝试从外部服务获取...")
	return getIPv6FromService()
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
		}

		time.Sleep(600 * time.Second)
	}
}

func getIPv6FromService() (string, error) {
	log.Println("正在通过外部服务 6.ipw.cn 获取 IPv6 地址...")

	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	var resp *http.Response
	var err error
	maxRetries := 3

	for i := 0; i < maxRetries; i++ {
		resp, err = client.Get("http://6.ipw.cn")
		if err == nil {
			break
		}
		log.Printf("请求 6.ipw.cn 失败 (尝试 %d/%d): %v", i+1, maxRetries, err)
		time.Sleep(2 * time.Second)
	}

	if err != nil {
		log.Printf("请求 6.ipw.cn 最终失败: %v", err)
		return "", fmt.Errorf("请求 6.ipw.cn 失败: %v", err)
	}

	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			log.Printf("关闭响应体失败: %v", err)
		}
	}(resp.Body)

	if resp.StatusCode != http.StatusOK {
		log.Printf("6.ipw.cn 返回状态码: %d", resp.StatusCode)
		return "", fmt.Errorf("6.ipw.cn 返回状态码: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("读取响应失败: %v", err)
		return "", fmt.Errorf("读取响应失败: %v", err)
	}

	ipv6 := string(body)
	if net.ParseIP(ipv6) == nil {
		log.Printf("无效的 IPv6 地址: %s", ipv6)
		return "", fmt.Errorf("无效的 IPv6 地址: %s", ipv6)
	}
	result := ipv6
	if result == "" {
		log.Println("从外部服务获取到的 IPv6 地址为空")
		return "", fmt.Errorf("从外部服务获取到的 IPv6 地址为空")
	}

	log.Printf("从外部服务获取到 IPv6 地址: %s", result)
	return result, nil
}
