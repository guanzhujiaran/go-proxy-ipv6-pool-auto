package main

import (
	"context"
	"log"
	"net"

	socks5 "github.com/armon/go-socks5"
)

var socks5Conf = &socks5.Config{}
var socks5Server *socks5.Server

func init() {
	// 指定出口 IP 地址
	// 指定本地出口 IPv6 地址

	// 创建一个 SOCKS5 服务器配置
	socks5Conf = &socks5.Config{
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// 拨号层负责「单次超时就换随机出口」重试
			conn, outgoingIP, err := dialWithRandomIPv6(ctx, "[socks5]", network, addr)
			if err != nil {
				return nil, err
			}
			log.Println("[socks5]", addr, "via", outgoingIP)
			return conn, nil
		},
	}
	var err error
	// 创建 SOCKS5 服务器
	socks5Server, err = socks5.New(socks5Conf)
	if err != nil {
		log.Fatal(err)
	}
}
