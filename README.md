# media-ai-gateway

Realtime audio gateway — nhận RTP/WebRTC từ hạ tầng IMS/telecom, xử lý audio, gửi sang AI worker (STT/translate) qua gRPC streaming, trả kết quả về backend qua HTTP callback hoặc WebRTC DataChannel.

## Yêu cầu cài đặt

### Build

| Công cụ | Phiên bản tối thiểu | Ghi chú |
|---------|---------------------|---------|
| Go | 1.25 | |
| GCC / G++ | hỗ trợ C++17 | Bắt buộc cho CGO — video decoder (C++17 + FFmpeg), PCAP (`gopacket/afpacket`), AMR-WB (opencore) |
| pkg-config | bất kỳ | CGO tìm FFmpeg qua pkg-config |

```bash
# Ubuntu/Debian
apt-get install build-essential pkg-config

# RedHat/CentOS
yum groupinstall "Development Tools"
yum install pkgconfig
```

### Chuẩn bị dependencies (vendor/)

Repo không commit thư mục `vendor/`. Sau khi clone (hoặc khi `go.mod`/`go.sum` thay đổi), chạy:

```bash
go mod download   # tải module (cần Internet hoặc GOPROXY)
go mod vendor     # tạo vendor/ từ go.mod + go.sum
```

Bắt buộc trước khi:
- Build Docker image — `deploy/Dockerfile` dùng `COPY vendor/` và `go build -mod=vendor`
- Chạy `scripts/test-real-ai-g711.sh` (build với `-mod=vendor`)

> Khi có `vendor/`, `go build` tự dùng vendor. Nếu báo `inconsistent vendoring` → chạy lại `go mod vendor`.

### System libraries

| Thư viện | Ubuntu/Debian | RedHat/CentOS | Dùng cho |
|----------|--------------|---------------|----------|
| FFmpeg (libavcodec, libavutil, libswscale) | `libavcodec-dev libavutil-dev libswscale-dev` | `ffmpeg-devel` ¹ | Video H.264 decode (`internal/codec/video`) — **bắt buộc** |
| Linux kernel headers | `linux-libc-dev` (có sẵn qua `build-essential`) | `kernel-headers` | PCAP capture (`gopacket/afpacket`) — **bắt buộc** |
| opencore-amrwb | `libopencore-amrwb-dev` | `opencore-amr-devel` ¹ | AMR-WB decode |
| opencore-amrnb | `libopencore-amrnb-dev` | `opencore-amr-devel` ¹ | AMR-NB decode (dự phòng) |

