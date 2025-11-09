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
		err := http.ListenAndServe(fmt.Sprintf("0.0.0.0:%d", httpPort), httpProxy)
		if err != nil {
			log.Fatal("http Server err", err)
		}
	}()
	execCmd("sysctl", "net.ipv6.ip_nonlocal_bind=1")
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
