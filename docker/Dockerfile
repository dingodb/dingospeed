FROM golang:1.23.0 AS builder

LABEL stage=gobuilder

ENV CGO_ENABLED=0
ENV GOPROXY=https://goproxy.cn,direct

WORKDIR /app

COPY .. .

# Installation of dependency packages and environment preparation
RUN go install github.com/google/wire/cmd/wire@latest
RUN cd cmd/ && wire gen ./...
RUN go mod tidy
RUN go get github.com/google/wire/cmd/wire@latest
RUN go generate ./...
RUN mkdir "repos"

# Compile Go project and generate executable file
RUN mkdir -p bin/ && CGO_ENABLED=0 GOOS=linux GOARCH=amd64  go build -ldflags "-s -w -X main.Version=$(VERSION)" -o ./bin/dingospeed dingospeed/cmd

# Expose the 8090 port of the container for external acces
EXPOSE 8090

VOLUME /app/repos
VOLUME /app/log

CMD ["./bin/dingospeed"]
