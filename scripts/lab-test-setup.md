# Hướng dẫn thiết lập test lab — Media AI Gateway

## Tổng quan kiến trúc

Gateway implement DCSF API (Vonras). Mỗi cuộc gọi tạo **2 session song song**:
`{callId}-tcore` (network-side) và `{callId}-taccess` (subscriber-side), mỗi session có RTP port riêng.

```
DCSF (IMS AS)
  │  POST /v1/vonras/call-sessions/{callId}/notify-event  (ANSWER)
  │      → gateway tạo 2 sessions: {callId}-tcore + {callId}-taccess
  │      ← response: tcore_rtp_port, taccess_rtp_port
  │
  │  POST /v1/vonras/call-sessions/{callId}/ctrl-result
  │      → gateway set callbackUrl per-termination
  │
MF (Media Function)
  │  UDP RTP  → tcore_rtp_port   (network-side audio)
  │  UDP RTP  → taccess_rtp_port (subscriber-side audio)
  ▼
media-ai-gateway :8080
  │  decode PCMU → PCM 16kHz → chunk 500ms
  │  gRPC protobuf → AI worker :50051
  │
  │  kết quả từ AI → dispatcher
  │  HTTP/2 POST callbackUrl (per-termination)
  ▼
mock-callback-server :9999   ← H2C server, log JSON mỗi callback
```

---

## Test scripts có sẵn

| Script | Mô tả | Yêu cầu |
|---|---|---|
| `test-callback-e2e.sh` | Tự build + chạy + verify toàn bộ pipeline với **mock AI** | Go, curl, python3 |
| `test-realai-e2e.sh` | E2E với **AI thật**, đọc file G.711 60s thực, binaries đã build sẵn | Binaries + AI gRPC |

---

## Bước 1 — Build binary

### Option A: Build trực tiếp (Linux/macOS)

```bash
cd ~/media-ai/src
mkdir -p bin

go build -o bin/media-ai-gateway     ./cmd/media-ai-gateway/
go build -o bin/mock-ai-worker       ./cmd/mock-ai-worker/
go build -o bin/mock-rtp-sender      ./cmd/mock-rtp-sender/
go build -o bin/mock-callback-server ./cmd/mock-callback-server/
```

### Option B: Cross-compile từ Windows (PowerShell)

```powershell
cd D:\NAMCHT\ims\SRC\media_ai
New-Item -ItemType Directory -Force bin | Out-Null

$env:GOOS="linux"; $env:GOARCH="amd64"
go build -o bin/media-ai-gateway     ./cmd/media-ai-gateway/
go build -o bin/mock-ai-worker       ./cmd/mock-ai-worker/
go build -o bin/mock-rtp-sender      ./cmd/mock-rtp-sender/
go build -o bin/mock-callback-server ./cmd/mock-callback-server/
Remove-Item Env:\GOOS, Env:\GOARCH
```

---

## Bước 2 — Copy lên lab server

```bash
LAB="USER@LAB_IP"

# Binaries
scp bin/media-ai-gateway     $LAB:~/media-ai/bin/
scp bin/mock-ai-worker       $LAB:~/media-ai/bin/
scp bin/mock-rtp-sender      $LAB:~/media-ai/bin/
scp bin/mock-callback-server $LAB:~/media-ai/bin/

# Config
scp config/gateway-mock.yaml $LAB:~/media-ai/config/

# Scripts
scp scripts/test-callback-e2e.sh $LAB:~/media-ai/scripts/
scp scripts/test-realai-e2e.sh   $LAB:~/media-ai/scripts/

# Data
scp data/generated/g711/speech.pcmu $LAB:~/media-ai/data/generated/g711/
```

Cấu trúc thư mục trên lab:

```
~/media-ai/
├── bin/
│   ├── media-ai-gateway        (executable)
│   ├── mock-ai-worker          (executable)
│   ├── mock-rtp-sender         (executable)
│   └── mock-callback-server    (executable)
├── config/
│   └── gateway-mock.yaml
├── scripts/
│   ├── test-callback-e2e.sh
│   └── test-realai-e2e.sh
└── data/
    └── generated/
        └── g711/
            └── speech.pcmu     (480044 bytes = 3000 frames = 60s)
```

---

## Bước 3 — Test với mock AI (test-callback-e2e.sh)

Script **tự quản lý toàn bộ**: build binary → khởi động services → tạo sessions → gửi RTP → verify callback → cleanup.

