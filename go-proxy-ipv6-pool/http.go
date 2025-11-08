package main

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"

	"github.com/elazarl/goproxy"
)

func init() {
	httpProxy.Verbose = runEnv == "dev"

	httpProxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		outgoingIP, err := generateRandomIPv6(cidr)
		if err != nil {
			if runEnv == "dev" {
				log.Printf("[HTTP] Generate IPv6 error: %v", err)
			}
			return req, nil
		}

		if err := addIPv6ToInterface(outgoingIP, netIf); err != nil {
			if runEnv == "dev" {
				log.Printf("[HTTP] Failed to add IP %s: %v", outgoingIP, err)
			}
			return req, nil
		}

		localAddr, err := net.ResolveTCPAddr("tcp", "["+outgoingIP+"]"+":0")
		if runEnv == "dev" {
			log.Printf("[HTTP] Outgoing IP: %s", outgoingIP)
		}
		if err != nil {
			if runEnv == "dev" {
				log.Printf("[HTTP] Resolve local addr error: %v", err)
			}
			return req, nil
		}

		dialer := &net.Dialer{LocalAddr: localAddr}
		client := &http.Client{
			Transport: &http.Transport{
				DialContext: dialer.DialContext,
			},
		}

		newReq, err := http.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), req.Body)
		if err != nil {
			if runEnv == "dev" {
				log.Printf("[HTTP] New request error: %v", err)
			}
			return req, nil
		}
		newReq.Header = req.Header.Clone()

		resp, err := client.Do(newReq)
		if err != nil {
			if runEnv == "dev" {
				log.Printf("[HTTP] Request error: %v", err)
			}
			return req, nil
		}
		return req, resp
	})

	httpProxy.OnRequest().HijackConnect(func(req *http.Request, client net.Conn, ctx *goproxy.ProxyCtx) {
		outgoingIP, err := generateRandomIPv6(cidr)
		if err != nil {
			if runEnv == "dev" {
				log.Printf("[HTTPS] Generate IPv6 error: %v", err)
			}
			client.Write([]byte("HTTP/1.1 500 Internal Server Error\r\n\r\n"))
			client.Close()
			return
		}

		if err := addIPv6ToInterface(outgoingIP, netIf); err != nil {
			if runEnv == "dev" {
				log.Printf("[HTTPS] Failed to add IP %s: %v", outgoingIP, err)
			}
			client.Write([]byte("HTTP/1.1 500 Internal Server Error\r\n\r\n"))
			client.Close()
			return
		}

		localAddr, err := net.ResolveTCPAddr("tcp", "["+outgoingIP+"]"+":0")
		if err != nil {
			if runEnv == "dev" {
				log.Printf("[HTTPS] Resolve local addr error: %v", err)
			}
			client.Write([]byte("HTTP/1.1 500 Internal Server Error\r\n\r\n"))
			client.Close()
			return
		}

		dialer := &net.Dialer{LocalAddr: localAddr}
		server, err := dialer.DialContext(context.Background(), "tcp", req.URL.Host)
		if err != nil {
			if runEnv == "dev" {
				log.Printf("[HTTPS] Dial error to %s: %v", req.URL.Host, err)
			}
			client.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
			client.Close()
			return
		}

		client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

		go func() { io.Copy(server, client); server.Close(); client.Close() }()
		go func() { io.Copy(client, server); server.Close(); client.Close() }()
	})
}
