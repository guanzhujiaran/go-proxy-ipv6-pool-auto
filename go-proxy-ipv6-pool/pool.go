package main

import (
	"log"
	"math/rand/v2"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 出口池：预先把一批随机地址挂到网卡上，请求时从池里随机取。
//
// 相比「每个请求 add 一个全新地址、用完删掉」：
//   - 不用每次改接口地址，省掉每条请求两次 ip 命令与内核路由/组播组增删；
//   - 地址常驻，CPE 邻居表命中缓存，首包不再丢（dial 从 ~1.1s 降到 ~165ms）；
//   - 并发请求互不干扰，地址始终在接口上；
//   - CPE 邻居表条目数稳定在池大小，不会随历史地址不断膨胀。
//
// 不能真的「批量绑完整个 /64」（2^64 个地址），池大小受 CPE 邻居表容量与内核
// 路由/组播组数量限制，因此默认 128 个，并按 TTL 整批轮换。
const (
	defaultPoolSize    = 128
	defaultPoolTTL     = 600 * time.Second
	defaultPoolMaxUses = 50
)

type exitPool struct {
	mu      sync.Mutex
	addrs   []string
	uses    map[string]int
	size    int
	maxUses int
	ttl     time.Duration
}

// exitPoolInstance 全局出口池，按环境变量初始化
var exitPoolInstance = &exitPool{
	uses:    make(map[string]int),
	size:    envInt("EXIT_POOL_SIZE", defaultPoolSize),
	maxUses: envInt("EXIT_POOL_MAX_USES", defaultPoolMaxUses),
	ttl:     time.Duration(envInt("EXIT_POOL_TTL", int(defaultPoolTTL/time.Second))) * time.Second,
}

func envInt(key string, def int) int {
	if raw := os.Getenv(key); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// initExitPoolFromEnv 重新读取池参数，需在 godotenv 加载之后调用
// （包级变量初始化早于 main 里的 godotenv.Load，直接在初始化时读会漏掉 .env）
func initExitPoolFromEnv() {
	exitPoolInstance.size = envInt("EXIT_POOL_SIZE", defaultPoolSize)
	exitPoolInstance.maxUses = envInt("EXIT_POOL_MAX_USES", defaultPoolMaxUses)
	exitPoolInstance.ttl = time.Duration(envInt("EXIT_POOL_TTL", int(defaultPoolTTL/time.Second))) * time.Second
}

// start 等前缀就绪后清理残留、建池，并启动整批轮换
func (p *exitPool) start() {
	go func() {
		for i := 0; i < 60 && cidr == ""; i++ {
			time.Sleep(time.Second)
		}
		if cidr == "" {
			log.Printf("出口池：未拿到池前缀，禁用池化（回落到按请求生成地址）")
			return
		}
		p.cleanupStale()
		p.refill()
		ticker := time.NewTicker(p.ttl)
		defer ticker.Stop()
		for range ticker.C {
			p.refill()
		}
	}()
}

// cleanupStale 删除上次运行残留在网卡上的池地址（容器重启不会清掉 host netns 的地址）
func (p *exitPool) cleanupStale() {
	real, err := getLocalIPv6()
	if err != nil {
		log.Printf("出口池：读取本机地址失败，跳过残留清理: %v", err)
		return
	}
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		log.Printf("出口池：解析池前缀失败: %v", err)
		return
	}

	out, err := runCmd("ip", "-6", "-o", "addr", "show", "dev", netIf)
	if err != nil {
		log.Printf("出口池：读取网卡地址失败: %v", err)
		return
	}

	removed := 0
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		for i, field := range fields {
			if field != "inet6" || i+1 >= len(fields) {
				continue
			}
			addr := strings.SplitN(fields[i+1], "/", 2)[0]
			ip := net.ParseIP(addr)
			if ip == nil || !ipnet.Contains(ip) || addr == real {
				continue
			}
			delExitAddress(addr)
			removed++
		}
	}
	if removed > 0 {
		log.Printf("出口池：清理上次残留地址 %d 个", removed)
	}
}

// refill 整批换新：先删旧池，再挂 size 个新随机地址
func (p *exitPool) refill() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, addr := range p.addrs {
		delExitAddress(addr)
	}
	p.addrs = p.addrs[:0]
	p.uses = make(map[string]int, p.size)

	for len(p.addrs) < p.size {
		ip, err := generateRandomIPv6(cidr)
		if err != nil {
			log.Printf("出口池：生成随机地址失败: %v", err)
			break
		}
		if !addExitAddress(ip) {
			// 挂不上地址（多半缺 NET_ADMIN）：清空池，回落到按请求生成
			log.Printf("出口池：地址挂载失败，禁用池化")
			for _, a := range p.addrs {
				delExitAddress(a)
			}
			p.addrs = p.addrs[:0]
			return
		}
		p.addrs = append(p.addrs, ip)
	}
	log.Printf("出口池就绪：%d 个随机地址已挂到 %s，%s 后整批轮换", len(p.addrs), netIf, p.ttl)
}

// pick 随机取一个未用尽的地址；池不可用时返回 false
func (p *exitPool) pick() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.addrs) == 0 {
		return "", false
	}
	start := rand.IntN(len(p.addrs))
	for i := 0; i < len(p.addrs); i++ {
		addr := p.addrs[(start+i)%len(p.addrs)]
		if p.uses[addr] >= p.maxUses {
			continue
		}
		p.uses[addr]++
		return addr, true
	}
	return "", false
}

// drop 剔除连不通的地址并补一个新的，保持池大小
func (p *exitPool) drop(addr string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	found := false
	for i, item := range p.addrs {
		if item == addr {
			p.addrs = append(p.addrs[:i], p.addrs[i+1:]...)
			found = true
			break
		}
	}
	delete(p.uses, addr)
	if !found {
		return
	}
	delExitAddress(addr)

	// 锁内补齐：一次 exec，开销可接受，避免并发补充超量
	if ip, err := generateRandomIPv6(cidr); err == nil && addExitAddress(ip) {
		p.addrs = append(p.addrs, ip)
		log.Printf("出口池：剔除 %s，已补充新地址", addr)
	}
}
