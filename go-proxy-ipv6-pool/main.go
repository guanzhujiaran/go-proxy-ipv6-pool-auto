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
	err := godotenv.Load(".env")
	if err != nil {
		log.Fatal("Error loading .env file\n%v", err)
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
