#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""随机 IPv6 出口抽样分析。

批量生成某个 /64 前缀内的随机源地址，向同一个目标发起 ICMPv6 探测，
把「成功地址」和「失败地址」放在一起对比，找出规律并推断失败原因。

判定逻辑（配合 --capture 抓包）：
  有 echo reply                                -> 出口可用
  有 echo request、无 reply、也无该地址的 NS    -> 包在 CPE/上游被丢弃（源地址不被放行）
  有 echo request、有该地址的 NS、无 NA        -> 邻居解析没人应答（NDP 代答缺失）
  有 NS、有 NA、无 reply                       -> CPE 收到了 NA 但没把回程转发回来
  连 echo request 都没有                       -> 本机就没发出去（bind/路由问题）

用法示例：
  ./analyze_exit_ipv6.py                      # 自动取网卡前缀，20 个样本
  ./analyze_exit_ipv6.py -n 40 -c 3           # 40 个样本，每个 3 包
  ./analyze_exit_ipv6.py -t 2001:4860:4860::8888
  ./analyze_exit_ipv6.py --prefix 2409:8a1e:2e94:84e0::/64 -o result.csv
  ./analyze_exit_ipv6.py --no-capture         # 不抓包，只看 ping 结果
"""

from __future__ import annotations

import argparse
import csv
import ipaddress
import os
import re
import secrets
import shutil
import socket
import subprocess
import sys
import time
from collections.abc import Callable
from dataclasses import dataclass, field

CAP_CONTAINER = "ipv6-exit-probe-cap"
PROBE_CONTAINER = "ipv6-exit-probe-shell"
CAP_IMAGE = "golang:1.27.3-alpine"  # 本地已有；容器内临时装 tcpdump/iproute2，不需要宿主机 root
ALPINE_MIRROR = "mirrors.aliyun.com"

RE_IPV6 = re.compile(r"inet6\s+([0-9a-fA-F:]+)/(\d+)")
RE_LOSS = re.compile(r"(\d+(?:\.\d+)?)% packet loss")
RE_RTT = re.compile(r"rtt min/avg/max/mdev = ([\d.]+)/([\d.]+)/([\d.]+)/([\d.]+)")


@dataclass
class Probe:
    """单个出口地址的探测结果。"""

    address: str
    sent: int = 0
    received: int = 0
    loss_pct: float = 100.0
    rtt_avg: float | None = None
    echo_request: int = 0
    echo_reply: int = 0
    ns_for_addr: int = 0
    na_for_addr: int = 0
    error: str = ""

    @property
    def ok(self) -> bool:
        return self.received > 0

    def verdict(self) -> str:
        """按抓包计数给出失败环节。"""
        if self.ok:
            return "成功"
        if self.error:
            return f"执行失败: {self.error}"
        if self.echo_request == 0:
            return "本机未发出（bind/路由）"
        if self.ns_for_addr == 0:
            return "CPE/上游丢弃（无邻居解析）"
        if self.na_for_addr == 0:
            return "无 NDP 应答（NS 有、NA 无）"
        return "CPE 收到 NA 仍未转发回程"


@dataclass
class Report:
    """一次抽样分析的汇总。"""

    iface: str
    prefix: str
    target: str
    real_address: str
    probes: list[Probe] = field(default_factory=list)
    multicast_groups: set[str] = field(default_factory=set)
    baseline: Probe | None = None


def run(cmd: list[str], timeout: float = 30) -> subprocess.CompletedProcess[str]:
    return subprocess.run(cmd, capture_output=True, text=True, timeout=timeout, check=False)


def check_nonlocal_bind() -> bool:
    try:
        with open("/proc/sys/net/ipv6/ip_nonlocal_bind", encoding="utf-8") as fh:
            return fh.read().strip() == "1"
    except OSError:
        return False


def detect_iface() -> str:
    """按默认 IPv6 路由推断出口网卡（每台机器网卡名不同，不能假设 eth0）。"""
    out = run(["ip", "-6", "route", "show", "default"]).stdout
    for line in out.splitlines():
        parts = line.split()
        if "dev" in parts:
            idx = parts.index("dev")
            if idx + 1 < len(parts):
                return parts[idx + 1]
    return ""


def detect_prefix(iface: str) -> tuple[str, ipaddress.IPv6Network]:
    """取网卡上的公网 IPv6，推导池前缀（与 go 程序逻辑一致：排除 fe80 与 ULA）。"""
    out = run(["ip", "-6", "addr", "show", "dev", iface]).stdout
    candidates: list[ipaddress.IPv6Address] = []
    for addr_str, _plen in RE_IPV6.findall(out):
        ip = ipaddress.IPv6Address(addr_str)
        if ip.is_link_local or ip.is_private or ip.is_loopback:
            continue
        candidates.append(ip)
    if not candidates:
        raise SystemExit(f"网卡 {iface} 上找不到公网 IPv6 地址，请用 --prefix 手动指定")
    real = candidates[0]
    return str(real), ipaddress.IPv6Network(f"{real}/64", strict=False)


def multicast_groups(iface: str) -> set[str]:
    """网卡当前加入的 IPv6 组播组（neighbor solicitation 只有收到对应 solicited-node 组才会被处理）。"""
    out = run(["ip", "-6", "maddr", "show", "dev", iface]).stdout
    groups: set[str] = set()
    for line in out.splitlines():
        line = line.strip()
        if not line.startswith("inet6 "):
            continue
        groups.add(line.split()[1])
    return groups


def solicit_node(ip: ipaddress.IPv6Address) -> str:
    raw = ip.packed
    return str(ipaddress.IPv6Address(b"\xff\x02" + b"\x00" * 9 + b"\x01\xff" + raw[-3:]))


def random_address(net: ipaddress.IPv6Network) -> ipaddress.IPv6Address:
    host_bits = net.max_prefixlen - net.prefixlen
    base = int(net.network_address)
    return ipaddress.IPv6Address(base | secrets.randbits(host_bits))


def ping_probe(
    addr: str,
    target: str,
    count: int,
    wait: int,
    iface: str,
    interval: float,
    wrap: Callable[[list[str]], list[str]] | None = None,
) -> Probe:
    probe = Probe(address=addr, sent=count)
    cmd = ["ping", "-6", "-n", "-c", str(count), "-W", str(wait), "-i", str(interval), "-I", addr, target]
    if wrap is not None:
        cmd = wrap(cmd)
    try:
        out = run(cmd, timeout=count * (wait + interval) + 10).stdout
    except subprocess.TimeoutExpired:
        probe.error = "ping 超时"
        return probe

    match = RE_LOSS.search(out)
    if match:
        probe.loss_pct = float(match.group(1))
        probe.received = round(count * (100 - probe.loss_pct) / 100)
    rtt = RE_RTT.search(out)
    if rtt:
        probe.rtt_avg = float(rtt.group(2))
    if "Cannot assign requested address" in out:
        probe.error = "bind 失败（检查 net.ipv6.ip_nonlocal_bind）"
    return probe


def probe_env_start() -> str | None:
    """起一个常驻容器（host 网络 + NET_ADMIN），用于非 root 环境下挂地址和 ping。"""
    if shutil.which("docker") is None:
        return None
    run(["docker", "rm", "-f", PROBE_CONTAINER])
    script = (
        f"sed -i 's|dl-cdn.alpinelinux.org|{ALPINE_MIRROR}|' /etc/apk/repositories; "
        "apk add --no-cache iproute2 iputils >/dev/null 2>&1; exec sleep infinity"
    )
    res = run(
        [
            "docker",
            "run",
            "-d",
            "--name",
            PROBE_CONTAINER,
            "--net=host",
            "--cap-add=NET_ADMIN",
            "--cap-add=NET_RAW",
            CAP_IMAGE,
            "sh",
            "-c",
            script,
        ],
        timeout=180,
    )
    if res.returncode != 0:
        return None
    for _ in range(90):
        time.sleep(1)
        ready = run(["docker", "exec", PROBE_CONTAINER, "sh", "-c", "command -v ip && command -v ping"])
        if ready.returncode == 0:
            return PROBE_CONTAINER
    run(["docker", "rm", "-f", PROBE_CONTAINER])
    return None


@dataclass
class PingRunner:
    """按「是否先把地址挂到网卡」执行探测，可选通过容器执行（免 root）。"""

    iface: str
    prefixlen: int
    add_addr: bool
    container: str | None = None

    def _wrap(self, cmd: list[str]) -> list[str]:
        if self.container:
            return ["docker", "exec", self.container, *cmd]
        return cmd

    def _addr_cmd(self, action: str, addr: str) -> list[str]:
        cmd = ["ip", "-6", "addr", action, f"{addr}/{self.prefixlen}", "dev", self.iface]
        if action == "add":
            cmd.append("nodad")
        return cmd

    def probe(self, addr: str, target: str, count: int, wait: int, interval: float) -> Probe:
        if not self.add_addr:
            return ping_probe(addr, target, count, wait, self.iface, interval, self._wrap)

        added = run(self._wrap(self._addr_cmd("add", addr))).returncode == 0
        try:
            return ping_probe(addr, target, count, wait, self.iface, interval, self._wrap)
        finally:
            if added:
                run(self._wrap(self._addr_cmd("del", addr)))


# ---------------------------------------------------------------- 抓包（可选）


def capture_start(iface: str) -> bool:
    """起一个临时容器抓 icmp6：容器共享 host 网络，不需要宿主机 root。

    tcpdump 直接输出文本（-U 行缓冲），结束用 docker stop 让 PID1 收到 SIGTERM 正常退出，
    再去 docker logs 里取全部报文，避免再准备一次镜像来解 pcap。
    """
    if shutil.which("docker") is None:
        return False
    run(["docker", "rm", "-f", CAP_CONTAINER])
    script = (
        f"sed -i 's|dl-cdn.alpinelinux.org|{ALPINE_MIRROR}|' /etc/apk/repositories; "
        "apk add --no-cache tcpdump >/dev/null 2>&1; "
        f"exec tcpdump -Uni {iface} 'icmp6'"
    )
    res = run(
        [
            "docker",
            "run",
            "-d",
            "--name",
            CAP_CONTAINER,
            "--net=host",
            "--cap-add=NET_RAW",
            "--cap-add=NET_ADMIN",
            CAP_IMAGE,
            "sh",
            "-c",
            script,
        ],
        timeout=180,
    )
    if res.returncode != 0:
        print(f"[警告] 抓包容器启动失败：{res.stderr.strip()[:200]}", file=sys.stderr)
        return False

    # 等 tcpdump 真正开始监听（镜像内还要 apk add，首次可能十几秒）
    for _ in range(90):
        time.sleep(1)
        if "listening on" in capture_logs():
            return True
    print("[警告] 抓包容器 90s 内未就绪，跳过抓包分析", file=sys.stderr)
    run(["docker", "rm", "-f", CAP_CONTAINER])
    return False


def capture_logs() -> str:
    res = run(["docker", "logs", CAP_CONTAINER], timeout=60)
    return (res.stdout or "") + (res.stderr or "")


def capture_stop() -> str:
    """停止抓包（SIGTERM 让 tcpdump 正常退出）并返回报文文本。"""
    run(["docker", "stop", "-t", "10", CAP_CONTAINER], timeout=30)
    text = capture_logs()
    run(["docker", "rm", "-f", CAP_CONTAINER])
    return text


RE_ICMP6 = re.compile(r"IP6 (\S+) > (\S+): ICMP6, (.+)")
RE_NS = re.compile(r"neighbor solicitation, who has ([0-9a-fA-F:]+)")
RE_NA = re.compile(r"neighbor advertisement, tgt is ([0-9a-fA-F:]+)")


def apply_capture(probes: list[Probe], dump: str) -> None:
    by_addr = {p.address: p for p in probes}
    for line in dump.splitlines():
        m = RE_ICMP6.search(line)
        if not m:
            continue
        src, dst, message = m.groups()
        if message.startswith("echo request") and src in by_addr:
            by_addr[src].echo_request += 1
        elif message.startswith("echo reply") and dst in by_addr:
            by_addr[dst].echo_reply += 1
        elif ns := RE_NS.search(line):
            if ns.group(1) in by_addr:
                by_addr[ns.group(1)].ns_for_addr += 1
        elif na := RE_NA.search(line):
            if na.group(1) in by_addr:
                by_addr[na.group(1)].na_for_addr += 1


# ---------------------------------------------------------------- 规律分析


def bit_diff(report: Report) -> None:
    """在主机位（IID）上逐位对比成功集与失败集，找出「成功地址里固定/占优」的位。"""
    ok_ips = [ipaddress.IPv6Address(p.address) for p in report.probes if p.ok]
    bad_ips = [ipaddress.IPv6Address(p.address) for p in report.probes if not p.ok]
    net = ipaddress.IPv6Network(report.prefix, strict=False)
    start_bit = net.prefixlen  # 只看主机位

    print("\n=== 主机位逐位对比（1 的占比：成功集 vs 失败集）===")
    if ok_ips:
        print("成功地址：")
        for ip in ok_ips:
            print(f"  {ip}")
    if bad_ips:
        print("失败地址（前 10 个）：")
        for ip in bad_ips[:10]:
            print(f"  {ip}")

    if not ok_ips:
        print("没有任何成功样本，位级对比无从谈起 —— 说明失败不是「某些地址不行」，而是全部被拦。")
        return
    if len(ok_ips) < 3:
        print(
            f"成功样本只有 {len(ok_ips)} 个（失败 {len(bad_ips)} 个），"
            "位级差异全是噪声，不做位模式推断 —— 请把 -n 加到 40 以上再判断。"
        )
        return

    rows: list[str] = []
    for bit in range(start_bit, 128):
        def ones_ratio(ips: list[ipaddress.IPv6Address]) -> float:
            if not ips:
                return float("nan")
            mask = 1 << (127 - bit)
            return sum(1 for ip in ips if int(ip) & mask) / len(ips)

        ok_ratio = ones_ratio(ok_ips)
        bad_ratio = ones_ratio(bad_ips)
        if ok_ips and bad_ips and abs(ok_ratio - bad_ratio) >= 0.5:
            rows.append(f"  主机位 {bit - start_bit:>2}（第 {bit} 位）：成功 {ok_ratio:.0%} / 失败 {bad_ratio:.0%}")

    if rows:
        print("显著差异位（任一方向相差 >= 50%）：")
        print("\n".join(rows))
    else:
        print("主机位上没有发现显著差异位 —— 成功与失败不是由地址的位模式决定的。")


def iid_features(report: Report) -> None:
    """统计 IID 形态：U/L 位、EUI-64 形态、solicited-node 组播是否已加入。"""
    print("\n=== IID 形态与 solicited-node 组播 ===")
    ok = [ipaddress.IPv6Address(p.address) for p in report.probes if p.ok]
    bad = [ipaddress.IPv6Address(p.address) for p in report.probes if not p.ok]

    def summarize(name: str, ips: list[ipaddress.IPv6Address]) -> None:
        if not ips:
            print(f"{name}：无样本")
            return
        ul_zero = sum(1 for ip in ips if not (ip.packed[8] & 0x02))
        eui64 = sum(1 for ip in ips if ip.packed[11:13] == b"\xff\xfe" and not (ip.packed[8] & 0x02))
        joined = sum(1 for ip in ips if solicit_node(ip) in report.multicast_groups)
        print(
            f"{name}（{len(ips)} 个）：U/L=0 {ul_zero} 个，EUI-64 形态 {eui64} 个，"
            f"solicited-node 组播已加入 {joined} 个"
        )

    summarize("成功地址", ok)
    summarize("失败地址", bad)
    print(f"网卡已加入的组播组：{', '.join(sorted(report.multicast_groups)) or '（无）'}")


def conclude(report: Report) -> None:
    """按抓包结论汇总失败环节，给出最可能的原因。"""
    print("\n=== 结论 ===")
    total = len(report.probes)
    ok = [p for p in report.probes if p.ok]
    print(f"样本 {total} 个，成功 {len(ok)} 个（成功率 {len(ok) / total:.1%}）")
    if report.baseline:
        state = "通" if report.baseline.ok else "不通"
        print(f"基准（网卡真实地址 {report.baseline.address}）：{state}，丢包 {report.baseline.loss_pct:.0f}%")
        if not report.baseline.ok:
            print("  ↑ 基准都不通说明本次探测条件有问题（目标抖动/等待时间太短/网络故障），先排查基准再用结论。")

    tally: dict[str, int] = {}
    for probe in report.probes:
        if probe.ok:
            continue
        tally[probe.verdict()] = tally.get(probe.verdict(), 0) + 1
    for reason, cnt in sorted(tally.items(), key=lambda kv: -kv[1]):
        print(f"  失败原因「{reason}」：{cnt} 个")

    verified = [p for p in report.probes if p.ns_for_addr > 0]
    if verified:
        resend = sum(1 for p in verified if p.ns_for_addr > 1)
        print(
            f"  其中 {len(verified)} 个地址被 CPE 主动做了邻居验证（发过 NS），"
            f"{resend} 个地址被重复询问（说明 NA 未被采纳）"
        )
    multi_na = [p for p in report.probes if p.na_for_addr > max(p.ns_for_addr, 1)]
    if multi_na:
        print(
            f"  注意：{len(multi_na)} 个地址的 NA 数量多于 NS —— 同一个 NS 被答了多次，"
            "说明本机有多个 NDP 应答者（例如容器内 ndppd 与宿主机 ndppd 同时运行），建议只留一个"
        )

    ok_ns = [p.ns_for_addr for p in report.probes if p.ok and p.ns_for_addr > 0]
    bad_ns = [p.ns_for_addr for p in report.probes if not p.ok and p.ns_for_addr > 0]
    if ok_ns and bad_ns:
        print(
            f"  成功地址平均被 CPE 询问 {sum(ok_ns) / len(ok_ns):.1f} 次，失败地址 {sum(bad_ns) / len(bad_ns):.1f} 次"
            "（NS 越多说明 CPE 反复验证，说明它是否采纳 NA 带有随机性）"
        )

    if len(ok) == 0:
        print("\n推断：随机出口在当前网络下完全不可用。")
    elif len(ok) == total:
        print("\n推断：随机出口全部可用。")

    if tally.get("CPE/上游丢弃（无邻居解析）"):
        print(
            "推断：出站包用随机源地址发出后，CPE 连邻居解析都没做就丢弃 ——\n"
            "      LAN 侧 IPv6 源地址校验/防欺骗（光猫的 IPv6 防火墙、地址绑定），需要关掉它或改桥接。"
        )
    if tally.get("无 NDP 应答（NS 有、NA 无）"):
        print(
            "推断：CPE 发来 NS 但没人回 NA —— 检查 ndppd 是否单实例运行、"
            "内核 proxy_ndp 是否开启（sysctl net.ipv6.conf.<if>.proxy_ndp=1）。"
        )
    if tally.get("CPE 收到 NA 仍未转发回程"):
        print(
            "推断：CPE 为这些随机源地址发了 NS、本机也回了 NA，但它仍然不转发回程/不回包 ——\n"
            "      典型原因是该地址只存在于路由表（local route），光猫不认；\n"
            "      用 --add-addr（默认开启）把地址真正挂到网卡上，让内核用带 override 标志的 NA 应答。"
        )
    if tally.get("本机未发出（bind/路由）"):
        print("推断：本机就没发出包 —— 检查 net.ipv6.ip_nonlocal_bind=1、池前缀的 local 路由是否存在。")


def write_csv(path: str, report: Report) -> None:
    with open(path, "w", newline="", encoding="utf-8") as fh:
        writer = csv.writer(fh)
        writer.writerow(
            [
                "address",
                "result",
                "loss_pct",
                "rtt_avg_ms",
                "echo_request",
                "echo_reply",
                "ns_for_addr",
                "na_for_addr",
                "solicited_node",
                "verdict",
            ]
        )
        for probe in report.probes:
            writer.writerow(
                [
                    probe.address,
                    "ok" if probe.ok else "fail",
                    probe.loss_pct,
                    probe.rtt_avg,
                    probe.echo_request,
                    probe.echo_reply,
                    probe.ns_for_addr,
                    probe.na_for_addr,
                    solicit_node(ipaddress.IPv6Address(probe.address)),
                    probe.verdict(),
                ]
            )
    print(f"\n明细已写入 {path}")


def main() -> None:
    parser = argparse.ArgumentParser(description="随机 IPv6 出口抽样：找出成功/失败地址的规律")
    parser.add_argument(
        "-i",
        "--iface",
        default=os.getenv("NET_IF"),
        help="网卡名（默认取 .env / 环境变量 NET_IF，都没配时按默认 IPv6 路由推断）",
    )
    parser.add_argument("-n", "--count", type=int, default=20, help="随机地址样本数（默认 20）")
    parser.add_argument("-c", "--pings", type=int, default=2, help="每个地址发几个包（默认 2）")
    parser.add_argument("-W", "--wait", type=int, default=4, help="每包等待秒数（默认 4，链路 rtt 峰值可能 >3s）")
    parser.add_argument("--interval", type=float, default=0.3, help="包间隔秒数（默认 0.3，改成 1 可看首包验证是否影响后续包）")
    parser.add_argument("-t", "--target", default="ip.sb", help="探测目标（默认 ip.sb）")
    parser.add_argument("--prefix", help="手动指定池前缀，如 2409:8a1e:2e94:84e0::/64")
    parser.add_argument("-o", "--csv", help="把明细写成 CSV")
    parser.add_argument("--capture", dest="capture", action="store_true", help="强制抓包分析")
    parser.add_argument("--no-capture", dest="capture", action="store_false", help="不抓包")
    parser.add_argument(
        "--add-addr",
        dest="add_addr",
        action="store_true",
        default=None,
        help="先把随机地址挂到网卡再探测（与 go 程序一致；需要 root 或 docker，默认开启）",
    )
    parser.add_argument(
        "--no-add-addr",
        dest="add_addr",
        action="store_false",
        help="只靠 local 路由 + ndppd 代答（旧路径，成功率通常很低）",
    )
    parser.set_defaults(capture=None)
    args = parser.parse_args()

    if not args.iface:
        args.iface = detect_iface()
    if not args.iface:
        raise SystemExit("确定不了网卡：请用 -i 指定，或在 .env / 环境变量里设置 NET_IF")

    if not shutil.which("ping"):
        raise SystemExit("找不到 ping 命令")
    if not check_nonlocal_bind():
        print(
            "[警告] net.ipv6.ip_nonlocal_bind 不是 1，绑随机源地址会失败；\n"
            "      先执行：sudo sysctl -w net.ipv6.ip_nonlocal_bind=1",
            file=sys.stderr,
        )

    real_addr, net = detect_prefix(args.iface)
    if args.prefix:
        net = ipaddress.IPv6Network(args.prefix, strict=False)

    try:
        target_ip = socket.getaddrinfo(args.target, None, socket.AF_INET6)[0][4][0]
    except OSError as exc:
        raise SystemExit(f"目标 {args.target} 没有可用的 IPv6 地址：{exc}") from exc

    report = Report(
        iface=args.iface,
        prefix=str(net),
        target=target_ip,
        real_address=real_addr,
        multicast_groups=multicast_groups(args.iface),
    )

    print(f"网卡 {args.iface}：真实地址 {real_addr}，池前缀 {net}")
    print(f"目标 {args.target} -> {target_ip}，样本 {args.count} 个，每样本 {args.pings} 包/{args.wait}s")

    add_addr = True if args.add_addr is None else args.add_addr
    probe_container: str | None = None
    if add_addr and os.geteuid() != 0:
        probe_container = probe_env_start()
        if probe_container is None:
            print(
                "[提示] 当前非 root 且 docker 不可用，--add-addr 无法执行，退回纯 ndppd 模式（成功率会偏低）",
                file=sys.stderr,
            )
            add_addr = False
    runner = PingRunner(args.iface, net.prefixlen, add_addr, probe_container)
    print(
        f"探测方式：{'先把地址挂到网卡（nodad）' if add_addr else '仅 local 路由 + ndppd 代答'}"
        + (f"，容器 {probe_container}" if probe_container else "")
    )

    do_capture = args.capture
    if do_capture is None:
        do_capture = shutil.which("docker") is not None
    capturing = capture_start(args.iface) if do_capture else False
    if do_capture and not capturing:
        print("[提示] 抓包不可用，只输出 ping 结果与地址规律", file=sys.stderr)

    try:
        print("\n先测基准（网卡真实地址）...")
        report.baseline = ping_probe(
            real_addr, target_ip, args.pings, args.wait, args.iface, args.interval, runner._wrap
        )
        print(f"  基准结果：{'通' if report.baseline.ok else '不通'}（丢包 {report.baseline.loss_pct:.0f}%）")

        for idx in range(args.count):
            addr = str(random_address(net))
            probe = runner.probe(addr, target_ip, args.pings, args.wait, args.interval)
            report.probes.append(probe)
            print(
                f"  [{idx + 1:>2}/{args.count}] {addr}  "
                f"{'通' if probe.ok else '不通'}  丢包 {probe.loss_pct:.0f}%"
                + (f"  rtt {probe.rtt_avg:.0f}ms" if probe.rtt_avg else "")
            )
    finally:
        dump = capture_stop() if capturing else ""
        if dump:
            apply_capture(report.probes, dump)
            if report.baseline:
                apply_capture([report.baseline], dump)
        if probe_container:
            run(["docker", "rm", "-f", probe_container])

    print("\n=== 明细 ===")
    for probe in report.probes:
        print(
            f"  {probe.address}  {'OK  ' if probe.ok else 'FAIL'}  "
            f"loss={probe.loss_pct:>5.0f}%  req={probe.echo_request} reply={probe.echo_reply} "
            f"ns={probe.ns_for_addr} na={probe.na_for_addr}  {probe.verdict()}"
        )

    bit_diff(report)
    iid_features(report)
    conclude(report)
    if args.csv:
        write_csv(args.csv, report)


if __name__ == "__main__":
    main()
