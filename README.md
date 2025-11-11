# Go Proxy IPV6 Pool Auto

Random ipv6 egress proxy server (support http/socks5)

The simple extension of [XiaoMiku01/go-proxy-ipv6-pool](https://github.com/XiaoMiku01/go-proxy-ipv6-pool)

## Usage

```bash
apt install ndppd -y
sysctl net.ipv6.ip_nonlocal_bind=1
sudo ip route add local xx:xx:xx:xx::1/64 dev eth0
```

```bash
vim /etc/ndppd.conf
```

```text
route-ttl 30000
proxy eth0 {
    router no
	timeout 500
	ttl 30000
    rule xx:xx:xx:xx::/64 {
        static
    }
}
```

```bash
service ndppd restart
```

test with curl

```bash
curl --interface 2409:8a1e:2e90:d20::43 test.ipw.cn
```

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
> 3. ip -6 ro has one public ipv6 route
### reset ifconfig setting if not work (Caution)

```bash
ip addr flush dev eth0;
ip -6 ro flush dev eth0;
sudo ifconfig eth0 down; 
sudo ifconfig eth0 up;
```
```bash
ip addr flush dev lo;
ip -6 ro flush dev lo;
sudo ifconfig lo down; 
sudo ifconfig lo up;
```

## License

MIT License (see [LICENSE](go-proxy-ipv6-pool/LICENSE))
