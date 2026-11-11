package main

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

const testPoolCIDR = "2409:8a1e:2e93:4fe0::/64"

// resetProxyNdpTestState 隔离每个用例的全局状态：
// 出口地址池、账本、网卡名、命令执行入口、时间源都会被替换，用 t.Cleanup 还原。
func resetProxyNdpTestState(t *testing.T) {
	t.Helper()

	prevNetIf, prevLimit, prevNow := netIf, exitPoolLimit, proxyNdpNow
	prevRenew, prevBackend := exitPoolRenew, ndpBackend
	prevExec, prevExecInput := proxyNdpExec, proxyNdpExecInput
	prevEnabled := proxyNdpEnabled()

	t.Cleanup(func() {
		netIf, exitPoolLimit, proxyNdpNow = prevNetIf, prevLimit, prevNow
		exitPoolRenew, ndpBackend = prevRenew, prevBackend
		proxyNdpExec, proxyNdpExecInput = prevExec, prevExecInput
		proxyNdpSetEnabled(prevEnabled)
	})

	// 单测里不真的去动内核 neigh 表：网卡名给个假值，命令入口由各用例自己替换
	netIf = "eth0"
	ndpBackend = ndpBackendKernel
	proxyNdpSetEnabled(true)
	proxyNdp.mu.Lock()
	proxyNdp.entries = make(map[string]time.Time)
	proxyNdp.deleteFailures = 0
	proxyNdp.warned = false
	proxyNdp.mu.Unlock()

	exitPool.mu.Lock()
	exitPool.cidr = ""
	exitPool.ips = nil
	exitPool.seen = nil
	exitPool.mu.Unlock()
	exitPoolLimit = 0
	exitPoolRenew = 0
	proxyNdpNow = time.Now
}

// countExec 统计某类命令被执行的次数，并按需返回固定错误
type execRecorder struct {
	calls    int
	inputs   []string
	failWith error
}

// installProxyNdpExec 把单条 ip 命令入口换成记录器
func installProxyNdpExec(t *testing.T, rec *execRecorder) {
	t.Helper()
	proxyNdpExec = func(timeout time.Duration, name string, args ...string) (string, error) {
		rec.calls++
		rec.inputs = append(rec.inputs, name+" "+strings.Join(args, " "))
		return "", rec.failWith
	}
}

// installProxyNdpExecInput 把 `ip -batch` 入口换成记录器
func installProxyNdpExecInput(t *testing.T, rec *execRecorder) {
	t.Helper()
	proxyNdpExecInput = func(input string, timeout time.Duration, name string, args ...string) (string, error) {
		rec.calls++
		rec.inputs = append(rec.inputs, input)
		return "", rec.failWith
	}
}

// TestPickExitIPPoolCap 池上限：EXIT_POOL_SIZE 决定活跃出口地址数，
// 池满后只能复用池内地址，地址总数不再随请求数增长。
func TestPickExitIPPoolCap(t *testing.T) {
	resetProxyNdpTestState(t)
	exitPoolLimit = 4

	_, ipnet, err := net.ParseCIDR(testPoolCIDR)
	if err != nil {
		t.Fatalf("解析测试前缀失败: %v", err)
	}

	distinct := make(map[string]int)
	for i := 0; i < 200; i++ {
		ip, err := pickExitIP(testPoolCIDR)
		if err != nil {
			t.Fatalf("第 %d 次取出口地址失败: %v", i+1, err)
		}
		parsed := net.ParseIP(ip)
		if parsed == nil || !ipnet.Contains(parsed) {
			t.Fatalf("出口地址 %s 不在池前缀 %s 内", ip, testPoolCIDR)
		}
		distinct[ip]++
	}

	if len(distinct) != 4 {
		t.Fatalf("池上限为 4 时最多出现 4 个不同出口地址，实际 %d 个", len(distinct))
	}
	exitPool.mu.Lock()
	pooled := len(exitPool.ips)
	exitPool.mu.Unlock()
	if pooled != 4 {
		t.Fatalf("池内地址数应停在 4，实际 %d", pooled)
	}
}

