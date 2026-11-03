# Builder và runtime PHẢI cùng version Alpine: binary build với CGO link động
# tới musl + FFmpeg .so của builder, lệch version sẽ lỗi thiếu thư viện lúc chạy.
ARG ALPINE_VERSION=3.22

# Stage 1: build
FROM golang:1.25-alpine${ALPINE_VERSION} AS builder

# Build tag bật tính năng tuỳ chọn:
#   opencore_amrwb = decode AMR-WB / AMR-NB (cần cho profile IMS)
# Tắt: --build-arg GO_TAGS=""
ARG GO_TAGS=opencore_amrwb

# CGO bắt buộc cho:
#   internal/codec/video  — C++17 + FFmpeg (libavcodec/libavutil/libswscale)
#   internal/debug/pcap   — gopacket/afpacket (cần linux-headers: linux/if_packet.h)
#   internal/codec (tag opencore_amrwb) — libopencore-amrwb
RUN apk add --no-cache build-base pkgconf linux-headers ffmpeg-dev opencore-amr-dev

WORKDIR /src

COPY go.mod go.sum ./
# vendor/ không có trong git — chạy `go mod vendor` trước khi docker build
COPY vendor/ vendor/
COPY . .

RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
    go build -mod=vendor -trimpath -tags "${GO_TAGS}" \
    -ldflags="-s -w" \
    -o /media-ai-gateway \
    ./cmd/media-ai-gateway

# Stage 2: runtime
# Dùng alpine (có sh) để init container có thể inject macvlan IP qua shell wrapper
FROM alpine:${ALPINE_VERSION}

# Thư viện runtime cho binary CGO (chỉ .so, không cần binary ffmpeg)
RUN apk add --no-cache ca-certificates \
        ffmpeg-libavcodec ffmpeg-libavutil ffmpeg-libswscale \
        opencore-amr && \
    adduser -D -u 1000 gateway

COPY --from=builder /media-ai-gateway /media-ai-gateway

USER gateway

# Control plane HTTP/2
EXPOSE 8080
# Metrics
EXPOSE 9090
# Shared RTP UDP ingress
EXPOSE 5004/udp

ENTRYPOINT ["/media-ai-gateway"]
