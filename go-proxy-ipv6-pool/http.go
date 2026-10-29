package main

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/elazarl/goproxy"
)

var httpProxy = goproxy.NewProxyHttpServer()

func init() {
	if runEnv == "dev" {
		httpProxy.Verbose = true
	} else {
		httpProxy.Verbose = false
	}

	httpProxy.OnRequest().DoFunc(
		func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
			// 通过代理服务器建立到目标服务器的连接
			// 发送 http 请求
			// 使用自定义拨号器设置 HTTP 客户端
			// 创建新的 HTTP 请求

			newReq, err := http.NewRequest(req.Method, req.URL.String(), req.Body)
			if err != nil {
				log.Printf("[http] New request error: %v", err)
				return req, newProxyErrorResponse(req, http.StatusBadRequest, "proxy bad request: invalid request url")
			}
			newReq.Header = req.Header

			// 设置自定义拨号器的 HTTP 客户端
			// 拨号层负责「单次超时就换随机出口」重试，请求体不会被重放；
			// Timeout 覆盖「拨号重试 + 等待响应」全程，超时后直接回 504，不再无限挂住
			client := &http.Client{
				Timeout: requestTimeout,
				Transport: &http.Transport{
					DialContext: func(dialCtx context.Context, network, addr string) (net.Conn, error) {
						start := time.Now()
						conn, outgoingIP, err := dialWithRandomIPv6(dialCtx, "[http]", network, addr)
						if err != nil {
							return nil, err
						}
						// 记录实际连上的对端 IP 与拨号耗时，便于和失败日志的目标 IP 对比，
						// 区分「出口地址不可用」还是「目标节点不可达」
						log.Printf("[http] %s %s via %s -> %s, dial in %s", req.Method, req.URL.String(),
							outgoingIP, conn.RemoteAddr(), time.Since(start).Round(time.Millisecond))
						return conn, nil
					},
				},
			}

			// 发送 HTTP 请求
			resp, err := client.Do(newReq)
			if err != nil {
				status, msg := proxyErrorStatus(err)
				log.Printf("[http] %s %s failed (%d): %v", req.Method, req.URL.String(), status, err)
				return req, newProxyErrorResponse(req, status, msg)
			}
			return req, resp
		},
	)

	httpProxy.OnRequest().HijackConnect(
		func(req *http.Request, client net.Conn, ctx *goproxy.ProxyCtx) {
			// 通过代理服务器建立到目标服务器的连接
			server, outgoingIP, err := dialWithRandomIPv6(context.Background(), "[http] CONNECT", "tcp", req.URL.Host)
			if err != nil {
				status, msg := proxyErrorStatus(err)
				log.Printf("[http] CONNECT %s failed (%d): %s, err: %v", req.URL.Host, status, msg, err)
				writeConnectError(client, status)
				client.Close()
				return
			}
			log.Printf("[http] CONNECT %s via %s -> %s", req.URL.Host, outgoingIP, server.RemoteAddr())

			// 响应客户端连接已建立
			client.Write([]byte("HTTP/1.0 200 OK\r\n\r\n"))
			// 从客户端复制数据到目标服务器
			go func() {
				defer server.Close()
				defer client.Close()
				io.Copy(server, client)
			}()

			// 从目标服务器复制数据到客户端
			go func() {
				defer server.Close()
				defer client.Close()
				io.Copy(client, server)
			}()

		},
	)
}
