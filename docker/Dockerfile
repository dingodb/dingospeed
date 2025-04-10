# 使用官方的 Go 语言镜像作为基础镜像
FROM golang:1.23.0 AS builder

LABEL stage=gobuilder

ENV CGO_ENABLED=0
ENV GOPROXY=https://goproxy.cn,direct

# 设置工作目录为 /app
# 所有后续操作都会在这个目录下进行
WORKDIR /app

COPY .. .

# 安装依赖包和环境准备
RUN go install github.com/google/wire/cmd/wire@latest
RUN cd cmd/ && wire gen ./...
RUN go mod tidy
RUN go get github.com/google/wire/cmd/wire@latest
RUN go generate ./...

# 编译 Go 项目，生成可执行文件
RUN mkdir -p bin/ && CGO_ENABLED=0 GOOS=linux GOARCH=amd64  go build -ldflags "-s -w -X main.Version=$(VERSION)" -o ./bin/dingo-hfmirror dingo-hfmirror/cmd

# 暴露容器的 8090 端口，用于外部访问
EXPOSE 8090

# 设置容器启动时运行的命令
# 这里是运行编译好的可执行文件
CMD ["./bin/dingo-hfmirror"]