### Chạy

```bash
cd ~/media-ai/src       # hoặc thư mục gốc source
chmod +x scripts/test-callback-e2e.sh
bash scripts/test-callback-e2e.sh
```

### Luồng script

```
Step 1  Preflight (kiểm tra Go, curl, python3, config)
Step 2  Build: gateway, mock-ai-worker, mock-rtp-sender, mock-callback-server
Step 3  Start: mock-ai-worker :50051
        Start: gateway :8080 (config/gateway-mock.yaml)
        Start: mock-callback-server :9999 (daemon mode)
Step 4  Metrics baseline
Step 5  POST /v1/vonras/call-sessions/{callId}/notify-event  (ANSWER)
            ← tcore_rtp_port + taccess_rtp_port
Step 6  POST /v1/vonras/call-sessions/{callId}/ctrl-result
            → callbackUrl = http://127.0.0.1:9999 cho cả tcore + taccess
Step 7  Gửi 200 PCMU packets → tcore_rtp_port  (song song)
        Gửi 200 PCMU packets → taccess_rtp_port (song song)
Step 8  Poll log: chờ ≥2 final callback (1 tcore + 1 taccess)
Step 9  Phân tích callback payload
Step 10 DELETE /v1/vonras/call-sessions/{callId}
Step 11 Verify gateway metrics
```

### Kết quả kỳ vọng

```
[OK]    Binaries built ✓
[OK]    Gateway ready ✓
[OK]    Sessions created (HTTP 201) — {callId}-tcore + {callId}-taccess
[OK]    tcore_rtp_port   : 40099
[OK]    taccess_rtp_port : 40098
[OK]    ctrl-result OK — callbackUrl set cho tcore + taccess ✓
[OK]    tCore   sender done ✓
[OK]    tAccess sender done ✓
[OK]    Nhận đủ 2 final callback sau Xs ✓
[OK]    Pipeline: +400/400 jobs ✓
[OK]    Dispatcher sent: +6 ✓
[OK]    Callback errors: 0 ✓
[OK]    AI recv errors: 0 ✓
  RESULT: PASS
```

### Output callback (Step 9)

```
[partial] seq=1  sid={callId}-tcore
  text           = "xin chào..."
  mediaResources = {"tCore": {"contextId": "ctx-tcore-001", "terminationId": "term-core-001"}}

[FINAL  ] seq=3  sid={callId}-tcore
  text           = "Xin chào, tôi cần hỗ trợ."
  mediaResources = {"tCore": {"contextId": "ctx-tcore-001", "terminationId": "term-core-001"}}

[partial] seq=1  sid={callId}-taccess
  text           = "xin chào..."
  mediaResources = {"tAccess": {"contextId": "ctx-taccess-001", "terminationId": "term-access-001"}}

[FINAL  ] seq=3  sid={callId}-taccess
  text           = "Xin chào, tôi cần hỗ trợ."
  mediaResources = {"tAccess": {"contextId": "ctx-taccess-001", "terminationId": "term-access-001"}}
```

Lưu ý `mediaResources`: mỗi session chỉ có đúng 1 side (tCore **hoặc** tAccess), side còn lại bị omit.

---

## Bước 4 — Test với AI thật (test-realai-e2e.sh)

Đọc file G.711 thật 60s, gửi song song vào 2 sessions, chờ transcript từ AI gRPC thật.

### Yêu cầu

- Binaries đã build sẵn trong `./bin/` (xem Bước 1)
- AI gRPC server đang chạy và kết nối được
- File `data/generated/g711/speech.pcmu` tồn tại

### Tham số (env)

| Biến | Mặc định | Ý nghĩa |
|---|---|---|
| `AI_ADDR` | `127.0.0.1:50051` | Địa chỉ AI gRPC server |
| `GW_PORT` | `8080` | Cổng gateway HTTP |
| `CALLBACK_PORT` | `9999` | Cổng mock-callback-server |
| `EXPECT_FINAL` | `2` | Số final tối thiểu để PASS (≥1 tcore + ≥1 taccess) |
| `TIMEOUT` | `120` | Timeout chờ callback (giây) |

### Chạy