// TestPickExitIPWithoutPool EXIT_POOL_SIZE=0 时退回「每请求全新随机地址」的旧行为
func TestPickExitIPWithoutPool(t *testing.T) {
	resetProxyNdpTestState(t)
	exitPoolLimit = 0

	first, err := pickExitIP(testPoolCIDR)
	if err != nil {
		t.Fatalf("取出口地址失败: %v", err)
	}
	second, err := pickExitIP(testPoolCIDR)
	if err != nil {
		t.Fatalf("取出口地址失败: %v", err)
	}
	if first == second {
		t.Fatalf("EXIT_POOL_SIZE=0 时每次都应生成全新地址，两次都拿到 %s", first)
	}
	exitPool.mu.Lock()
	defer exitPool.mu.Unlock()
	if len(exitPool.ips) != 0 {
		t.Fatalf("不走池时不应记录池内地址，实际 %d 个", len(exitPool.ips))
	}
}

// TestExitPoolRotatesAcrossSubnet 池内地址超过轮换周期就换新：同时活跃地址数受 EXIT_POOL_SIZE 限制，
// 但出口地址空间持续覆盖整个 /64，不会长期固定在最初那一小批地址上。
func TestExitPoolRotatesAcrossSubnet(t *testing.T) {
	resetProxyNdpTestState(t)
	exitPoolLimit = 3
	exitPoolRenew = 10 * time.Minute

	now := time.Unix(1700000000, 0)
	proxyNdpNow = func() time.Time { return now }

	_, ipnet, err := net.ParseCIDR(testPoolCIDR)
	if err != nil {
		t.Fatalf("解析测试前缀失败: %v", err)
	}

	firstRound := make(map[string]bool, 3)
	for i := 0; i < 3; i++ {
		ip, err := pickExitIP(testPoolCIDR)
		if err != nil {
			t.Fatalf("建池失败: %v", err)
		}
		firstRound[ip] = true
	}

	// 轮换周期内必须复用池内地址，不能继续扩建
	if ip, err := pickExitIP(testPoolCIDR); err != nil || !firstRound[ip] {
		t.Fatalf("轮换周期内应复用池内地址，实际拿到 %s（err=%v）", ip, err)
	}

	now = now.Add(exitPoolRenew + time.Second)
	ip, err := pickExitIP(testPoolCIDR)
	if err != nil {
		t.Fatalf("轮换后取地址失败: %v", err)
	}
	if firstRound[ip] {
		t.Fatalf("超过轮换周期后不应再返回旧池地址 %s", ip)
	}
	if parsed := net.ParseIP(ip); parsed == nil || !ipnet.Contains(parsed) {
		t.Fatalf("轮换出的新地址 %s 不在 /64 内", ip)
	}

	pool := snapshotExitPool()
	if len(pool) != 1 {
		t.Fatalf("轮换后池里应只剩刚补的地址，实际 %d 个", len(pool))
	}
	// 轮换出去的地址必须失去池保护，否则它的 pneigh 永远不会被回收
	for old := range firstRound {
		if exitPoolHas(old) {
			t.Fatalf("轮换出去的地址 %s 仍在池内受保护，pneigh 将永久残留", old)
		}
	}
}

// snapshotExitPool 取池内地址快照（测试用）
func snapshotExitPool() []string {
	exitPool.mu.Lock()
	defer exitPool.mu.Unlock()
	ips := make([]string, 0, len(exitPool.ips))
	for _, item := range exitPool.ips {
		ips = append(ips, item.ip)
	}
	return ips
}

// TestProxyNeighborAddUserspaceSkipsKernel userspace 后端（整段代答）不允许为单个地址写内核条目：
// 一个 /64 不可能一条条 add，那正是把 ip 命令卡死、pneigh 堆到 38 万条的根源。
func TestProxyNeighborAddUserspaceSkipsKernel(t *testing.T) {
	resetProxyNdpTestState(t)
	ndpBackend = ndpBackendUserspace
	exitPoolLimit = 0
	rec := &execRecorder{}
	installProxyNdpExec(t, rec)

	if !proxyNeighborAdd("2409:8a1e:2e93:4fe0::abcd") {
		t.Fatal("userspace 后端下添加出口地址应直接成功（由代答器覆盖整段）")
	}
	pruneProxyNeighbors(time.Unix(1700000000, 0))
	if err := proxyNeighborDel("2409:8a1e:2e93:4fe0::abcd"); err != nil {
		t.Fatalf("userspace 后端下删除不应报错: %v", err)
	}
	if rec.calls != 0 {
		t.Fatalf("userspace 后端不应执行任何内核 neigh 命令，实际 %d 次: %v", rec.calls, rec.inputs)
	}
}

