# Go Proxy IPV6 Pool Auto

Random ipv6 egress proxy server (support http/socks5)

The simple extension of [XiaoMiku01/go-proxy-ipv6-pool](https://github.com/XiaoMiku01/go-proxy-ipv6-pool)

## Usage

```bash
apt install ndppd -y
sysctl net.ipv6.ip_nonlocal_bind=1
ip route add local xxx/64 dev eth0
```

```bash
vim /etc/ndppd.conf
```

make sure xxx is exactly same as above

```text
route-ttl 30000
proxy eth0 {
        router no
    rule xxx/64 {
        static
    }
}
```

```bash
service ndppd restart
```

test with curl

```bash
curl --interface yyy test.ipw.cn
```

response yyy if yyy in cidr xxx

```bash
    go run ./go-proxy-ipv6-pool --port <port> --prefix < your ipv6 cidr prefix length >  # e.g. 2001:399:8205:ae00::/64
```

### Use as a proxy server

```bash
    curl -x http://xxx:3128 http://6.ipw.cn/ # 2001:399:8205:ae00:456a:ab12 (random ipv6 address)
```

```bash
    curl -x socks5://xxx:3129 http://6.ipw.cn/ # 2001:399:8205:ae00:456a:ab12 (random ipv6 address)
```

### guide

> make sure ：
> 1. sysctl net.ipv6.ip_nonlocal_bind=1
> 2. /etc/ndppd.conf rule ip match ifconfig ip

### reset ifconfig setting if not work (Caution)

```bash
ip addr flush dev eth0
ip -6 ro flush dev eth0
ifconfig eth0 down
ifconfig eth0 up
```

## License

MIT License (see [LICENSE](go-proxy-ipv6-pool/LICENSE))