```bash
cd ~/media-ai

# AI worker cùng máy (mock hoặc thật)
bash scripts/test-realai-e2e.sh

# AI worker trên máy khác
AI_ADDR=192.168.1.10:50051 bash scripts/test-realai-e2e.sh

# Tùy chỉnh expect + timeout (AI xử lý chậm hoặc nhiều kết quả)
AI_ADDR=192.168.1.10:50051 EXPECT_FINAL=10 TIMEOUT=180 bash scripts/test-realai-e2e.sh
```

### Luồng script

```
Step 1  Preflight: kiểm tra binaries, file RTP, TCP reachable tới AI
Step 2  Sinh gateway config với ai.grpc_target = AI_ADDR  (vào file temp)
Step 3  Dựng mock-callback-server :CALLBACK_PORT  (daemon mode — không tự thoát)
Step 4  Start gateway với config temp
Step 5  Metrics baseline
Step 6  notify-event ANSWER → tcore_rtp_port + taccess_rtp_port
Step 7  ctrl-result → callbackUrl per-termination
Step 8  Gửi data/generated/g711/speech.pcmu song song:
            3000 packets × 20ms = 60s  → tcore_rtp_port   (SSRC=77001)
            3000 packets × 20ms = 60s  → taccess_rtp_port  (SSRC=77002)
Step 9  Poll log: chờ EXPECT_FINAL final (timeout TIMEOUT giây)
        Kill callback server sau khi đủ
Step 10 Phân tích toàn bộ callback payload
Step 11 DELETE session
Step 12 Verify metrics
```

### Kết quả kỳ vọng (mock-ai-worker)

```
[OK]    Binaries ✓  RTP file: 480044 bytes → 3000 frames = 60s
[OK]    AI gRPC: 127.0.0.1:50051 TCP reachable ✓
[OK]    Config written → /tmp/tmp.xxxx
[OK]    mock-callback-server PID=... → :9999 ✓  (daemon mode)
[OK]    Gateway ready ✓
[OK]    tcore_rtp_port   : 40099
[OK]    taccess_rtp_port : 40098
[OK]    ctrl-result OK ✓
[OK]    tCore   sender done ✓  (3000 packets = 60s)
[OK]    tAccess sender done ✓  (3000 packets = 60s)
[OK]    Nhận đủ 40 final callback sau Xs ✓
[OK]    Pipeline: +6000/6000 jobs ✓
[OK]    Dispatcher sent: +120 ✓
[OK]    Callback errors: 0 ✓
[OK]    AI send errors: 0 ✓
[OK]    AI recv errors: 0 ✓
  RESULT: PASS
  G.711 60s → AI thật → HTTP/2 callback pipeline hoạt động đúng ✓
```

Pipeline math với mock-ai-worker:

```
3000 packets × 20ms = 60s audio   (mỗi stream)
60s / 500ms = 120 AudioChunks     (mỗi stream)
final mỗi 6 chunks → 20 final     (mỗi stream)
2 streams × 20 final = 40 final   (tổng)
2 streams × 60 result = 120 sent  (tổng partial + final)
```

### Tại sao mock-callback-server chạy daemon mode

Gateway gửi callback liên tục trong suốt 60s audio. Nếu server thoát sớm (sau khi nhận đủ N final):
- Gateway POST tiếp → connection refused → `dispatcher_send_errors` tăng giả
- Test FAIL dù pipeline hoàn toàn đúng

Daemon mode (`--expect-final 0`) giữ server mở suốt test; script poll log thủ công rồi kill sau khi đủ final.

---

## Bước 5 — Kiểm tra trạng thái kết nối

```bash
curl -s http://127.0.0.1:8080/v1/connections | python3 -m json.tool
```

```json
{
  "ai_workers": [{
    "addr": "127.0.0.1:50051",
    "state": "READY",
    "active_streams": 2,
    "latency": { "last_ms": 85, "avg_ms": 90, "count": 40 }
  }],
  "callback": {
    "url": "http://127.0.0.1:9999/",
    "connected": true,
    "preconnect_at": "2026-07-03T10:09:53Z"
  },
  "rtp": {
    "per_session_open": 2,
    "per_session_capacity": 100,
    "shared_ingress": false
  }
}
```

| Field | Bình thường | Cảnh báo |
|---|---|---|
| `ai_workers[0].state` | `READY` | `CONNECTING` → worker chưa accept; `TRANSIENT_FAILURE` → sai port |
| `callback.connected` | `true` | `false` → callback server chưa chạy khi gateway start (không ảnh hưởng hoạt động) |
| `rtp.per_session_open` | N (số session đang giữ port) | Bằng 0 sau DELETE → port đã được giải phóng |

