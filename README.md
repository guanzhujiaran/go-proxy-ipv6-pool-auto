# Go Proxy IPV6 Pool Auto

Random ipv6 egress proxy server (support http/socks5) 

The extension of [XiaoMiku01/go-proxy-ipv6-pool](https://github.com/XiaoMiku01/go-proxy-ipv6-pool)

## Usage

```bash
apt install ndppd -y
sysctl net.ipv6.ip_nonlocal_bind=1
```

```bash
    go run . --port <port> --cidr < your ipv6 cidr >  # e.g. 2001:399:8205:ae00::/64
```

### Use as a proxy server

```bash
    curl -x http://xxx:3128 http://6.ipw.cn/ # 2001:399:8205:ae00:456a:ab12 (random ipv6 address)
```

```bash
    curl -x socks5://xxx:3129 http://6.ipw.cn/ # 2001:399:8205:ae00:456a:ab12 (random ipv6 address)
```

## License

MIT License (see [LICENSE](go-proxy-ipv6-pool/LICENSE))