// TestInitExitPoolDefaultsByBackend 池大小默认值随后端变化：
// userspace 不产生内核条目 → 默认每请求 /64 内全新地址；kernel → 默认 512 的池。
func TestInitExitPoolDefaultsByBackend(t *testing.T) {
	resetProxyNdpTestState(t)
	t.Setenv("EXIT_POOL_SIZE", "")
	t.Setenv("EXIT_POOL_RENEW_SECONDS", "")

	ndpBackend = ndpBackendUserspace
	initExitPool()
	if exitPoolLimit != 0 {
		t.Fatalf("userspace 后端默认应每请求新地址（池=0），实际 %d", exitPoolLimit)
	}

	ndpBackend = ndpBackendKernel
	initExitPool()
	if exitPoolLimit != defaultExitPoolSize {
		t.Fatalf("kernel 后端默认池应为 %d，实际 %d", defaultExitPoolSize, exitPoolLimit)
	}
}

// TestProxyNeighborAddSkipsWithinRefreshWindow 刷新窗口内重复命中同一出口地址不再 exec `ip`：
// 池化后同一地址每秒被命中几十次，每次都 exec 会把 CPU 打满。
func TestProxyNeighborAddSkipsWithinRefreshWindow(t *testing.T) {
	resetProxyNdpTestState(t)

	now := time.Unix(1700000000, 0)
	proxyNdpNow = func() time.Time { return now }
	rec := &execRecorder{}
	installProxyNdpExec(t, rec)

	ip := "2409:8a1e:2e93:4fe0::1234"
	if !proxyNeighborAdd(ip) {
		t.Fatal("添加代理邻居条目应成功")
	}
	if !proxyNeighborAdd(ip) {
		t.Fatal("刷新窗口内再次添加应直接返回成功")
	}
	if rec.calls != 1 {
		t.Fatalf("刷新窗口内不应重复 exec ip，实际执行 %d 次", rec.calls)
	}

	now = now.Add(proxyNeighborRefresh + time.Second)
	if !proxyNeighborAdd(ip) {
		t.Fatal("过了刷新窗口再次添加应成功")
	}
	if rec.calls != 2 {
		t.Fatalf("过了刷新窗口应重新 exec ip，实际执行 %d 次", rec.calls)
	}
}

// TestPruneKeepsLedgerWhenDeleteFails 删除失败时账本必须保留（账实一致），
// 连续失败到阈值再整表 flush 熔断。
func TestPruneKeepsLedgerWhenDeleteFails(t *testing.T) {
	resetProxyNdpTestState(t)

	now := time.Unix(1700000000, 0)
	proxyNdpNow = func() time.Time { return now }
	ip := "2409:8a1e:2e93:4fe0::aaaa"
	proxyNdp.mu.Lock()
	proxyNdp.entries[ip] = now.Add(-proxyNeighborTTL - time.Minute)
	proxyNdp.mu.Unlock()

	batch := &execRecorder{failWith: errors.New("signal: killed")}
	installProxyNdpExecInput(t, batch)
	flushes := 0
	proxyNdpExec = func(timeout time.Duration, name string, args ...string) (string, error) {
		if len(args) >= 3 && args[2] == "flush" {
			flushes++
			return "", nil
		}
		t.Fatalf("未预期的单条 ip 命令: %s %v", name, args)
		return "", nil
	}

	for round := 1; round <= proxyNeighborFailThreshold; round++ {
		pruneProxyNeighbors(now)
		if round < proxyNeighborFailThreshold {
			proxyNdp.mu.Lock()
			_, kept := proxyNdp.entries[ip]
			proxyNdp.mu.Unlock()
			if !kept {
				t.Fatalf("第 %d 轮删除失败后账本被销账：内核条目会永久残留（这正是 pneigh 堆到 38 万条的原因）", round)
			}
		}
	}

	if batch.calls != proxyNeighborFailThreshold {
		t.Fatalf("应重试 %d 轮批量删除，实际 %d 轮", proxyNeighborFailThreshold, batch.calls)
	}
	if flushes != 1 {
		t.Fatalf("连续 %d 轮删除失败应触发且只触发一次整表 flush 熔断，实际 %d 次", proxyNeighborFailThreshold, flushes)
	}
	proxyNdp.mu.Lock()
	remaining := len(proxyNdp.entries)
	failures := proxyNdp.deleteFailures
	proxyNdp.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("熔断 flush 成功后账本应清空，实际剩 %d 条", remaining)
	}
	if failures != 0 {
		t.Fatalf("熔断后连续失败计数应清零，实际 %d", failures)
	}
}

