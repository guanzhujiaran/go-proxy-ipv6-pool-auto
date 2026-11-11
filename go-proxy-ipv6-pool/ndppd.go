package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ndppd 后端：由系统包 ndppd 对整段 /64 代答回程 NS。
//
// 分工：
//   - 本程序只负责「按当前池前缀生成 /etc/ndppd.conf」+「把 ndppd 拉起来并监护」；
//   - NS/NA 全部由 ndppd 处理，内核 neigh 表一条都不写（`ip -6 neigh show proxy` 恒为 0）；
//   - 出口地址因此可以是 /64 里的任意地址，不需要为每个地址执行一次 `ip` 命令。
//
// 配置里用 `static`：对该前缀内任意地址的 NS 直接回 NA，不做 per-address 状态跟踪，
// 这样连 ndppd 自己也不会因为地址数量增长而变慢。
const (
	// ndppdBin ndppd 可执行文件（Debian 包安装在 /usr/sbin/ndppd，容器内以 root 运行）
	ndppdBin = "ndppd"
	// ndppdDefaultConfPath ndppd 默认配置路径，可用 NDPPD_CONF 覆盖
	ndppdDefaultConfPath = "/etc/ndppd.conf"
	// ndppdSuperviseInterval 监护轮询间隔：进程挂了就重新拉起，池前缀变了就重写配置并重启
	ndppdSuperviseInterval = 2 * time.Second
)

var ndppdState struct {
	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan struct{}
	cidr string
}

// ndppdConfPath 当前使用的 ndppd 配置文件路径
func ndppdConfPath() string {
	if path := strings.TrimSpace(os.Getenv("NDPPD_CONF")); path != "" {
		return path
	}
	return ndppdDefaultConfPath
}

// ndppdConfText 生成整段 /64 的 ndppd 配置
func ndppdConfText(cidr string) string {
	return fmt.Sprintf(`# 由 go-proxy-ipv6-pool 自动生成，前缀变化时会被重写，请勿手工修改。
# 整段 %[1]s 由 ndppd 代答：出口地址不落到网卡，也不写内核 neigh 表。
route-ttl 30000

proxy %[2]s {
    router no
    timeout 500
    ttl 30000

    rule %[1]s {
        static
    }
}
`, cidr, netIf)
}

// writeNdppdConf 把配置写到 ndppdConfPath（目录不存在时先建出来）
func writeNdppdConf(cidr string) error {
	path := ndppdConfPath()
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("创建配置目录 %s 失败: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, []byte(ndppdConfText(cidr)), 0o644); err != nil {
		return fmt.Errorf("写 ndppd 配置 %s 失败: %w", path, err)
	}
	return nil
}

// startNdppd 拉起 ndppd（前台子进程，输出直接并到本进程日志）。
// ndppd 是守护进程式的程序，但默认不 daemon 化，由这里当子进程管理更好排障。
func startNdppd(cidr string) error {
	cmd := exec.Command(ndppdBin, "-c", ndppdConfPath())
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 %s 失败: %w", ndppdBin, err)
	}
	done := make(chan struct{})
	go func() {
		err := cmd.Wait()
		close(done)
		if err != nil {
			log.Printf("警告：ndppd 退出: %v", err)
			return
		}
		log.Printf("警告：ndppd 自行退出（exit 0）")
	}()

	ndppdState.mu.Lock()
	ndppdState.cmd = cmd
	ndppdState.done = done
	ndppdState.cidr = cidr
	ndppdState.mu.Unlock()
	return nil
}

// ndppdDone 取当前 ndppd 进程的退出信号通道；没有在跑的进程时返回 nil
func ndppdDone() <-chan struct{} {
	ndppdState.mu.Lock()
	defer ndppdState.mu.Unlock()
	return ndppdState.done
}

// stopNdppd 停掉当前 ndppd 子进程：先 SIGTERM，3 秒不退再 SIGKILL
func stopNdppd() {
	ndppdState.mu.Lock()
	cmd, done := ndppdState.cmd, ndppdState.done
	ndppdState.cmd, ndppdState.done = nil, nil
	ndppdState.mu.Unlock()

	if cmd == nil || cmd.Process == nil || done == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		log.Printf("警告：ndppd 3 秒未退出，强制结束")
		_ = cmd.Process.Kill()
		<-done
	}
}

// ndppdSupervise 等池前缀就绪后按当前前缀生成配置并拉起 ndppd，之后持续监护：
// 进程挂了重新拉起；光猫换前缀时重写配置并重启（ndppd 的 rule 是启动时读的）。
func ndppdSupervise() {
	// 宿主刚开机 / 光猫刚重启时 RA 还没下发，前缀和网卡都可能没就绪
	for pool.CIDR() == "" || netIf == "" {
		time.Sleep(time.Second)
	}

	// 先清掉宿主机上可能残留的 ndppd（旧配置、上一轮容器留下的），避免两套 rule 抢答
	if _, err := runCmd("pkill", "-x", "ndppd"); err == nil {
		log.Printf("已停止宿主机上遗留的 ndppd，改由本进程按当前前缀拉起")
		time.Sleep(500 * time.Millisecond)
	}

	for {
		cidr := pool.CIDR()
		if err := writeNdppdConf(cidr); err != nil {
			log.Printf("警告：%v（%s 后重试）", err, ndppdSuperviseInterval)
			time.Sleep(ndppdSuperviseInterval)
			continue
		}
		if err := startNdppd(cidr); err != nil {
			log.Printf("警告：%v（%s 后重试）", err, ndppdSuperviseInterval)
			time.Sleep(ndppdSuperviseInterval)
			continue
		}
		log.Printf("ndppd 已启动：配置 %s，代答段 %s，网卡 %s（内核 neigh 表不写任何条目）",
			ndppdConfPath(), cidr, netIf)

		done := ndppdDone()
		exited := false
		for !exited && pool.CIDR() == cidr {
			select {
			case <-done:
				exited = true
			case <-time.After(ndppdSuperviseInterval):
			}
		}
		if !exited {
			log.Printf("池前缀变化（%s -> %s），重启 ndppd 以更新代答段", cidr, pool.CIDR())
		}
		stopNdppd()
		// 稍等再拉起，避免 ndppd 本身有问题时形成快速重启风暴
		time.Sleep(time.Second)
	}
}
