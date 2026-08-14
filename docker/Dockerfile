# Build context is the repo root; Jenkins runs:
#   docker build --pull --file docker/Dockerfile --tag "$IMAGE" .
FROM golang:1.23-bookworm AS builder

LABEL stage=gobuilder

ENV CGO_ENABLED=0
# 内网构建：走国内 Go 代理（CI 可用 --build-arg GOPROXY 覆盖）。
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}

WORKDIR /app

# Cached module download — go.mod / go.sum change less often than source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# wire_gen.go is committed, so no `wire gen` here. `go mod tidy` / `go get` are
# gone too: they mutate go.mod at build time, which makes the image depend on
# whatever upstream published today rather than on go.sum.
ARG VERSION=dev
ARG TARGETARCH=amd64
RUN mkdir -p bin/ \
    && GOOS=linux GOARCH=${TARGETARCH} \
       go build -trimpath -ldflags "-s -w -X main.Version=${VERSION}" \
       -o ./bin/dingospeed dingospeed/cmd

# Runtime: slim Debian rather than the 1GB toolchain image.
#
# No apt-get here on purpose. This build server reaches the internet only
# through the docker *daemon* proxy (image pulls); build containers get no
# proxy, and deb.debian.org is unreachable from them — `apt-get update` stalls
# until it times out. The two things the runtime actually needs are copied out
# of the builder stage instead, which is also faster and pins them to the same
# base the binary was compiled against.
#
#   ca-certificates.crt — the binary is static, but Go still reads the system
#                         trust store to reach huggingface.co / hf-mirror.com.
#   zoneinfo            — keeps log timestamps and the cache-cleanup schedule
#                         on Asia/Shanghai.
FROM debian:bookworm-slim

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

ENV TZ=Asia/Shanghai

WORKDIR /app

COPY --from=builder /app/bin/dingospeed ./bin/dingospeed
COPY --from=builder /app/docker/config/ ./docker/config/
COPY --from=builder /app/config/ ./config/

RUN mkdir -p /app/repos /app/log

# Runs as root, matching the containers already deployed on the test host: the
# repos/ and log/ volumes and the NFS bind mount were provisioned for that uid.
# 8090 is the download API. 8091 (upload) is deliberately NOT exposed: the
# server forces upload.host back to 127.0.0.1 (internal/server/upload.go), so
# under bridge networking it binds the *container's* loopback and the host-side
# ingest agent cannot reach it. A deployment that needs the upload path must run
# this image with --network host (see spinfield deploy/spinfield-ingest/README).
EXPOSE 8090

VOLUME /app/repos
VOLUME /app/log

CMD ["./bin/dingospeed"]
