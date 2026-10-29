package main

import (
	"context"
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
		// 优先从常驻出口池里随机取（地址已在网卡上，命中 CPE 邻居缓存，首包不丢）；
		// 池未就绪时回落到「按请求生成 + 临时挂地址 + 用完删除」。
		randomIP, pooled := exitPoolInstance.pick()
		if !pooled {
			ip, err := generateRandomIPv6(cidr)
			if err != nil {
				// 生成出口地址失败与出口是否可用无关，重试没有意义
				log.Printf("%s Generate random IPv6 error: %v", tag, err)
				return nil, "", err
			}
			randomIP = ip
		}
		outgoingIP := "[" + randomIP + "]"

		// 使用指定的出口 IP 地址创建连接
		localAddr, err := net.ResolveTCPAddr("tcp", outgoingIP+":0")
		if err != nil {
			lastErr = err
			log.Printf("%s Resolve local address %s error: %v", tag, outgoingIP, err)
			if pooled {
				exitPoolInstance.drop(randomIP)
			}
			continue
		}

		// 池外地址才需要临时挂到网卡上（nodad 立即生效）：
		// 只有真实存在于接口上的地址，CPE 才会把回程包送过来。
		// 挂不上时（缺 NET_ADMIN）回落到内核 proxy_ndp 条目 / ndppd 代答。
		addrAdded := false
		proxyAdded := false
		if !pooled {
			addrAdded = addExitAddress(randomIP)
			if !addrAdded {
				proxyAdded = addNdpProxyEntry(randomIP)
			}
		}

		dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
		dialer := net.Dialer{
			LocalAddr: localAddr,
		}
		start := time.Now()
		conn, err := dialer.DialContext(dialCtx, network, addr)
		cancel()
		if addrAdded {
			delExitAddress(randomIP)
		}
		if proxyAdded {
			delNdpProxyEntry(randomIP)
		}
		if err == nil {
			return conn, outgoingIP, nil
		}
		if pooled {
			exitPoolInstance.drop(randomIP)
		}

		lastErr = err
		log.Printf("%s dial %s via %s failed in %s (%d/%d): %v",
			tag, addr, outgoingIP, time.Since(start).Round(time.Millisecond), attempt, dialMaxAttempts, err)
	}
	log.Printf("%s dial %s failed, all %d random exits exhausted", tag, addr, dialMaxAttempts)
	return nil, "", lastErr
}
