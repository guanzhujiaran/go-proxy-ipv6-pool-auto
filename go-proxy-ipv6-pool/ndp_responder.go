package main

import (
	"encoding/binary"
	"log"
	"net"
	"sync"
	"syscall"
	"time"
)

// 自实现的 NDP 代答器。
//
// 为什么不用 ndppd：抓包可见 ndppd 0.2.4 回的 NA 标志是 `Flags [solicited]`，
// **没有 override**，本环境 CPE 不采纳（邻居一直 INCOMPLETE），回程包进不来。
// 而 `ip -6 addr add` 时内核回的 NA 带 `[solicited, override]`，CPE 立刻采纳。
//
// 这里用 AF_PACKET raw socket 抓 NS，对落在池前缀内的目标地址回一条带 override 的 NA：
//   - 不改网卡地址 → 不产生 netlink 地址事件 → Chromium 不会判定「网络已变化」；
//   - NA 带 override → CPE 采纳 → 回程可达（效果等同把地址加到网卡上）。
const (
	icmpv6TypeNeighborSolicitation  = 135
	icmpv6TypeNeighborAdvertisement = 136

	etherTypeIPv6 = 0x86dd
	ipProtoICMPv6 = 58

	naFlagSolicited = 0x40000000
	naFlagOverride  = 0x20000000

	ndOptionTargetLinkLayerAddr = 2
)

var ndpState struct {
	sync.Mutex
	fd       int
	ifindex  int
	hardware net.HardwareAddr
	pool     *net.IPNet
}

// startNdpResponderWhenReady 等池前缀与网卡都确定后启动代答器
// （前缀由 ipv6Monitor 首轮获取，网卡由 resolveNetIf 探测，宿主刚开机时两者都可能还没就绪）
func startNdpResponderWhenReady() {
	for i := 0; i < 60 && (cidr == "" || netIf == ""); i++ {
		time.Sleep(time.Second)
	}
	startNdpResponder()
}

// startNdpResponder 启动 NDP 代答（阻塞，需放在 goroutine 里）。
// poolCIDR 为池前缀（如 2409:8a1e:2e94:84e0::/64），只对该段内的地址代答。
func startNdpResponder() {
	if netIf == "" || cidr == "" {
		log.Printf("NDP 代答：网卡或池前缀未就绪，跳过")
		return
	}
	iface, err := net.InterfaceByName(netIf)
	if err != nil {
		log.Printf("NDP 代答：读取网卡 %s 失败: %v", netIf, err)
		return
	}
	_, pool, err := net.ParseCIDR(cidr)
	if err != nil {
		log.Printf("NDP 代答：解析池前缀 %s 失败: %v", cidr, err)
		return
	}

	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, int(htons(etherTypeIPv6)))
	if err != nil {
		log.Printf("NDP 代答：创建 raw socket 失败（需要 NET_RAW）: %v", err)
		return
	}
	if err := syscall.Bind(fd, &syscall.SockaddrLinklayer{
		Protocol: htons(etherTypeIPv6),
		Ifindex:  iface.Index,
	}); err != nil {
		log.Printf("NDP 代答：绑定网卡 %s 失败: %v", netIf, err)
		_ = syscall.Close(fd)
		return
	}

	ndpState.Lock()
	ndpState.fd = fd
	ndpState.ifindex = iface.Index
	ndpState.hardware = iface.HardwareAddr
	ndpState.pool = pool
	ndpState.Unlock()

	log.Printf("NDP 代答已启动：网卡 %s（%s），代答段 %s，NA 带 override 标志",
		netIf, iface.HardwareAddr, pool.String())

	buf := make([]byte, 2048)
	for {
		n, from, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			if err == syscall.EINTR || err == syscall.EAGAIN {
				continue
			}
			log.Printf("NDP 代答：收包失败: %v", err)
			return
		}
		handleNdPacket(buf[:n], from)
	}
}

