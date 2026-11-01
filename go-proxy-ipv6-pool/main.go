package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"github.com/joho/godotenv"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
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
		log.Fatalf("Error loading .env file: %v", err)
	}
	netIf = os.Getenv("NET_IF")
	runEnv = os.Getenv("RUN_ENV")
	// 拨号参数从环境变量读取（.env 已在上面加载）
	initDialTuning()
	// 随机 IPv6 出口是本机网卡上不存在的地址，内核必须允许 bind 非本机地址；
	// 容器内 /proc/sys 通常只读，宿主机已配置时这里会直接跳过
	ensureNonLocalBind()
	// 回程包的 NS 由内置 NDP 代答器应答（NA 带 override，CPE 才采纳）。
	// ndppd 回的 NA 没有 override，实测 CPE 不采纳，这里直接停掉它，避免双应答干扰。
	if _, err := runCmd("pkill", "-x", "ndppd"); err == nil {
		log.Printf("已停止 ndppd（改由内置 NDP 代答器应答，NA 带 override）")
	}
	// 清理旧版本（每请求 add/del 地址）残留在网卡上的出口地址
	go cleanupStaleExitAddresses()
	// 等池前缀就绪后启动 NDP 代答器
	go startNdpResponderWhenReady()

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

// execCmd 执行外部命令并记录日志（命令失败只提示，不影响主流程）
func execCmd(name string, arg ...string) {
	log.Printf("执行命令: %s %s", name, arg)
	output, err := runCmd(name, arg...)
	if err != nil {
		log.Printf("命令执行失败: %v\n执行输出：%s", err, output)
		return
	}
	if len(output) > 0 {
		log.Printf("命令输出: %s", string(output))
	}
}

// runCmd 执行外部命令并返回合并输出；错误原样返回，由调用方决定是报错还是降级提示。
func runCmd(name string, arg ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, name, arg...)
	// 服务脚本（如 ndppd 的 init 脚本）启动守护进程后，守护进程会继承 stdout/stderr 管道，
	// CombinedOutput 等不到 EOF 就会一直卡住；WaitDelay 到期后强制关闭管道解除阻塞。
	c.WaitDelay = 5 * time.Second
	output, err := c.CombinedOutput()
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return string(output), err
	}
	return string(output), nil
}

// nonLocalBindKey 允许 bind 本机网卡上不存在的地址，随机 IPv6 出口依赖它
const nonLocalBindKey = "net.ipv6.ip_nonlocal_bind"

// nonLocalBindProcPath sysctl 键对应的 /proc 路径，读它不需要特权
const nonLocalBindProcPath = "/proc/sys/net/ipv6/ip_nonlocal_bind"

// ensureNonLocalBind 确保 net.ipv6.ip_nonlocal_bind=1。
// host 网络模式下该键与宿主机共享 netns：宿主机已配置时直接跳过，避免容器内做无意义的写入；
// 容器内 /proc/sys 通常是只读挂载，写不动时只给宿主机侧的处理办法，不当作程序错误。
func ensureNonLocalBind() {
	if readNonLocalBind() == "1" {
		return
	}

	log.Printf("%s 未开启，尝试写入 1", nonLocalBindKey)
	output, err := runCmd("sysctl", nonLocalBindKey+"=1")
	if err == nil && readNonLocalBind() == "1" {
		log.Printf("%s 已开启", nonLocalBindKey)
		return
	}

	if err != nil {
		log.Printf("警告：%s 写入失败（%v）：%s", nonLocalBindKey, err, strings.TrimSpace(output))
	} else {
		log.Printf("警告：%s 写入后仍未生效", nonLocalBindKey)
	}
	log.Printf("警告：容器内 /proc/sys 为只读挂载，请在宿主机执行 %s 后重启容器：sudo sysctl -w %s=1",
		nonLocalBindKey, nonLocalBindKey)
	log.Printf("持久化：echo '%s=1' | sudo tee /etc/sysctl.d/99-ipv6-pool.conf", nonLocalBindKey)
}

// readNonLocalBind 读取当前值，读不到时返回空串
func readNonLocalBind() string {
	b, err := os.ReadFile(nonLocalBindProcPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// envInt 读取正整数环境变量，缺失或非法时返回默认值
func envInt(key string, def int) int {
	if raw := os.Getenv(key); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return def
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
