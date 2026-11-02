package main

import (
	"net"
	"testing"
)

// 复现现场：光猫重新拨号后，网卡上同时存在「旧前缀（deprecated）」和「新前缀」两个地址。
// 旧版本按 `net.Interface.Addrs()` 的顺序取第一个，取到的就是旧前缀，
// 于是所有随机出口都落在已经作废的 /64 里 → 全网 i/o timeout。
const addrShowAfterPrefixChange = `2: eth0    inet6 2409:8a1e:2e94:7420:3629:30c4:6954:4efc/64 scope global deprecated dynamic \       valid_lft 1500sec preferred_lft 0sec
2: eth0    inet6 2409:8a1e:2e93:4fe0:5cbb:f6ff:fe9e:eefa/64 scope global dynamic mngtmpaddr noprefixroute \       valid_lft 2591968sec preferred_lft 604768sec
2: eth0    inet6 fe80::4bb:f6ff:fe9e:eefa/64 scope link \       valid_lft forever preferred_lft forever
`

const addrShowOnlyDeprecated = `2: eth0    inet6 2409:8a1e:2e94:7420:3629:30c4:6954:4efc/64 scope global deprecated dynamic \       valid_lft 1500sec preferred_lft 0sec
`

const addrShowStaleExitAddress = `2: eth0    inet6 2409:8a1e:2e93:4fe0:aaaa:bbbb:cccc:dddd/64 scope global nodad \       valid_lft forever preferred_lft forever
2: eth0    inet6 2409:8a1e:2e93:4fe0:5cbb:f6ff:fe9e:eefa/64 scope global dynamic mngtmpaddr noprefixroute \       valid_lft 2591968sec preferred_lft 604768sec
`

func TestParseIPv6Addrs(t *testing.T) {
	list := parseIPv6Addrs(addrShowAfterPrefixChange)
	if len(list) != 3 {
		t.Fatalf("应解析出 3 条地址，实际 %d", len(list))
	}
	if !list[0].deprecated || !list[0].dynamic {
		t.Errorf("第 1 条应为 deprecated+dynamic，实际 %+v", list[0])
	}
	if list[1].deprecated || list[1].temporary {
		t.Errorf("第 2 条不应被判为 deprecated/temporary，实际 %+v", list[1])
	}
	if list[1].iface != "eth0" {
		t.Errorf("网卡名应为 eth0，实际 %q", list[1].iface)
	}
	if list[1].prefixLen != 64 {
		t.Errorf("前缀长度应为 64，实际 %d", list[1].prefixLen)
	}
	if !list[1].ip.Equal(net.ParseIP("2409:8a1e:2e93:4fe0:5cbb:f6ff:fe9e:eefa")) {
		t.Errorf("解析出的地址不对：%s", list[1].ip)
	}
}

// TestSelectPoolAddressSkipsDeprecated 换前缀后必须选新前缀，不能继续用旧前缀。
func TestSelectPoolAddressSkipsDeprecated(t *testing.T) {
	info, ok := selectPoolAddress(parseIPv6Addrs(addrShowAfterPrefixChange), isPublicIPv6)
	if !ok {
		t.Fatal("应当选出一个可用地址")
	}
	if got, want := info.ip.String(), "2409:8a1e:2e93:4fe0:5cbb:f6ff:fe9e:eefa"; got != want {
		t.Fatalf("应选新前缀地址 %s，实际 %s", want, got)
	}
	cidr, err := maskCIDR(info.ip.String(), 64)
	if err != nil {
		t.Fatalf("掩码失败: %v", err)
	}
	if cidr != "2409:8a1e:2e93:4fe0::/64" {
		t.Fatalf("池前缀应为 2409:8a1e:2e93:4fe0::/64，实际 %s", cidr)
	}
}

// TestSelectPoolAddressRejectsOnlyDeprecated 只剩已经废弃的地址时必须报「没有可用地址」，
// 让 monitor 保留旧前缀并快速重试，而不是把池前缀切到一段已经不通的 /64 上。
func TestSelectPoolAddressRejectsOnlyDeprecated(t *testing.T) {
	if _, ok := selectPoolAddress(parseIPv6Addrs(addrShowOnlyDeprecated), isPublicIPv6); ok {
		t.Fatal("只有 deprecated 地址时不应选出地址")
	}
}

// TestSelectPoolAddressPrefersStable 稳定地址优先于 temporary 地址（两者前缀相同，但稳定地址更不容掉）。
func TestSelectPoolAddressPrefersStable(t *testing.T) {
	list := []ipv6AddrInfo{
		{ip: net.ParseIP("2409:8a1e:2e93:4fe0::1"), temporary: true},
		{ip: net.ParseIP("2409:8a1e:2e93:4fe0::2")},
	}
	info, ok := selectPoolAddress(list, isPublicIPv6)
	if !ok {
		t.Fatal("应当选出一个可用地址")
	}
	if info.temporary {
		t.Fatal("应优先选稳定地址")
	}
	// 只有临时地址时仍要能用（前缀一样）
	if _, ok := selectPoolAddress(list[:1], isPublicIPv6); !ok {
		t.Fatal("只有 temporary 地址时也应能选出地址")
	}
}

// TestMaskCIDR 未掩码的地址不能直接当池前缀 / 比对路由表。
func TestMaskCIDR(t *testing.T) {
	got, err := maskCIDR("2409:8a1e:2e93:4fe0:5cbb:f6ff:fe9e:eefa", 64)
	if err != nil {
		t.Fatalf("掩码失败: %v", err)
	}
	if got != "2409:8a1e:2e93:4fe0::/64" {
		t.Fatalf("掩码结果错误：%s", got)
	}
}

// TestParseStaleExitAddress 残留清理只认 nodad 且非 dynamic 的地址，SLAAC 真实地址必须保留。
func TestParseStaleExitAddress(t *testing.T) {
	list := parseIPv6Addrs(addrShowStaleExitAddress)
	if len(list) != 2 {
		t.Fatalf("应解析出 2 条地址，实际 %d", len(list))
	}
	if !list[0].nodad || list[0].dynamic {
		t.Errorf("第 1 条应判为旧版残留地址（nodad 且非 dynamic），实际 %+v", list[0])
	}
	if list[1].nodad || !list[1].dynamic {
		t.Errorf("第 2 条是 SLAAC 真实地址，不应被判为残留，实际 %+v", list[1])
	}
}