---

## Bước 6 — Test thủ công DCSF API

### notify-event ANSWER

```bash
CALL_ID="test-call-$(date +%s)"
GW="http://127.0.0.1:8080"

curl -s -X POST "${GW}/v1/vonras/call-sessions/${CALL_ID}/notify-event" \
  -H "Content-Type: application/json" \
  -d '{
    "callId": "'"${CALL_ID}"'",
    "event": "ANSWER",
    "selectedService": "speech_to_text",
    "direction": "MT",
    "role": "terminator",
    "bearerCapability": "AUDIO"
  }' | python3 -m json.tool
```

Response (HTTP 201):

```json
{
  "session_id": "test-call-1783048171",
  "status": "CREATED",
  "source_type": "raw_rtp",
  "codec": "PCMU",
  "sample_rate": 8000,
  "channels": 1,
  "task": "speech_to_text",
  "gateway_id": "gw-mock",
  "rtp_ip": "127.0.0.1",
  "tcore_rtp_port": 40099,
  "taccess_rtp_port": 40098,
  "tcore_local_non_dc_media":  { "sdpmLine": "audio 40099 RTP/AVP 0", "sdpaLines": ["rtpmap:0 PCMU/8000", "ptime:20", "maxptime:20", "recvonly"] },
  "taccess_local_non_dc_media": { "sdpmLine": "audio 40098 RTP/AVP 0", "sdpaLines": ["rtpmap:0 PCMU/8000", "ptime:20", "maxptime:20", "recvonly"] }
}
```

### ctrl-result (set callbackUrl + H.248 identity)

```bash
curl -s -X POST "${GW}/v1/vonras/call-sessions/${CALL_ID}/ctrl-result" \
  -H "Content-Type: application/json" \
  -d '{
    "callId": "'"${CALL_ID}"'",
    "mediaResources": {
      "tCore": {
        "contextId": "ctx-core-001",
        "termination": { "terminationId": "term-core-001" },
        "callbackUrl": "http://127.0.0.1:9999"
      },
      "tAccess": {
        "contextId": "ctx-access-001",
        "termination": { "terminationId": "term-access-001" },
        "callbackUrl": "http://127.0.0.1:9999"
      }
    }
  }'
```

### Gửi RTP thử

```bash
# Lấy port từ response notify-event
TCORE_PORT=40099
TACCESS_PORT=40098

# Gửi 200 synthetic PCMU packets → tcore
python3 -c "import sys; sys.stdout.buffer.write(bytes([0x7F]*160)*200)" > /tmp/test.pcmu

./bin/mock-rtp-sender \
    --codec PCMU --pt 0 --ssrc 77001 \
    --ptime 20 --sample-rate 8000 \
    --file-format raw --frame-size 160 --count 200 \
    --target "127.0.0.1:${TCORE_PORT}" \
    --file /tmp/test.pcmu

# Gửi song song sang taccess
./bin/mock-rtp-sender \
    --codec PCMU --pt 0 --ssrc 77002 \
    --ptime 20 --sample-rate 8000 \
    --file-format raw --frame-size 160 --count 200 \
    --target "127.0.0.1:${TACCESS_PORT}" \
    --file /tmp/test.pcmu

rm /tmp/test.pcmu
```

### Xóa session

```bash
curl -s -o /dev/null -w "%{http_code}" \
  -X DELETE "${GW}/v1/vonras/call-sessions/${CALL_ID}"
# → 204
```

Sau DELETE, cả 2 port (tcore + taccess) được trả về pool bất đồng bộ (vài ms sau khi cancel context).

---

## Bước 7 — Xem metrics

```bash
curl -s http://127.0.0.1:8080/metrics | grep -E "^media_ai_(pool|dispatcher|ai_|rtp_|result_)" | sort
```

Các metric quan trọng:

| Metric | Ý nghĩa | Kỳ vọng sau test |
|---|---|---|
| `media_ai_pool_processed_total` | Packet đã decode | = số packet gửi (mỗi session) |
| `media_ai_pool_decode_errors_total` | Lỗi decode | 0 |
| `media_ai_dispatcher_sent_total` | Callback đã gửi thành công | > 0 |
| `media_ai_dispatcher_send_errors_total` | Callback lỗi | 0 (daemon mode) |
| `media_ai_ai_send_errors_total` | Lỗi gửi lên AI | 0 |
| `media_ai_ai_recv_errors_total` | Lỗi nhận từ AI | 0 |
| `media_ai_rtp_ports_available` | Port còn trống trong pool | Tăng lại sau DELETE |

