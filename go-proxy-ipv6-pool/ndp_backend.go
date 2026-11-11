package main

import (
	"log"
	"os"
	"strings"
)

// NDP 回程代答的三种后端。
//
// 背景：出口地址取自整个 /64，回程时 CPE 要先解析这个地址（发 NS）。
// 谁来应答 NS 决定了两条完全不同的运维路径：
//
//	ndppd     —— 由 ndppd（系统包，路径 /usr/sbin/ndppd）对整段 /64 代答。
//	             程序只负责生成 /etc/ndppd.conf 并把 ndppd 拉起来，内核 neigh 表一条都不写。
//	userspace —— 项目内置代答器（ndp_responder.go，AF_PACKET 自己收 NS/发 NA）。
//	             同样不写内核条目，仅作为 ndppd 不可用时的兜底，不推荐。
//	kernel    —— 内核 proxy_ndp，用哪个地址就给哪个地址写一条 pneigh。
//	             内核没有「整段通配」，一个 /64 不可能一条条 add，只适合小规模场景。
//
// 注意用户态代答（ndppd / 内置）必须能让代答器「收得到」NS：NS 发往目标地址的
// solicited-node 组播组（ff02::1:ffXX:XXXX），没加入该组的帧在网卡层就被过滤掉了。
// 整段 /64 不可能逐个加组（那等于把组播组表撑爆，正是本次要消除的开销），
// 因此打开 allmulticast（见 ensureAllMulticast）。
const (
	ndpBackendNdppd     = "ndppd"
	ndpBackendUserspace = "userspace"
	ndpBackendKernel    = "kernel"
)

// ndpBackend 当前生效的回程代答后端，由 initNdpBackend 在加载 .env 之后设置
var ndpBackend = ndpBackendNdppd

// ndpNdppd 是否由 ndppd 代答（默认）
func ndpNdppd() bool {
	return ndpBackend == ndpBackendNdppd
}

// ndpUserspace 是否由内置代答器代答
func ndpUserspace() bool {
	return ndpBackend == ndpBackendUserspace
}

// ndpNoKernelEntries 用户态代答（ndppd / 内置）都不写任何内核 neigh 条目
func ndpNoKernelEntries() bool {
	return ndpNdppd() || ndpUserspace()
}

// initNdpBackend 读取 NDP_BACKEND：
//
//	ndppd（默认）—— 由 ndppd 对整段 /64 代答，不写任何 neigh 条目；
//	userspace    —— 内置代答器兜底（ndppd 装不上/起不来时才用）；
//	kernel       —— 内核 proxy_ndp + 用哪个地址就 add 一条 pneigh；
//	auto         —— proxy_ndp 能开就用 kernel，开不了回落 userspace。
func initNdpBackend() {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("NDP_BACKEND")))
	switch raw {
	case "", ndpBackendNdppd:
		ndpBackend = ndpBackendNdppd
		log.Printf("NDP 后端：ndppd（整段 /64 由 ndppd 代答，内核 neigh 表不写任何条目）")
	case ndpBackendUserspace, "builtin":
		ndpBackend = ndpBackendUserspace
		log.Printf("NDP 后端：userspace（内置代答器兜底，内核 neigh 表不写任何条目）")
	case ndpBackendKernel:
		ndpBackend = ndpBackendKernel
		log.Printf("NDP 后端：kernel（内核 proxy_ndp + 按需逐条 neigh add）")
	case "auto":
		// 内核后端要求 proxy_ndp 能开；开不了就只能靠用户态代答
		if ensureProxyNdp() {
			ndpBackend = ndpBackendKernel
			log.Printf("NDP 后端：auto -> kernel（proxy_ndp 可用）")
			return
		}
		ndpBackend = ndpBackendUserspace
		log.Printf("NDP 后端：auto -> userspace（proxy_ndp 不可用，改由内置代答器覆盖整段 /64）")
	default:
		ndpBackend = ndpBackendNdppd
		log.Printf("NDP_BACKEND=%q 无法识别（可选 ndppd/userspace/kernel/auto），按 ndppd 处理", raw)
		log.Printf("NDP 后端：ndppd（整段 /64 由 ndppd 代答，内核 neigh 表不写任何条目）")
	}
}

// ensureAllMulticast 让网卡接收全部组播帧，这是整段用户态代答的前提。
//
// 默认开启（NDP_ALLMULTICAST=off 可关）。它是有代价的：接口上所有组播流量都会交给
// 协议栈，代答器自己会按前缀过滤；但相比「几万个 solicited-node 组播组 + MLDv2 周期重报」
// 的开销，整段代答这条路反而更省 CPU。
func ensureAllMulticast() {
	if netIf == "" {
		log.Printf("警告：未探测到网卡，无法开启 allmulticast；用户态代答收不到发往任意地址的 NS")
		return
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("NDP_ALLMULTICAST"))) {
	case "off", "false", "0", "no":
		log.Printf("警告：NDP_ALLMULTICAST=off，网卡只接收已加入的组播组；" +
			"未加入 solicited-node 组的地址，其首包 NS 到不了代答器（dial 会超时）")
		return
	}
	// allmulticast 是接口级开关（宿主共享 netns），进程退出不会自动还原；
	// 需要还原时在宿主机执行 `ip link set dev <网卡> allmulticast off`
	execCmd("ip", "link", "set", "dev", netIf, "allmulticast", "on")
	log.Printf("已开启 %s 的 allmulticast：整段 /64 的 NS 都会送到代答器（退出后可 `ip link set dev %s allmulticast off` 还原）",
		netIf, netIf)
}