> ¹ RedHat/CentOS: cài từ [RPM Fusion](https://rpmfusion.org/) (FFmpeg, opencore-amr) hoặc build opencore-amr từ source tại [opencore-amr.sf.net](https://opencore-amr.sourceforge.net/).

**Cài FFmpeg dev (Ubuntu/Debian):**
```bash
apt-get install libavcodec-dev libavutil-dev libswscale-dev
```

> Thiếu FFmpeg dev hoặc kernel headers thì không build được `cmd/media-ai-gateway`.

**Cài opencore-amr (Ubuntu/Debian):**
```bash
apt-get install libopencore-amrwb-dev libopencore-amrnb-dev
```

**Build với AMR-WB support:**
```bash
CGO_ENABLED=1 go build -tags opencore_amrwb ./...

# Chạy test AMR-WB
CGO_ENABLED=1 go test -tags opencore_amrwb ./internal/codec/...
```

> Nếu không cài opencore-amr, gateway vẫn build và chạy bình thường —  
> chỉ `Decode()` với codec AMR-WB / AMR-NB sẽ trả về `ErrAMRNotAvailable`.

### Lab testing (scripts/)

| Công cụ | Ghi chú |
|---------|---------|
| `curl` | Cần nghttp2 (HTTP/2 support) — Ubuntu: `apt-get install curl`, RedHat: xem script |
| `python3` | Tạo RTP packet (inline) trong các test script |
| `ffmpeg` | Test transcode audio trong `test-rtp-*.sh` |

```bash
# Ubuntu/Debian
apt-get install curl python3 ffmpeg

# RedHat/CentOS
yum install python3 ffmpeg   # ffmpeg từ RPM Fusion
```

### Development binaries (cmd/)

| Binary | Dùng cho |
|--------|----------|
| `cmd/mock-ai-worker` | Fake AI gRPC server — partial mỗi 3 chunks, final mỗi 6 chunks |
| `cmd/mock-callback-server` | H2C callback receiver — log mỗi callback, thoát sau N final |
| `cmd/mock-rtp-sender` | Gửi RTP packet từ file PCM/AMR/G.711 vào gateway (dùng trong test script) |

```bash
# Build tất cả (pure Go, không cần CGO hay library ngoài)
go build -o bin/mock-ai-worker       ./cmd/mock-ai-worker
go build -o bin/mock-callback-server ./cmd/mock-callback-server
go build -o bin/mock-rtp-sender      ./cmd/mock-rtp-sender

# Usage: mock-callback-server
./bin/mock-callback-server --port 9999 --expect-final 1 --timeout 30s
```

| Flag | Default | Ý nghĩa |
|------|---------|---------|
| `--port` | `9999` | TCP port lắng nghe H2C |
| `--expect-final` | `0` (daemon) | Thoát sau khi nhận đủ N `is_final=true` |
| `--timeout` | `30s` | Timeout khi `expect-final > 0` |

Output: mỗi dòng là một JSON — `{"event":"ready"}`, `{"event":"callback",...}`, `{"event":"summary",...}`.

## Config profiles

| File | Dùng cho |
|------|----------|
| `config/gateway.yaml` | Reference config — tất cả field có comment giải thích, giá trị default production |
| `config/gateway-mock.yaml` | Lab test — `mock-ai-worker` localhost, PCM dump, callback timeout rộng |
| `config/gateway-ims.yaml` | IMS/Telecom — AMR-WB, SBC per-session port pool, jitter rộng hơn |
| `config/gateway-ha.yaml` | High Availability — multi-node cluster, port range tách biệt per node |

```bash
./bin/media-ai-gateway --config config/gateway-ims.yaml
./bin/media-ai-gateway --config config/gateway-ha.yaml
```

## Deploy

```bash
# Chuẩn bị vendor/ (bắt buộc, Dockerfile build -mod=vendor)
go mod vendor

# Build image
docker build -f deploy/Dockerfile -t registry.local/media-ai-gateway:latest .
docker push registry.local/media-ai-gateway:latest

# Apply lên Kubernetes
kubectl apply -f deploy/k8s/network-attachment.yaml
kubectl apply -f deploy/k8s/configmap.yaml
kubectl apply -f deploy/k8s/deployment.yaml
kubectl apply -f deploy/k8s/service.yaml
```

**Build args** (`deploy/Dockerfile`):

| Arg | Default | Ý nghĩa |
|-----|---------|---------|
| `GO_TAGS` | `opencore_amrwb` | Build tag tính năng tuỳ chọn. `""` = tắt AMR-WB / AMR-NB |
| `ALPINE_VERSION` | `3.22` | Version Alpine dùng chung cho stage build (`golang:1.25-alpine<ver>`) và runtime. Phải giống nhau vì binary CGO link động tới musl + FFmpeg `.so` |

```bash
# Ví dụ: build không có AMR-WB
docker build -f deploy/Dockerfile --build-arg GO_TAGS="" -t registry.local/media-ai-gateway:latest .
```

> Image runtime có sẵn thư viện FFmpeg `.so` (không có binary `ffmpeg`), dung lượng khoảng 175MB.

> Xem `deploy/k8s/network-attachment.yaml` để chỉnh `master` interface và subnet macvlan trước khi apply.

## Documentation

| File | Mô tả |
|------|-------|
| [docs/openapi.yaml](docs/openapi.yaml) | OpenAPI 3.0.3 spec — tất cả REST endpoints của gateway (sessions, WebRTC, health, metrics) |
| [docs/openapi-callback.yaml](docs/openapi-callback.yaml) | OpenAPI 3.0.3 spec — Callback Receiver API mà **MF** phải implement để nhận ASR kết quả |
| [docs/callback.md](docs/callback.md) | Callback message reference — HTTP/2 POST format, JSON schema, retry policy, drop policy, implement guide |
| [docs/metrics.md](docs/metrics.md) | Prometheus metrics reference — 37 metric, ý nghĩa, alert threshold, drop rate |
| [docs/media-ai-gateway-design-modules_v2.md](docs/media-ai-gateway-design-modules_v2.md) | Thiết kế kiến trúc hiện hành — module breakdown, luồng dữ liệu, API, config đầy đủ |
| [docs/media-ai-gateway-design-modules_v1.md](docs/media-ai-gateway-design-modules_v1.md) | Phiên bản thiết kế ban đầu (tham khảo) |
| [docs/pion-webrtc-analysis.md](docs/pion-webrtc-analysis.md) | Phân tích pion/webrtc v4 — lựa chọn API, ICE/DTLS/SRTP, DataChannel |
| [docs/stack_webrtc.md](docs/stack_webrtc.md) | IMS Data Channel setup modes (HTTP Proxy / UDP Proxy) |
| [docs/webrct_ims_data_channel.md](docs/webrct_ims_data_channel.md) | WebRTC IMS Data Channel — ghi chú tích hợp |
| [docs/23228-j00.docx](docs/23228-j00.docx) | 3GPP TS 23.228 Annex AC — IMS Data Channel spec (tham chiếu chuẩn) |
