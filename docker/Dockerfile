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
#
# The offline instance's "cache to public" job (internal/service/task/
# mount_cache_task.go) shells out to the Hugging Face CLI, `hf download`.
# bookworm-slim has no `hf`, so since the 2026-10-10 images every mount failed
# with `exec: "hf": executable file not found in $PATH`. python:slim is the same
# Debian base with Python preinstalled; pip goes to a domestic mirror because
# PyPI, like deb.debian.org, is unreachable from build containers. Override
# with --build-arg PIP_INDEX_URL. click/typer are listed because
# huggingface_hub 1.2.3 does not pull in the CLI's own dependencies.
FROM python:3.12-slim-bookworm

ARG PIP_INDEX_URL=https://mirrors.aliyun.com/pypi/simple/
RUN pip install --no-cache-dir -i "${PIP_INDEX_URL}" \
        "huggingface_hub==1.2.3" "click" "typer" \
    && hf version

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
