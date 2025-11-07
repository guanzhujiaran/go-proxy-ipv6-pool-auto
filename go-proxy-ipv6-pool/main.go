package main

import (
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/joho/godotenv"
)

var port int
var prefixLen int
var cidr = ""
var ipv6Addr = ""
var netIf = ""

// runEnv 表示运行环境，可选值为 "prod" 或 "dev"
var runEnv string

func main() {
	flag.IntVar(&prefixLen, "prefix", 64, "ipv6 prefix length")
	flag.IntVar(&port, "port", 3128, "server port")
	flag.Parse()
	err := godotenv.Load(".env.dev", ".env")
	if err != nil {
		log.Fatal("Error loading .env file")
	}
	netIf = os.Getenv("NET_IF")
	runEnv = os.Getenv("RUN_ENV")
	httpPort := port
	socks5Port := port + 1

	if socks5Port > 65535 {
		log.Fatal("port too large")
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go ipv6Monitor()

	go func() {
		err := socks5Server.ListenAndServe("tcp", fmt.Sprintf("0.0.0.0:%d", socks5Port))
		if err != nil {
			log.Fatal("socks5 Server err:", err)
		}

	}()
	go func() {
		err := http.ListenAndServe(fmt.Sprintf("0.0.0.0:%d", httpPort), httpProxy)
		if err != nil {
			log.Fatal("http Server err", err)
		}
	}()

	log.Println("server running ...")
	log.Printf("http running on 0.0.0.0:%d", httpPort)
	log.Printf("socks5 running on 0.0.0.0:%d", socks5Port)
	wg.Wait()

}
func execCmd(name string, arg ...string) {
	log.Printf("执行命令: %s %s", name, arg)
	c := exec.Command(name, arg...)
	output, err := c.CombinedOutput()
	if err != nil {
		log.Printf("命令执行失败: %v\n执行输出：%s", err, output)
		return
	}
	if len(output) > 0 {
		log.Printf("命令输出: %s", string(output))
	}
}

func changeNdppdConfig() {
	log.Printf("变更 ndppd 配置文件：%s", cidr)
	execCmd("ip", "route", "del", "local", cidr, "dev", "lo")
	execCmd("ip", "route", "add", "local", cidr, "dev", "lo")

	// 写 ndppd 配置
	confContent := fmt.Sprintf(`
route-ttl 30000
proxy %s {
	# router no
    rule %s {
        static
    }
}
`, netIf, cidr)

	if err := os.WriteFile("/etc/ndppd.conf", []byte(confContent), 0644); err != nil {
		log.Printf("写入 ndppd.conf 失败: %v", err)
		return
	}

	execCmd("service", "ndppd", "restart")
	// 可选：省略 sysctl，让 ndppd 自动处理 proxy_ndp
}

func getLocalIPv6() (string, error) {
	log.Println("尝试从本地网卡获取 IPv6 地址...")
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
				log.Printf("从网卡 %s 获取到 IPv6 地址: %s", iface.Name, ip.String())
				mask := net.CIDRMask(prefixLen, 128)
				subnetIP := ip.Mask(mask)
				_, subnet, _ := net.ParseCIDR(subnetIP.String() + "/64")
				return subnet.String(), nil
			}
		}
	}

	log.Println("本地网卡未找到公网 IPv6 地址，尝试从外部服务获取...")
	return getIPv6FromService()
}

func ipv6Monitor() {
	prevCidr := ""     // 初始的ip前缀
	prevIPv6Addr := "" // 初始的IPv6地址

	for {
		currentCidr, err := getLocalIPv6()
		if err != nil {
			log.Println("获取 IPv6 地址失败:", err)
			time.Sleep(600 * time.Second)
			continue
		}

		if currentCidr != prevCidr || ipv6Addr != prevIPv6Addr {
			cidr = currentCidr
			if runEnv == "dev" {
				log.Printf("获取到 IPv6 地址变动 currentCidr:[%s], currentIPv6Addr:[%s]", currentCidr, ipv6Addr)
			}
			changeNdppdConfig()
			prevCidr = currentCidr
			prevIPv6Addr = ipv6Addr
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

	mask := net.CIDRMask(prefixLen, 128)
	subnetIP := net.ParseIP(ipv6).Mask(mask)
	_, subnet, _ := net.ParseCIDR(subnetIP.String() + "/64")
	result := subnet.String()
	if result == "" {
		log.Println("从外部服务获取到的 IPv6 地址为空")
		return "", fmt.Errorf("从外部服务获取到的 IPv6 地址为空")
	}

	log.Printf("从外部服务获取到 IPv6 地址: %s", result)
	return result, nil
}

func generateRandomIPv6(cidr string) (string, error) {
	// 解析CIDR
	_, ipv6Net, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", err
	}

	// 获取网络部分和掩码长度
	maskSize, _ := ipv6Net.Mask.Size()

	// 计算随机部分的长度
	randomPartLength := 128 - maskSize

	// 生成随机部分
	randomPart := make([]byte, randomPartLength/8)
	_, err = rand.Read(randomPart)
	if err != nil {
		return "", err
	}

	// 获取网络部分
	networkPart := ipv6Net.IP.To16()

	// 合并网络部分和随机部分
	for i := 0; i < len(randomPart); i++ {
		networkPart[16-len(randomPart)+i] = randomPart[i]
	}

	return networkPart.String(), nil
}
