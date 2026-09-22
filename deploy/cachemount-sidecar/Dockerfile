# Build context: this directory (source snapshot included).
FROM golang:1.24 AS build
WORKDIR /src
COPY source/ ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/cachemount ./cmd/cachemount

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends python3 fuse3 ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -g 65532 nonroot \
    && useradd -u 65532 -g 65532 -d /home/nonroot nonroot \
    && mkdir -p /run/cachemount /home/nonroot \
    && chown -R 65532:65532 /run/cachemount /home/nonroot
COPY --from=build /out/cachemount /usr/local/bin/cachemount
COPY source/sidecar.py /opt/cachemount/sidecar.py
USER 65532:65532
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
ENTRYPOINT ["python3", "/opt/cachemount/sidecar.py"]
