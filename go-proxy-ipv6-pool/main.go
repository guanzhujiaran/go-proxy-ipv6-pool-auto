package main

import (
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/joho/godotenv"
)

var port int
var prefixLen int
var cidr = ""
var net_if = ""

func main() {
	flag.IntVar(&prefixLen, "prefix", 60, "ipv6 prefix length")
	flag.IntVar(&port, "port", 3128, "server port")
	flag.Parse()
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}

	net_if = os.Getenv("NET_IF")
	execCmd("sysctl net.ipv6.ip_nonlocal_bind=1")
	execCmd(fmt.Sprintf("sysctl -w net.ipv6.conf.%s.accept_dad=0", net_if))

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
	log.Printf("ipv6 cidr:[%s]", cidr)
	wg.Wait()

}
func execCmd(cmd string) {
	log.Printf("执行命令: %s", cmd)
	c := exec.Command("/bin/sh", "-c", cmd)
	output, err := c.CombinedOutput()
	if err != nil {
		log.Printf("命令执行失败: %v\n输出: %s", err, string(output))
		return
	}
	if len(output) > 0 {
		log.Printf("命令输出: %s", string(output))
	}
}

func changeNdppdConfig(cidr string) {
	func() {
		file, err := os.Create("/etc/ndppd.conf")
		if err != nil {
			log.Fatal(err)
		}
		defer func(file *os.File) {
			_ = file.Close()
		}(file) // 重要：关闭文件
		// 使用 io.WriteString 写入字符串
		_, err = io.WriteString(file, fmt.Sprintf(`
route-ttl 30000
proxy %s {
    router no
    timeout 10000
    ttl 30000
    rule %s {
        static
    }
}
`, net_if, cidr),
		)
	}()

	execCmd("service ndppd restart")
	execCmd(fmt.Sprintf("ip route del local %s dev %s", cidr, net_if))
	execCmd(fmt.Sprintf("ip route add local %s dev %s", cidr, net_if))

}

func ipv6Monitor() {
	originPrefix := "" // 初始的ip前缀

	for true {
		interFace, err := net.InterfaceByName(net_if)
		if err != nil {
			log.Println("获取网络接口失败:", err)
			time.Sleep(60 * time.Second)
			continue
		}
		addrs, err := interFace.Addrs()
		if err != nil {
			time.Sleep(60 * time.Second)
			continue
		}

		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP.To4() != nil {
				continue
			}
			if ipNet.IP.To16() != nil && ipNet.IP.IsGlobalUnicast() {
				prefix, _ := netip.AddrFrom16([16]byte(ipNet.IP.To16())).Prefix(prefixLen)
				prefixStr := prefix.String()
				if prefixStr != originPrefix {
					cidr = prefix.String()
					log.Printf("获取到网卡ipv6地址变动 cidr:[%s]", cidr)
					changeNdppdConfig(cidr)
					originPrefix = prefixStr
				}
			}
		}

		time.Sleep(60 * time.Second)
	}
}

func generateRandomIPv6(cidr string) (string, error) {
	// 解析CIDR
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return "", err
	}

	// 获取网络部分和掩码长度
	maskSize := prefix.Bits()

	// 计算随机部分的长度
	randomPartLength := 128 - maskSize

	// 生成随机部分
	randomPart := make([]byte, randomPartLength/8)
	_, err = rand.Read(randomPart)
	if err != nil {
		return "", err
	}

	// 获取网络部分
	networkPart := prefix.Addr().As16()

	// 合并网络部分和随机部分
	for i := 0; i < len(randomPart); i++ {
		networkPart[16-len(randomPart)+i] = randomPart[i]
	}

	// 转换为netip.Addr并返回字符串
	ip := netip.AddrFrom16(networkPart)

	return ip.String(), nil
}