// TestPruneRemovesLedgerAfterConfirmedDelete 只有 ip -batch 确认删除成功才销账，
// 且未过期的条目不能被误删。
func TestPruneRemovesLedgerAfterConfirmedDelete(t *testing.T) {
	resetProxyNdpTestState(t)

	now := time.Unix(1700000000, 0)
	proxyNdpNow = func() time.Time { return now }
	expiredA := "2409:8a1e:2e93:4fe0::1111"
	expiredB := "2409:8a1e:2e93:4fe0::2222"
	fresh := "2409:8a1e:2e93:4fe0::3333"
	proxyNdp.mu.Lock()
	proxyNdp.entries[expiredA] = now.Add(-proxyNeighborTTL - time.Minute)
	proxyNdp.entries[expiredB] = now.Add(-proxyNeighborTTL - time.Minute)
	proxyNdp.entries[fresh] = now.Add(-time.Minute)
	proxyNdp.mu.Unlock()

	batch := &execRecorder{}
	installProxyNdpExecInput(t, batch)

	pruneProxyNeighbors(now)

	if batch.calls != 1 {
		t.Fatalf("应执行一次 ip -batch 批量删除，实际 %d 次", batch.calls)
	}
	for _, ip := range []string{expiredA, expiredB} {
		if !strings.Contains(batch.inputs[0], "neigh del proxy "+ip) {
			t.Fatalf("删除批次里缺少过期条目 %s", ip)
		}
	}
	if strings.Contains(batch.inputs[0], fresh) {
		t.Fatalf("未过期条目 %s 不应进入删除批次", fresh)
	}
	proxyNdp.mu.Lock()
	defer proxyNdp.mu.Unlock()
	if len(proxyNdp.entries) != 1 {
		t.Fatalf("确认删除后账本应只剩 1 条未过期条目，实际 %d 条", len(proxyNdp.entries))
	}
	if _, ok := proxyNdp.entries[fresh]; !ok {
		t.Fatalf("未过期条目 %s 被误删", fresh)
	}
}

// TestPruneSkipsPoolAddresses 池内地址不参与 TTL 回收：它会被下一个请求复用，
// 删掉之后内核表里要重建，而 CPE 邻居缓存已过期 → 首包必丢。
func TestPruneSkipsPoolAddresses(t *testing.T) {
	resetProxyNdpTestState(t)

	now := time.Unix(1700000000, 0)
	proxyNdpNow = func() time.Time { return now }
	exitPoolLimit = 2

	poolIP, err := pickExitIP(testPoolCIDR)
	if err != nil {
		t.Fatalf("取出口地址失败: %v", err)
	}
	nonPoolIP := "2409:8a1e:2e93:4fe0::dddd"
	proxyNdp.mu.Lock()
	// 都已远超 TTL：区别只在于一个在池内、一个不在池内
	proxyNdp.entries[poolIP] = now.Add(-2 * proxyNeighborTTL)
	proxyNdp.entries[nonPoolIP] = now.Add(-2 * proxyNeighborTTL)
	proxyNdp.mu.Unlock()

	batch := &execRecorder{}
	installProxyNdpExecInput(t, batch)

	pruneProxyNeighbors(now)

	proxyNdp.mu.Lock()
	_, poolKept := proxyNdp.entries[poolIP]
	_, nonPoolKept := proxyNdp.entries[nonPoolIP]
	proxyNdp.mu.Unlock()
	if !poolKept {
		t.Fatalf("池内地址 %s 不应被 TTL 回收", poolIP)
	}
	if nonPoolKept {
		t.Fatalf("非池地址 %s 过期后应被回收", nonPoolIP)
	}
	if len(batch.inputs) != 1 || strings.Contains(batch.inputs[0], poolIP) {
		t.Fatalf("池内地址不应出现在删除批次里：%v", batch.inputs)
	}
}