---

## Bước 8 — Chạy unit tests

```bash
go test ./... 2>&1
```

Kết quả kỳ vọng:

```
ok  media-ai-gateway/internal/ai
ok  media-ai-gateway/internal/audio
ok  media-ai-gateway/internal/codec
ok  media-ai-gateway/internal/config
ok  media-ai-gateway/internal/controlplane
ok  media-ai-gateway/internal/coordinator
ok  media-ai-gateway/internal/ingress/rawrtp
ok  media-ai-gateway/internal/ingress/webrtc
ok  media-ai-gateway/internal/jitter
ok  media-ai-gateway/internal/pipeline
ok  media-ai-gateway/internal/result
ok  media-ai-gateway/internal/session
```

---

## Cấu hình gateway-mock.yaml

File `config/gateway-mock.yaml` dùng cho test local. Các field quan trọng:

```yaml
ai:
  grpc_target: "127.0.0.1:50051"   # mock-ai-worker hoặc AI thật

callback:
  url: "http://127.0.0.1:9999"     # pre-connect warm-up (không phải URL gửi kết quả)
                                    # URL thực tế đến từ ctrl-result per-termination

rtp:
  public_ip: "127.0.0.1"           # đổi thành IP lab server nếu gửi từ máy khác
  port_start: 40000
  port_end:   40099                 # pool 100 port = tối đa 50 cuộc gọi đồng thời

session:
  idle_timeout_sec: 60              # session tự đóng sau 60s không nhận packet
```

Khi dùng `test-realai-e2e.sh`: script **tự sinh config** với `ai.grpc_target = AI_ADDR`, không cần sửa `gateway-mock.yaml`.

---

## Troubleshooting

| Triệu chứng | Nguyên nhân | Cách fix |
|---|---|---|
| notify-event → HTTP 400 `unknown event` | Field `event` sai case | Dùng `"ANSWER"` (chữ hoa) |
| notify-event → HTTP 200 (không tạo session) | `selectedService` không map được | Dùng `"speech_to_text"` hoặc `"realtime_translation"` |
| notify-event → HTTP 409 Conflict | `callId` đã tồn tại | Dùng callId mới hoặc DELETE trước |
| notify-event → HTTP 503 `no RTP ports` | Pool cạn port | Tăng `port_end` hoặc DELETE session cũ |
| ctrl-result → HTTP 404 | Session không tồn tại (callId sai hoặc chưa ANSWER) | Kiểm tra callId, gọi notify-event trước |
| `dispatcher_send_errors > 0` | mock-callback-server tắt sớm | Dùng daemon mode (`--expect-final 0`) — đã fix trong `test-realai-e2e.sh` |
| `pool_processed < packets_sent` | Jitter buffer drop packet đến muộn | Bình thường nếu < 5%; kiểm tra UDP trên loopback |
| `ai_recv_errors > 0` + log `recv idle timeout` | AI worker không trả kết quả trong 30s | Kiểm tra GPU/CPU AI worker, tăng `stream_timeout_sec` |
| `ai_recv_errors > 0` + log `unexpected EOF` | AI worker đóng stream sớm | AI có idle timeout ngắn — cấu hình AI tăng session timeout |
| `rtp.per_session_open > 0` sau DELETE | Goroutine release port chưa chạy kịp | Bình thường — bất đồng bộ, port về pool sau vài ms |
| `callback.connected: false` | mock-callback-server chưa chạy khi gateway start | Không ảnh hưởng hoạt động; gateway tự connect khi có callback đầu tiên |
| `ai_workers[0].state: TRANSIENT_FAILURE` | mock-ai-worker không chạy hoặc sai port | Start mock-ai-worker trước; kiểm tra port 50051 |

### Log debug

```bash
# Gateway log realtime
tail -f /tmp/gateway-realai.log

# Chỉ lỗi
grep "level=ERROR" /tmp/gateway-realai.log

# Đổi sang debug mode (sửa config rồi restart)
# log.level: "debug"
```

---

## Cleanup

```bash
pkill media-ai-gateway
pkill mock-ai-worker
pkill mock-callback-server

rm -f /tmp/gateway-realai.log /tmp/rtp-tcore-realai.log /tmp/rtp-taccess-realai.log
```
