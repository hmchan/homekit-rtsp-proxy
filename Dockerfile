# syntax=docker/dockerfile:1.7

FROM golang:1.25-bookworm AS builder
RUN sed -i 's/^Components: main$/Components: main non-free/' /etc/apt/sources.list.d/debian.sources && \
    apt-get update && apt-get install -y --no-install-recommends \
        libfdk-aac-dev \
        libavcodec-dev libavutil-dev libswscale-dev \
        && rm -rf /var/lib/apt/lists/*
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /app/homekit-rtsp-proxy ./cmd/homekit-rtsp-proxy/

FROM debian:bookworm-slim
RUN sed -i 's/^Components: main$/Components: main non-free/' /etc/apt/sources.list.d/debian.sources && \
    apt-get update && apt-get install -y --no-install-recommends \
        libfdk-aac2 \
        libavcodec59 libavutil57 libswscale6 \
        ca-certificates \
        && rm -rf /var/lib/apt/lists/*
# Run unprivileged: the process parses camera-supplied H.264/AAC in-process
# via libavcodec/libfdk-aac, so contain any parser compromise. UID 1000
# matches the pi user on the host, which owns the bind-mounted data dir.
RUN useradd --uid 1000 --user-group --home-dir /app/data \
        --shell /usr/sbin/nologin rtsp
COPY --from=builder /app/homekit-rtsp-proxy /app/homekit-rtsp-proxy
WORKDIR /app/data
USER rtsp
CMD ["/app/homekit-rtsp-proxy", "-config", "config.yaml"]
