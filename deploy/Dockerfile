# Stage 1: build
FROM golang:1.25-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
COPY vendor/ vendor/
COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -mod=vendor -trimpath \
    -ldflags="-s -w" \
    -o /media-ai-gateway \
    ./cmd/media-ai-gateway

# Stage 2: runtime
# Dùng alpine (có sh) để init container có thể inject macvlan IP qua shell wrapper
FROM alpine:3.20

RUN apk add --no-cache ca-certificates && \
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
