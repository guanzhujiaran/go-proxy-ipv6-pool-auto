package main

import (
	"net"
	"strings"
	"testing"
)

func TestIsVirtualIface(t *testing.T) {
	virtual := []string{"lo", "docker0", "br-639a8d09b923", "veth1fac595", "virbr0", "tun0", "tailscale0", "wg0"}
	for _, name := range virtual {
		if !isVirtualIface(name) {
			t.Errorf("%s 应判为虚拟网卡", name)
		}
	}
	physical := []string{"eth0", "enp2s0", "ens18", "eno1", "wlan0"}
	for _, name := range physical {
		if isVirtualIface(name) {
			t.Errorf("%s 不应判为虚拟网卡", name)
		}
	}
}

func TestDefaultRouteIface(t *testing.T) {
	name := defaultRouteIface(6)
	if name == "" {
		t.Skip("本机没有 IPv6 默认路由（宿主可能未下发 IPv6），跳过")
	}
	if isVirtualIface(name) {
		t.Errorf("默认 IPv6 路由不应指向虚拟网卡：%s", name)
	}
	if _, err := net.InterfaceByName(name); err != nil {
		t.Errorf("默认 IPv6 路由的网卡 %s 在本机不存在: %v", name, err)
	}
}

// TestResolveNetIfIgnoresUnknownName NET_IF 填了本机不存在的网卡名时必须回落自动探测，
// 不能把不存在的名字继续往 NDP 代答 / ip addr 里塞（每台机器网卡名不同，换机器时最容易踩）。
func TestResolveNetIfIgnoresUnknownName(t *testing.T) {
	t.Setenv("NET_IF", "bogus-iface-xyz")
	name := resolveNetIf()
	if name == "bogus-iface-xyz" {
		t.Fatalf("不存在于本机的 NET_IF 不应被采用，实际返回 %s", name)
	}
	if name == "" {
		t.Skip("自动探测也失败（纯内网环境），跳过")
	}
	if _, err := net.InterfaceByName(name); err != nil {
		t.Fatalf("回落后的网卡 %s 仍不存在: %v", name, err)
	}
}

// TestDetectNetIf 在本机实跑探测：结果必须是真实存在、非虚拟的网卡，
// 且在宿主已有公网 IPv6 时该网卡上确实带公网 IPv6。
func TestDetectNetIf(t *testing.T) {
	name := detectNetIf()
	if name == "" {
		t.Skip("本机既无默认路由也无公网 IPv6（如纯内网 CI 环境），跳过")
	}
	if _, err := net.InterfaceByName(name); err != nil {
		t.Fatalf("探测出的网卡 %s 在本机不存在: %v", name, err)
	}
	if isVirtualIface(name) {
		t.Fatalf("探测出的网卡 %s 是虚拟网卡", name)
	}
	if ip, ok := findIPv6OnInterfaceName(name, isPublicIPv6); ok {
		if !strings.Contains(ip, ":") {
			t.Fatalf("探测出的地址 %s 不是 IPv6", ip)
		}
	}
}
