package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"time"
)

// 拨号参数做成变量（可用环境变量覆盖）：坏出口是「确定不通」，早放弃早换比长等更划算。
// 实测可用出口建连只需 165ms~1.1s（首包触发邻居解析时约 1.1s），5s 余量充足。
var (
	// dialTimeout 单次随机出口的拨号超时：超过该时间仍未连通，就认为这个出口不可用
	dialTimeout = 5 * time.Second
	// dialMaxAttempts 建立一条连接最多尝试的随机出口数量
	dialMaxAttempts = 5
	// requestTimeout 单次客户端请求的总超时（拨号重试 + 等待响应），超过后直接回 504
	requestTimeout = time.Duration(dialMaxAttempts)*dialTimeout + 20*time.Second
)

// initDialTuning 从环境变量读取拨号参数，需在 godotenv 加载之后调用
func initDialTuning() {
	dialTimeout = time.Duration(envInt("DIAL_TIMEOUT_SECONDS", int(dialTimeout/time.Second))) * time.Second
	dialMaxAttempts = envInt("DIAL_MAX_ATTEMPTS", dialMaxAttempts)
	requestTimeout = time.Duration(dialMaxAttempts)*dialTimeout + 20*time.Second
	log.Printf("拨号参数：单次超时 %s，最多 %d 个随机出口，请求总超时 %s",
		dialTimeout, dialMaxAttempts, requestTimeout)
}

// dialWithRandomIPv6 用随机 IPv6 出口拨号；单次超过 dialTimeout 未连通或拨号出错时，
// 换一个随机出口重试，最多 dialMaxAttempts 次。
// tag 只用于日志区分调用方（如 "[http]"、"[http] CONNECT"、"[socks5]"）。
// 返回已建立的连接，以及该连接实际使用的出口地址（已加方括号）。
func dialWithRandomIPv6(ctx context.Context, tag, network, addr string) (net.Conn, string, error) {
	// 目标是 IPv4 字面量（SOCKS5 常见）时无法用 IPv6 出口地址绑定，直接走默认路由，
	// 否则会报 "no suitable address found" 并且白白重试三次。
	if host, _, err := net.SplitHostPort(addr); err == nil {
		if ip := net.ParseIP(host); ip != nil && ip.To4() != nil {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				log.Printf("%s dial %s via ipv4 failed: %v", tag, addr, err)
				return nil, "", err
			}
			return conn, "ipv4", nil
		}
	}

	var lastErr error
	for attempt := 1; attempt <= dialMaxAttempts; attempt++ {
		// 每个请求用一个全新的随机地址作为源地址，只 bind、不改网卡地址：
		// 网络配置在启动/前缀变化时已配好（local 路由 + proxy_ndp），
		// 请求期间只写内核 neigh 表（没有地址事件），避免浏览器的 network-change 检测。
		poolCidr := pool.CIDR()
		if poolCidr == "" {
			// 池前缀还没就绪（宿主刚开机 / 光猫刚重启），没有源地址可用
			requestPoolRecheck()
			return nil, "", fmt.Errorf("pool prefix not ready yet")
		}
		randomIP, err := generateRandomIPv6(poolCidr)
		if err != nil {
			// 生成出口地址失败与出口是否可用无关，重试没有意义
			log.Printf("%s Generate random IPv6 error: %v", tag, err)
			return nil, "", err
		}
		outgoingIP := "[" + randomIP + "]"

		// 该随机地址不在网卡上，CPE 回程前要先解析它：必须让内核替它代答 NS，
		// 否则 NS 无人应答，CPE 一直 INCOMPLETE，dial 必然 i/o timeout（见 proxy_ndp.go）。
		proxyNeighborAdd(randomIP)

		localAddr, err := net.ResolveTCPAddr("tcp", outgoingIP+":0")
		if err != nil {
			lastErr = err
			log.Printf("%s Resolve local address %s error: %v", tag, outgoingIP, err)
			continue
		}

		dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
		dialer := net.Dialer{
			LocalAddr: localAddr,
		}
		start := time.Now()
		conn, err := dialer.DialContext(dialCtx, network, addr)
		cancel()
		if err == nil {
			return conn, outgoingIP, nil
		}

		lastErr = err
		// 单个随机出口失败是常态（换下一个即可），只在 debug 模式逐条打印
		debugLog("%s dial %s via %s failed in %s (%d/%d): %v",
			tag, addr, outgoingIP, time.Since(start).Round(time.Millisecond), attempt, dialMaxAttempts, err)
	}
	log.Printf("%s dial %s failed, all %d random exits exhausted", tag, addr, dialMaxAttempts)
	// 全部随机出口都不通，最可能是池前缀已失效（光猫换了前缀、本地还留着旧地址）：
	// 立刻让 monitor 复查一次，而不是等下一个定时周期。
	requestPoolRecheck()
	return nil, "", lastErr
}
