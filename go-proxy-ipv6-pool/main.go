package main

import (
	"crypto/rand"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/elazarl/goproxy"
	"github.com/joho/godotenv"
)

var (
	port      int
	prefixLen int
	cidr      string // 来自 .env 或自动探测
	netIf     string
	runEnv    string

	httpProxy = goproxy.NewProxyHttpServer()
)

func main() {
	flag.IntVar(&prefixLen, "prefix", 64, "IPv6 prefix length (e.g., 64)")
	flag.IntVar(&port, "port", 3128, "HTTP proxy port")
	flag.Parse()

	if err := godotenv.Load(".env"); err != nil && !os.IsNotExist(err) {
		log.Fatalf("Error loading .env file: %v", err)
	}

	netIf = os.Getenv("NET_IF")
	if netIf == "" {
		log.Fatal("NET_IF not set in .env")
	}

	runEnv = os.Getenv("RUN_ENV")
	if runEnv == "" {
		runEnv = "prod"
	}

	// 优先从 .env 读取 CIDR，否则尝试自动获取
	cidr = os.Getenv("CIDR")
	if cidr == "" {
		log.Println("CIDR not set in .env, attempting to detect...")
		detected, err := getLocalIPv6()
		if err != nil {
			log.Fatalf("Failed to detect CIDR and none provided in .env: %v", err)
		}
		cidr = detected
		log.Printf("Detected CIDR: %s", cidr)
	}

	httpPort := port
	socks5Port := port + 1
	if socks5Port > 65535 {
		log.Fatal("port too large")
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// 启动 HTTP 代理
	go func() {
		defer wg.Done()
		if err := http.ListenAndServe(fmt.Sprintf(":%d", httpPort), httpProxy); err != nil {
			log.Fatalf("HTTP server failed: %v", err)
		}
	}()

	// 启动 SOCKS5 代理（需你自己实现 socks5Server）
	go func() {
		defer wg.Done()
		if err := socks5Server.ListenAndServe("tcp", fmt.Sprintf(":%d", socks5Port)); err != nil {
			log.Fatalf("SOCKS5 server failed: %v", err)
		}
	}()

	log.Println("Proxy server running...")
	log.Printf("HTTP  proxy: http://0.0.0.0:%d", httpPort)
	log.Printf("SOCKS5 proxy: socks5://0.0.0.0:%d", socks5Port)

	wg.Wait()
}

// addIPv6ToInterface 幂等地将 IPv6/128 添加到接口
func addIPv6ToInterface(ip, ifname string) error {
	cmd := exec.Command("ip", "-6", "addr", "add", ip+"/128", "dev", ifname)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "File exists") {
			return nil // 已存在，忽略
		}
		return fmt.Errorf("failed to add IP %s: %w (output: %s)", ip, err, out)
	}
	return nil
}

// generateRandomIPv6 从 CIDR 生成随机 IPv6 地址
func generateRandomIPv6(cidrStr string) (string, error) {
	_, ipv6Net, err := net.ParseCIDR(cidrStr)
	if err != nil {
		return "", err
	}

	maskOnes, maskBits := ipv6Net.Mask.Size()
	if maskBits != 128 {
		return "", fmt.Errorf("expected /128 mask for address generation, got /%d", maskOnes)
	}

	// 计算可变位数
	randomBits := 128 - maskOnes
	if randomBits <= 0 {
		return ipv6Net.IP.String(), nil
	}

	randomBytes := make([]byte, (randomBits+7)/8)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", err
	}

	ip := make(net.IP, 16)
	copy(ip, ipv6Net.IP.To16())

	// 将随机字节写入低位
	for i := 0; i < len(randomBytes); i++ {
		byteIndex := 15 - i
		if byteIndex < 0 {
			break
		}
		// 清除原字节中将被覆盖的位（如果 randomBits 不是 8 的倍数）
		if i == len(randomBytes)-1 && randomBits%8 != 0 {
			mask := byte((1 << (randomBits % 8)) - 1)
			ip[byteIndex] = (ip[byteIndex] &^ mask) | (randomBytes[i] & mask)
		} else {
			ip[byteIndex] = randomBytes[i]
		}
	}

	return ip.String(), nil
}

// ========== HTTP Proxy Logic ==========

// ========== 辅助函数 ==========

func getLocalIPv6() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}

	for _, iface := range ifaces {
		if iface.Name != netIf {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.To4() == nil {
				if ipnet.IP.IsGlobalUnicast() && !ipnet.IP.IsLinkLocalUnicast() {
					mask := net.CIDRMask(prefixLen, 128)
					subnetIP := ipnet.IP.Mask(mask)
					_, subnet, _ := net.ParseCIDR(subnetIP.String() + fmt.Sprintf("/%d", prefixLen))
					return subnet.String(), nil
				}
			}
		}
	}
	return "", fmt.Errorf("no global IPv6 found on interface %s", netIf)
}