// TestResetProxyNeighborsSkipsLargeTable 启动残留超过阈值时不允许多次/同步 flush：
// 大表上 flush 近似 O(n²)，同步做会把启动卡死几分钟且几乎删不动，只能告警交给维护窗口。
func TestResetProxyNeighborsSkipsLargeTable(t *testing.T) {
	resetProxyNdpTestState(t)

	huge := proxyNeighborFlushSkipThreshold + 1
	lines := make([]string, 0, huge)
	for i := 0; i < huge; i++ {
		lines = append(lines, fmt.Sprintf("2409:8a1e:2e93:4fe0::%x lladdr 00:00:00:00:00:01 PERMANENT", i))
	}
	flushed := 0
	proxyNdpExec = func(timeout time.Duration, name string, args ...string) (string, error) {
		if len(args) >= 3 && args[2] == "flush" {
			flushed++
			return "", nil
		}
		return strings.Join(lines, "\n"), nil
	}

	resetProxyNeighbors()

	if flushed != 0 {
		t.Fatalf("残留 %d 条（超过阈值 %d）时不应执行 flush，实际执行 %d 次",
			huge, proxyNeighborFlushSkipThreshold, flushed)
	}
	proxyNdp.mu.Lock()
	adopted := len(proxyNdp.entries)
	proxyNdp.mu.Unlock()
	if adopted != 0 {
		t.Fatalf("大表既不应 flush 也不应接管进账本，实际账本 %d 条", adopted)
	}
}

// TestResetProxyNeighborsFlushesSmallTable 小表（<= 阈值）直接整表 flush，
// flush 成功则账本保持为空（条目在内核侧已经没了）。
func TestResetProxyNeighborsFlushesSmallTable(t *testing.T) {
	resetProxyNdpTestState(t)

	showOut := "2409:8a1e:2e93:4fe0::1 lladdr 00:00:00:00:00:01 PERMANENT\n" +
		"2409:8a1e:2e93:4fe0::2 lladdr 00:00:00:00:00:02 PERMANENT"
	flushed := 0
	proxyNdpExec = func(timeout time.Duration, name string, args ...string) (string, error) {
		if len(args) >= 3 && args[2] == "flush" {
			flushed++
			return "", nil
		}
		return showOut, nil
	}

	resetProxyNeighbors()

	if flushed != 1 {
		t.Fatalf("小表应执行一次整表 flush，实际 %d 次", flushed)
	}
	proxyNdp.mu.Lock()
	defer proxyNdp.mu.Unlock()
	if len(proxyNdp.entries) != 0 {
		t.Fatalf("flush 成功后账本应为空，实际 %d 条", len(proxyNdp.entries))
	}
}

// TestResetProxyNeighborsAdoptsWhenFlushFails flush 失败时降级接管，交给 TTL 分批回收
func TestResetProxyNeighborsAdoptsWhenFlushFails(t *testing.T) {
	resetProxyNdpTestState(t)

	showOut := "2409:8a1e:2e93:4fe0::1 lladdr 00:00:00:00:00:01 PERMANENT"
	proxyNdpExec = func(timeout time.Duration, name string, args ...string) (string, error) {
		if len(args) >= 3 && args[2] == "flush" {
			return "", errors.New("signal: killed")
		}
		return showOut, nil
	}

	resetProxyNeighbors()

	proxyNdp.mu.Lock()
	defer proxyNdp.mu.Unlock()
	if _, ok := proxyNdp.entries["2409:8a1e:2e93:4fe0::1"]; !ok {
		t.Fatal("flush 失败时应接管残留条目，交给 TTL 回收，否则条目永久残留")
	}
}

// TestDeleteProxyNeighborsTreatsMissingAsDeleted ip -force -batch 在「条目已经不在了」时
// 仍返回非 0；这种失败内核里确实没有该条目，账本必须照常销账，
// 否则一次超时 kill 产生的幽灵条目会让之后每轮删除都失败。
func TestDeleteProxyNeighborsTreatsMissingAsDeleted(t *testing.T) {
	resetProxyNdpTestState(t)

	ips := []string{"2409:8a1e:2e93:4fe0::9999"}
	proxyNdpExecInput = func(input string, timeout time.Duration, name string, args ...string) (string, error) {
		return "Command failed /dev/stdin:1\nRTNETLINK answers: No such file or directory\n", errors.New("exit status 2")
	}
	if err := deleteProxyNeighbors(ips); err != nil {
		t.Fatalf("全部是「条目已不存在」的失败应按成功处理，实际返回 %v", err)
	}

	proxyNdpExecInput = func(input string, timeout time.Duration, name string, args ...string) (string, error) {
		return "", errors.New("signal: killed")
	}
	if err := deleteProxyNeighbors(ips); err == nil {
		t.Fatal("命令被超时 kill 必须返回 error，账本才能保留下来重试")
	}

	proxyNdpExecInput = func(input string, timeout time.Duration, name string, args ...string) (string, error) {
		return "Command failed /dev/stdin:1\noperation not permitted\n", errors.New("exit status 2")
	}
	if err := deleteProxyNeighbors(ips); err == nil {
		t.Fatal("权限类失败必须返回 error，账本不能销账")
	}
}
