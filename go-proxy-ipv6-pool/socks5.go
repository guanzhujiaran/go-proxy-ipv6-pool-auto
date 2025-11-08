package main

import (
	"context"
	"log"
	"net"

	socks5 "github.com/armon/go-socks5"
)

var socks5Server *socks5.Server

func init() {
	socks5Conf := &socks5.Config{
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			outgoingIP, err := generateRandomIPv6(cidr)
			if err != nil {
				if runEnv == "dev" {
					log.Printf("[SOCKS5] Generate random IPv6 error: %v", err)
				}
				return nil, err
			}

			// 自动添加 IP 到接口
			if err := addIPv6ToInterface(outgoingIP, netIf); err != nil {
				if runEnv == "dev" {
					log.Printf("[SOCKS5] Failed to add IP %s to %s: %v", outgoingIP, netIf, err)
				}
				return nil, err
			}

			// 注意：ResolveTCPAddr 不需要方括号！
			localAddr, err := net.ResolveTCPAddr("tcp", outgoingIP+":0")
			if err != nil {
				if runEnv == "dev" {
					log.Printf("[SOCKS5] Resolve local address error: %v", err)
				}
				return nil, err
			}

			dialer := &net.Dialer{
				LocalAddr: localAddr,
			}

			if runEnv == "dev" {
				log.Printf("[SOCKS5] Connecting to %s via %s", addr, outgoingIP)
			}

			return dialer.DialContext(ctx, network, addr)
		},
	}

	var err error
	socks5Server, err = socks5.New(socks5Conf)
	if err != nil {
		log.Fatal("[SOCKS5] Server init failed:", err)
	}
}
