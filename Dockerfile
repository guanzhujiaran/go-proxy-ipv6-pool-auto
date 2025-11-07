# 使用官方 Golang 镜像作为基础镜像
FROM golang:1.25

# 安装 ndppd
RUN apt-get update && apt-get install -y ndppd iproute2

# 设置工作目录
WORKDIR /app

# 复制 go.mod 和 go.sum 文件并下载依赖
COPY ./go-proxy-ipv6-pool/go.mod ./go-proxy-ipv6-pool/go.sum ./
RUN go mod download
RUN apt-get install -y vim net-tools sudo
# 复制项目文件
COPY ./go-proxy-ipv6-pool/ .

# 构建应用
RUN go build -o main .

# 暴露端口
EXPOSE 3128


# 运行应用
CMD ["./main"]