// handleNdPacket 解析以太网/IPv6/ICMPv6，命中 NS 且目标属于池前缀时回 NA
func handleNdPacket(frame []byte, from syscall.Sockaddr) {
	// 以太网头 14 字节：dst(6) + src(6) + ethertype(2)
	if len(frame) < 14+40+8 {
		return
	}
	if binary.BigEndian.Uint16(frame[12:14]) != etherTypeIPv6 {
		return
	}

	ip := frame[14:]
	if ip[0]>>4 != 6 || ip[6] != ipProtoICMPv6 || ip[7] != 255 {
		return
	}
	ipPayloadLen := int(binary.BigEndian.Uint16(ip[4:6]))
	if len(ip) < 40 || len(ip) < 40+ipPayloadLen {
		return
	}
	icmp6 := ip[40 : 40+ipPayloadLen]
	if len(icmp6) < 24 || icmp6[0] != icmpv6TypeNeighborSolicitation || icmp6[1] != 0 {
		return
	}

	// NS 报文：type(1) code(1) checksum(2) reserved(4) target(16)
	target := net.IP(icmp6[8:24])
	if target.To4() != nil {
		return
	}

	ndpState.Lock()
	pool := ndpState.pool
	hardware := ndpState.hardware
	fd := ndpState.fd
	ifindex := ndpState.ifindex
	ndpState.Unlock()

	if pool == nil || !pool.Contains(target) {
		return
	}

	// NS 的源地址就是 NA 的目的地（一般是 CPE 的 link-local）
	srcIP := net.IP(ip[8:24])
	if srcIP.To4() != nil || srcIP.IsUnspecified() {
		return
	}
	// NA 的链路层目的地址：优先用 NS 的源 MAC
	var dstMAC net.HardwareAddr
	if ll, ok := from.(*syscall.SockaddrLinklayer); ok && ll.Halen >= 6 {
		dstMAC = net.HardwareAddr(ll.Addr[:6])
	}
	if dstMAC == nil {
		dstMAC = net.HardwareAddr(frame[6:12])
	}

	frameOut := buildNeighborAdvertisement(target, srcIP, hardware, dstMAC)
	if err := syscall.Sendto(fd, frameOut, 0, &syscall.SockaddrLinklayer{
		Protocol: htons(etherTypeIPv6),
		Ifindex:  ifindex,
		Halen:    6,
		Addr:     toSockaddrAddr(dstMAC),
	}); err != nil {
		log.Printf("NDP 代答：发送 NA(%s) 失败: %v", target, err)
	}
}

// buildNeighborAdvertisement 构造 NA 帧：flags = solicited|override + target link-layer 选项
func buildNeighborAdvertisement(target, dstIP net.IP, srcMAC, dstMAC net.HardwareAddr) []byte {
	icmp6 := make([]byte, 32)
	icmp6[0] = icmpv6TypeNeighborAdvertisement
	icmp6[1] = 0
	// checksum 先置 0，最后计算
	binary.BigEndian.PutUint32(icmp6[4:8], naFlagSolicited|naFlagOverride)
	copy(icmp6[8:24], target.To16())
	icmp6[24] = ndOptionTargetLinkLayerAddr
	icmp6[25] = 1 // 选项长度：1 个 8 字节
	copy(icmp6[26:32], srcMAC)

	ip := make([]byte, 40)
	ip[0] = 0x60
	binary.BigEndian.PutUint16(ip[4:6], uint16(len(icmp6)))
	ip[6] = ipProtoICMPv6
	ip[7] = 255
	copy(ip[8:24], target.To16()) // 源地址用 target（RFC 4861 §7.2.4）
	copy(ip[24:40], dstIP.To16())

	binary.BigEndian.PutUint16(icmp6[2:4], icmpv6Checksum(ip[8:24], ip[24:40], icmp6))

	frame := make([]byte, 14+len(ip)+len(icmp6))
	copy(frame[0:6], dstMAC)
	copy(frame[6:12], srcMAC)
	binary.BigEndian.PutUint16(frame[12:14], etherTypeIPv6)
	copy(frame[14:], ip)
	copy(frame[14+len(ip):], icmp6)
	return frame
}

// icmpv6Checksum 计算 ICMPv6 校验和（含 IPv6 伪头）
func icmpv6Checksum(src, dst, payload []byte) uint16 {
	pseudo := make([]byte, 40+len(payload))
	copy(pseudo[0:16], src)
	copy(pseudo[16:32], dst)
	binary.BigEndian.PutUint32(pseudo[32:36], uint32(len(payload)))
	pseudo[39] = ipProtoICMPv6
	copy(pseudo[40:], payload)

	var sum uint32
	for i := 0; i+1 < len(pseudo); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(pseudo[i : i+2]))
	}
	if len(pseudo)%2 == 1 {
		sum += uint32(pseudo[len(pseudo)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

// htons 主机序转网络序（按 16 位交换）
func htons(v uint16) uint16 {
	return v<<8 | v>>8
}

// toSockaddrAddr 把 6 字节 MAC 转成 SockaddrLinklayer 的地址字段
func toSockaddrAddr(mac net.HardwareAddr) [8]byte {
	var addr [8]byte
	copy(addr[:], mac)
	return addr
}
