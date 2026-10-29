package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/elazarl/goproxy"
)

// isTimeoutError 判断错误是否由超时引起（拨号超时、响应超时、客户端取消）。
func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return false
}

// proxyErrorStatus 把转发失败映射成对客户端的状态码与错误描述：
// 超时 -> 504，其他（拨号被拒、DNS 失败、连接重置等）-> 502。
func proxyErrorStatus(err error) (int, string) {
	if isTimeoutError(err) {
		return http.StatusGatewayTimeout, fmt.Sprintf(
			"proxy timeout: no exit reachable within %s x %d attempts", dialTimeout, dialMaxAttempts)
	}
	return http.StatusBadGateway, "proxy bad gateway: failed to reach target"
}

// writeConnectError 在 CONNECT 隧道未建立时按状态码回一个规范的错误响应。
func writeConnectError(client net.Conn, status int) {
	fmt.Fprintf(client, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\n\r\n", status, http.StatusText(status))
}

// newProxyErrorResponse 构造代理自身的错误响应，直接回给客户端，
// 避免请求在失败后继续用宿主机默认路由转发（那会让出口 IP 不可控）。
func newProxyErrorResponse(req *http.Request, status int, msg string) *http.Response {
	resp := goproxy.NewResponse(req, "text/plain; charset=utf-8", status, msg+"\n")
	resp.Header.Set("X-Proxy-Error", msg)
	return resp
}
