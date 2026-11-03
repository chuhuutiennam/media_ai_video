# Media AI Gateway - Mô hình thiết kế và mô tả chức năng module

## 1. Mục tiêu thiết kế

Media AI Gateway là hệ thống trung gian nhận audio realtime từ nhiều nguồn media, chuẩn hóa audio, gửi sang module AI/STT như faster-whisper/PhoWhisper qua gRPC streaming, sau đó trả transcript/result về client hoặc backend.

Hệ thống hỗ trợ 2 nhóm nguồn vào chính:

- **Phase 1 - Raw RTP Ingress**: nhận RTP trực tiếp từ SBC, MGW, RTP Gateway, C/C++ Media Plane hoặc hệ thống telecom.
- **Phase 2 - WebRTC Ingress**: nhận audio từ browser, mobile app, web client hoặc WebRTC SDK thông qua Pion WebRTC.

Nguyên tắc thiết kế:

- Tách rõ **Control Plane** và **Data Plane**.
- Mỗi media session được pin vào một gateway cụ thể.
- Không xử lý nặng tại ingress.
- Không tạo goroutine không kiểm soát theo từng packet/result.
- Dùng **bounded queue**, **worker pool**, **backpressure**, **admission control**.
- Dùng chung audio pipeline cho cả Raw RTP và WebRTC.
- Tách Gateway và AI Worker để dễ scale độc lập.

---

## 2. Mô hình tổng thể

```text
                         +------------------------+
                         | Business / Call Service|
                         | Session Request        |
                         +-----------+------------+
                                     |
                                     v
                         +------------------------+
                         | Media Control Plane    |
                         | - Session allocation   |
                         | - Gateway selection    |
                         | - RTP port allocation  |
                         | - Load/health tracking |
                         +-----------+------------+
                                     |
              +----------------------+----------------------+
              |                                             |
              v                                             v
+-----------------------------+               +-----------------------------+
| RTP Gateway Pool            |               | WebRTC Gateway Pool         |
| - UDP RTP ingress           |               | - Pion WebRTC               |
| - pion/rtp                  |               | - SDP/ICE/DTLS/SRTP         |
| - jitter/codec/audio        |               | - DataChannel result        |
+--------------+--------------+               +--------------+--------------+
               |                                             |
               +----------------------+----------------------+
                                      |
                                      v
                         +------------------------+
                         | Common Audio Pipeline  |
                         | - jitter               |
                         | - decode               |
                         | - resample             |
                         | - chunk                |
                         +-----------+------------+
                                     |
                                     v
                         +------------------------+
                         | AI Stream Manager      |
                         | gRPC bidirectional     |
                         +-----------+------------+
                                     |
                                     v
                         +------------------------+
                         | AI Router              |
                         | Route by model/load    |
                         +-----------+------------+
                                     |
           +-------------------------+-------------------------+
           |                         |                         |
           v                         v                         v
+-------------------+     +-------------------+     +-------------------+
| AI Worker Pool A  |     | AI Worker Pool B  |     | AI Worker Pool C  |
| faster-whisper    |     | PhoWhisper        |     | Translation       |
| GPU/CPU           |     | GPU/CPU           |     | MarianMT/etc.     |
+---------+---------+     +---------+---------+     +---------+---------+
          |                         |                         |
          +-------------------------+-------------------------+
                                    |
                                    v
                         +------------------------+
                         | Result Dispatcher      |
                         | - DataChannel          |
                         | - WebSocket/SSE        |
                         | - HTTP Callback        |
                         | - Kafka/Redis Stream   |
                         +------------------------+
```

---

## 3. Phân lớp kiến trúc

| Layer | Vai trò chính | Module chính |
|---|---|---|
| Control Plane | Tạo session, chọn gateway, cấp port, tracking load | Session API, Gateway Registry, Port Allocator, Admission Controller |
| Ingress Layer | Nhận media từ bên ngoài | Raw RTP Ingress, WebRTC Ingress |
| Session Layer | Quản lý lifecycle và mapping session | Session Manager, Session Router, Session State Store |
| Media Pipeline | Xử lý audio realtime | Jitter Buffer, Codec Decoder, Resampler, Chunker |
| AI Integration | Gửi audio sang AI và nhận transcript | AI Stream Manager, AI Router, AI gRPC Client |
| Result Layer | Trả kết quả về client/backend | Result Dispatcher, DataChannel Sink, WebSocket Sink, HTTP Callback Sink, Kafka Sink |
| Runtime/Platform | Vận hành và quan sát hệ thống | Metrics, Health Check, Config, Worker Pool, Backpressure Controller |

---

## 4. Luồng xử lý end-to-end

### 4.1 Luồng Raw RTP Phase 1

```text
DCSF
   |
   | POST /v1/vonras/call-sessions/{callId}/notify-event  (event: ANSWER, selectedService: speech_to_text)
   v
Media Control Plane (DCAS)
   |
   | Admission check:
   |   session_capacity   — sessions ≥ 90% max          → 503, retry 5s
   |   queue_pressure     — audio job queue ≥ 80%        → 503, retry 1s
   |   ai_stream_capacity — AI streams ≥ 95% max         → 503, retry 2s
   |   no_ai_worker       — WorkerRegistry rỗng          → 503, retry 3s
   |   memory_pressure    — heap ≥ memThreshold           → 503, retry 5s
   |   port_exhausted     — portAlloc.Available() == 0   → 503, retry 10s
   | sessionMgr.Create({callId}-tcore)  → đăng ký 2 session nội bộ
   | sessionMgr.Create({callId}-taccess)
   | Allocate port1 (tcore) + port2 (taccess) từ per-session port pool
   | StartSessionListener(tcore, port1) → bind UDP socket
   | StartSessionListener(taccess, port2) → bind UDP socket
   | coord.Start(sessTCore) + coord.Start(sessAccess) → khởi động 2 pipeline
   v
   | trả { rtp_ip, tcore_rtp_port, taccess_rtp_port, tcore_local_non_dc_media, taccess_local_non_dc_media }
   v
MF gửi tCore RTP → port1, tAccess RTP → port2
   |
   v
Raw RTP Ingress
   | Chế độ A — Shared ingress (:5004):
   |   SSRC != 0 → RouteBySSRC  (ssrcIndex)
   |   SSRC == 0 → RouteByAddr  (addrIndex, cần remote_addr trong CREATE)
   | Chế độ B — Per-session listener (<port>): direct → PacketQueue
   v
PacketQueue (per-session, bounded)
   |
   v
Jitter Buffer  (reorder, drop late packets)
   |
   v
Worker Pool  (decode → resample → chunk)
   | Codec Decoder  (PCMU / PCMA / Opus)
   | Audio Normalizer  (→ PCM 16kHz mono)
   | Audio Chunker  (→ 500 ms AudioChunk)
   v
AudioQueue (per-session, bounded)
   |
   v
AI Stream Manager
   | WorkerRegistry.Select(language, task) → chọn worker ít tải nhất
   | RoutingDialer.Dial → SharedConnPool.getOrCreate(addr) + conn.NewStream
   | Encoding: protobuf binary  (/speech.SpeechStream/Recognize)
   | 1 shared grpc.ClientConn per worker addr — tất cả session multiplex HTTP/2
   | Keepalive PING mỗi 30s — giữ conn sống qua đoạn im lặng / firewall NAT
   | Reconnect tự động khi stream lỗi (max_retry lần, exponential backoff)
   v
AI Worker  (faster-whisper / PhoWhisper / translation)
   |
   v
ResultQueue (per-session, bounded)
   |
   v
Result Dispatcher
   |
   v
HTTP Callback Sink  (POST callback_url, retry giới hạn)
```

> **Hai chế độ RTP ingress:**
> - **Shared** (`rtp.listen_addr`, luôn bật): dùng SSRC → remote addr để route; phù hợp LAN đơn giản.
> - **Per-session** (`rtp.port_start/end`, khi cấu hình): mỗi session được cấp port riêng, port tự release khi session đóng.
>
> **Routing tại shared ingress:** `SSRC != 0` → `ssrcIndex`; `SSRC == 0` → `addrIndex`. Nếu `remote_addr` không được cung cấp khi tạo session, `addrIndex` rỗng và packet có `SSRC=0` sẽ bị drop (`DroppedUnknownSSRC`). Dùng per-session listener để tránh cần `remote_addr`.

### 4.2 Luồng WebRTC Phase 2

```text
Browser/Mobile Client
   |
   | POST /v1/webrtc/offer  (SDP Offer — session provisioning TBD)
   v
Control Plane / WebRTC Gateway
   |
   | newPeerSession → PeerConnection (recvonly) + DataChannel "transcript"
   | DataChannelSink đăng ký vào Dispatcher
   | ICE gathering inline → SDP Answer đầy đủ
   v
   | { sdp, udp_proxy_port? }
   v
Client thiết lập PeerConnection
   |
   | Audio Track RTP qua DTLS-SRTP
   v
Pion WebRTC Ingress
   | OnTrack → readRTPLoop → MediaPacket → PacketQueue
   v
PacketQueue → Jitter Buffer → Worker Pool → AudioQueue
   |
   v
AI Stream Manager  (same path as Raw RTP)
   |
   v
AI Worker
   |
   v
Result Dispatcher
   |
   +→ DataChannelSink  → WebRTC DataChannel "transcript" → UE
   +→ HTTP Callback Sink  → backend
```

---

## 5. Mô tả chi tiết từng module

## 5.1 Media Control Plane

### Vai trò

Media Control Plane là bộ điều phối session và tài nguyên. Module này không xử lý packet RTP/audio trực tiếp, mà quyết định session được tạo ở đâu và đi vào gateway nào.

### Chức năng

- Nhận sự kiện cuộc gọi từ DCSF (BEGIN/ANSWER) và tạo **2 session** khi cần: `{callId}-tcore` (luồng core) và `{callId}-taccess` (luồng subscriber).
- Cấp **2 RTP port** (một per stream) từ per-session port pool.
- Trả `tcore_local_non_dc_media` và `taccess_local_non_dc_media` (SDP endpoint) để DCSF forward cho MF.
- Cập nhật `contextId`, `terminationId`, per-termination `callbackUrl` khi nhận ctrl-result từ DCSF.
- Theo dõi health/load và từ chối session mới nếu hệ thống quá tải.

### Input — notify-event (ANSWER)

```json
{
  "callId": "p2uc31@[FC00:0DB8::]",
  "event": "ANSWER",
  "selectedService": "speech_to_text",
  "direction": "MT",
  "role": "terminator",
  "bearerCapability": "VIDEO",
  "calling": "86156****5398",
  "called": "86156****5399",
  "callbackUrl": "http://dcsf.ims.internal:9090/v1/dcsf/call-sessions/p2uc31%40%5BFC00%3A0DB8%3A%3A%5D/call-control"
}
```

> Codec được suy từ `selectedService`: `speech_to_text` / `realtime_translation` → PCMU/8000.  
> `ssrc` và `remote_addr` không có trong ANSWER body — dùng per-session port pool, remote_addr được set sau qua ctrl-result.  
> `callbackUrl`: URL DCSF cấp để DCAS POST CALL_CTRL lại — xem §5.1 "Outbound — CALL_CTRL đến DCSF".

### Outbound — CALL_CTRL đến DCSF

Sau khi tạo session thành công, DCAS POST CALL_CTRL đến `callbackUrl` nhận từ ANSWER:

```http
POST {callbackUrl từ ANSWER}
Content-Type: application/json
```

```json
{
  "callId": "p2uc31@[FC00:0DB8::]",
  "service_name": "speech_to_text",
  "actions": "duplicate",
  "callbackUrl": "http://10.10.1.5:8443/v1/vonras/call-sessions/p2uc31%40%5BFC00%3A0DB8%3A%3A%5D/ctrl-result"
}
```

> `callbackUrl` trong body CALL_CTRL là địa chỉ DCAS (cấu hình `server.public_url`) kèm path ctrl-result — để DCSF biết gửi kết quả SDP negotiation về đâu.  
> Timeout cấu hình bởi `dcsf.call_control_timeout_ms` (mặc định 30s — DCSF block chờ CALL_RESULT từ IMS-AS trước khi respond).

### Input — ctrl-result

```json
{
  "callId": "p2uc31@[FC00:0DB8::]",
  "mediaResources": {
    "tCore": {
      "contextId": "ctx-001",
      "termination": { "terminationId": "term-core-001", "medias": [...] },
      "callbackUrl": "http://dcsf.example.com/callback/tcore"
    },
    "tAccess": {
      "contextId": "ctx-002",
      "termination": { "terminationId": "term-access-001", "medias": [...] },
      "callbackUrl": "http://dcsf.example.com/callback/taccess"
    }
  }
}
```

> Cấu trúc `mediaResources` theo 3GPP MRM (TS29.176): mỗi termination có `contextId` + `termination.{terminationId, medias}` + `callbackUrl`.  
> `contextId` và `terminationId` được lưu vào session để enrich HTTP callback payload.  
> `callbackUrl` per-termination được gán vào `sess.CallbackURL` — coordinator tạo `HTTPCallbackSink` khi giá trị này không rỗng.  
> `medias` là `[]MediaInfo` theo 3GPP TS29.176 — được chấp nhận nhưng không parse (forward-compatible).

### Output

```json
{
  "session_id": "call-001",
  "gateway_id": "gw-02",
  "rtp_ip": "10.10.10.22",
  "tcore_rtp_port": 40028,
  "taccess_rtp_port": 40029,
  "status": "CREATED",
  "source_type": "raw_rtp",
  "codec": "PCMU",
  "sample_rate": 8000,
  "channels": 1,
  "tcore_local_non_dc_media": {
    "sdpmLine": "audio 40028 RTP/AVP 0",
    "sdpaLines": ["rtpmap:0 PCMU/8000", "ptime:20", "maxptime:20", "recvonly"]
  },
  "taccess_local_non_dc_media": {
    "sdpmLine": "audio 40029 RTP/AVP 0",
    "sdpaLines": ["rtpmap:0 PCMU/8000", "ptime:20", "maxptime:20", "recvonly"]
  }
}
```

> Mỗi call tạo **2 internal session**: `{callId}-tcore` và `{callId}-taccess` — mỗi cái 1 UDP port riêng biệt.  
> `tcore_local_non_dc_media` / `taccess_local_non_dc_media` chỉ có khi `rtp.port_start > 0`.  
> DCAS dùng 2 field này làm `remoteNonDcMedia` riêng cho tCore và tAccess termination khi gọi MF MRM `POST /contexts` (3GPP TS29.176).  
> `session_id` trong response luôn là `callId` (không phải internal ID `callId-tcore`/`callId-taccess`).

### State quản lý

- Gateway registry.
- Session mapping.
- Port allocation table.
- Gateway capacity/load.
- Session status.

### HTTP Router & Middleware

**Router:** `github.com/gin-gonic/gin` v1.12.

```
gin.New()
  └─ gin.Recovery()      — recover panic, trả 500
  └─ ginLogger()         — log mỗi request bằng slog (xem bên dưới)
       └─ /v1/vonras/call-sessions/:callId/notify-event   POST
       └─ /v1/vonras/call-sessions/:callId/ctrl-result    POST
       └─ /v1/vonras/call-sessions/:callId                DELETE / GET
       └─ /v1/webrtc/offer                                POST
       └─ /v1/gateways/:id/heartbeat                      PUT
       └─ /v1/stats                                       GET
       └─ /health/live                                    GET
       └─ /health/ready                                   GET
       └─ /health                                         GET  (backward compat)
       └─ /metrics                                        GET
```

**Hai chế độ TLS:**

| Mode | Điều kiện | Cơ chế |
|------|-----------|--------|
| h2c (cleartext) | `cert_file` rỗng | `h2c.NewHandler(ginEngine, &http2.Server{})` |
| TLS + HTTP/2 | `cert_file` được cấu hình | `http2.ConfigureServer` + `ListenAndServeTLS` |

**`ginLogger` middleware** — `internal/controlplane/middleware.go`:

- Ghi log sau khi response hoàn thành (`c.Next()` trước).
- Fields: `method`, `path`, `status`, `latency_ms`, `ip`, `bytes`, `request_id` (nếu có header `X-Request-ID`), `error` (nếu handler gắn lỗi vào `c.Errors`).
- Level tự động: `DEBUG` cho 2xx/3xx · `WARN` cho 4xx · `ERROR` cho 5xx.
- Tích hợp với `log/slog` (JSON handler) — nhất quán với toàn bộ project.

**Metrics server riêng** — khi `metrics_addr != http_addr`, một `net/http.Server` tối giản chạy riêng chỉ phục vụ `GET /metrics`, không đi qua gin engine.

### Lưu ý thiết kế

- Nên chạy HA ít nhất 2 replica.
- State có thể lưu Redis/PostgreSQL tùy mức độ production.
- Không đặt logic media processing trong Control Plane.

---

## 5.2 Gateway Registry

### Vai trò

Gateway Registry lưu danh sách gateway đang hoạt động và năng lực hiện tại của từng gateway.

### Chức năng

- Gateway đăng ký khi start.
- Gateway gửi heartbeat định kỳ.
- Lưu load metric: active sessions, queue usage, CPU, memory, packet drop.
- Đánh dấu gateway unhealthy khi mất heartbeat.
- Không route session mới vào gateway unhealthy.

### Dữ liệu mẫu

```json
{
  "gateway_id": "gw-02",
  "node_ip": "10.10.10.22",
  "type": "rtp",
  "status": "healthy",
  "active_sessions": 3200,
  "max_sessions": 10000,
  "rtp_port_start": 40000,
  "rtp_port_end": 40999,
  "used_ports": 612,
  "cpu_usage": 0.61,
  "audio_queue_usage": 0.42,
  "last_heartbeat_ms": 1710000000000
}
```

---

## 5.3 Admission Controller

### Vai trò

Admission Controller quyết định có nhận session mới hay không.

### Chức năng

- Kiểm tra gateway còn session slot không.
- Kiểm tra còn RTP port không.
- Kiểm tra AI Worker Pool còn capacity không.
- Kiểm tra queue có đang quá tải không.
- Trả lỗi rõ ràng nếu quá tải.

### Điều kiện reject ví dụ

```text
active_sessions > 90% max_sessions
available_rtp_ports = 0
audio_job_queue_usage > 80%
ai_active_streams > 95% capacity
packet_drop_rate tăng bất thường
```

### Response khi quá tải

```json
{
  "error": "capacity_exceeded",
  "message": "No available gateway capacity",
  "retry_after_ms": 5000
}
```

---

## 5.4 RTP Port Allocator

### Vai trò

Quản lý pool UDP port cho per-session Raw RTP listener trên một gateway node. Mỗi gateway node có một `PortAllocator` riêng — không có cross-gateway coordination.

### Cơ chế hoạt động

Free-list (stack): `Acquire` = pop, `Release` = push. Cả hai O(1), goroutine-safe bằng `sync.Mutex`.

```text
[portStart .. portEnd]  →  free-list khởi tạo đầy đủ
Acquire()  →  pop port từ đỉnh stack  →  trả port cho caller
Release(port)  →  push port trở lại stack  →  sẵn sàng cấp lại
```

`Release` được gọi qua `defer` trong `StartSessionListener` goroutine — port tự động trả về pool khi session đóng (`sess.Ctx.Done()`).

### API

```go
NewPortAllocator(portStart, portEnd int) (*PortAllocator, error)
// Lỗi nếu portStart <= 0 hoặc portEnd < portStart.
// NewServer() panic nếu range không hợp lệ.

Acquire() (int, error)
// Trả ErrNoPortsAvailable khi pool cạn.
// Handler createSession trả 503 retry_after=10s.

Release(port int)
// Implements rawrtp.PortReleaser — tránh circular import.

Available() int   // số port còn rảnh — dùng bởi AdmissionController (port_exhausted)
Total() int       // tổng port trong range — dùng bởi /metrics (rtp_ports_total)
UsagePct() float64  // used/total — dùng bởi selfInfo() snapshot
```

### Integration points

| Caller | Method | Mục đích |
|--------|--------|----------|
| `controlplane.NewServer` | `NewPortAllocator` | Tạo khi `RTPPortStart > 0`; panic nếu range lỗi |
| `createSession` handler | `Acquire` | Lấy port trước `StartSessionListener` |
| `StartSessionListener` goroutine | `Release` (qua defer) | Trả port khi session đóng |
| `AdmissionController` | `Available() == 0` | Từ chối session mới khi hết port (`port_exhausted`) |
| `/metrics` handler | `Available`, `Total` | Xuất `rtp_ports_available`, `rtp_ports_total` |
| `selfInfo()` | `UsagePct` | Gateway registry snapshot |

### Cấu hình mẫu

```yaml
rtp:
  port_start: 40000   # gateway A
  port_end:   40999   # 1000 port = 1000 session đồng thời tối đa
```

Mỗi gateway nên dùng range riêng biệt để tránh xung đột nếu chạy nhiều node trên cùng host.

### Lưu ý

Source (SBC/MGW) phải gửi RTP đúng tới `rtp_ip:rtp_port` trả về trong CREATE response. Không dùng random UDP load balancing — mỗi port gắn với một session cụ thể.

---

## 5.5 Raw RTP Ingress Module

### Vai trò

Nhận RTP packet trực tiếp qua UDP từ SBC/MGW/C++ Media Plane. Hoạt động theo hai chế độ độc lập — cả hai có thể bật đồng thời.

### Hai chế độ ingress

| | Shared ingress | Per-session listener |
|---|---|---|
| Socket | 1 UDP socket dùng chung (`:5004`) | 1 UDP socket riêng / session |
| Bật khi | Luôn bật | `rtp.port_start > 0` |
| Goroutine | 1 goroutine toàn cục | 1 goroutine / session |
| Routing | `SessionRouter` (RouteBySSRC → RouteByAddr) | Push thẳng vào `sess.PacketQueue` — không qua router |
| Port release | Không cần | `defer releaser.Release(port)` khi goroutine thoát |

### RTP Parser

Dùng `github.com/pion/rtp` (`Header.Unmarshal`). Parse theo RFC 3550 §5.1:

```text
Fixed header  12 bytes
CSRC list     CSRCCount × 4 bytes
Extension     4 + extWords×4 bytes  (nếu Extension bit = 1)
Payload       phần còn lại
```

pion/rtp không validate version field — gateway tự kiểm tra `hdr.Version != 2` sau `Unmarshal` và drop nếu sai. Packet bị drop tăng counter `DroppedParseError`.

### Chức năng

- Đọc datagram liên tục, không xử lý nặng tại read loop.
- SO_RCVBUF = `ReadBufferBytes` (default **2 MiB**) — cả shared lẫn per-session.
- Parse RTP header → lấy SSRC, PayloadType, Sequence, Timestamp, Marker, payload offset.
- Route packet → `PacketQueue` (xem bảng trên).
- Drop silently nếu parse lỗi hoặc không tìm được session; tăng counter tương ứng.
- Không tạo goroutine theo từng packet.

### Output

```go
MediaPacket{
    SessionID:    "call-001",
    SourceType:   "raw_rtp",
    SSRC:         123456,
    PayloadType:  0,
    Sequence:     1001,
    Timestamp:    96000,
    Marker:       false,
    Payload:      []byte{...},
    ReceivedAtMs: 1718000000000,  // time.Now().UnixMilli()
    Codec:        "PCMU",
    SampleRate:   8000,
    Channels:     1,
}
```

### Metrics (IngressStats)

```text
rtp_packets_total           — tổng datagram nhận được
rtp_packets_routed_total    — packet đưa vào queue thành công
rtp_parse_errors_total      — datagram lỗi header (ErrTooShort / ErrUnsupportedVersion)
rtp_unknown_ssrc_total      — không tìm được session (SSRC + addr đều miss)
rtp_queue_dropped_total     — session PacketQueue đầy
```

Tất cả tracked bằng `atomic.Uint64`, đọc qua `ingress.Stats()` → handler `/metrics`.

### Quá tải

Nếu `PacketQueue` đầy: packet bị drop (`default` branch trong `select`), tăng `DroppedQueueFull`. Không có trạng thái "session degraded" — operator giám sát qua metric `rtp_queue_dropped_total`.

---

## 5.6 WebRTC Ingress Module

### Vai trò

Nhận audio từ browser/mobile/web client thông qua WebRTC.

### Chức năng

- Xử lý HTTP signaling Offer/Answer.
- Tạo Pion `PeerConnection` với transceiver `recvonly` (gateway chỉ nhận audio, không gửi).
- Cấu hình ICE/STUN/TURN, NAT 1:1, port range.
- Nhận audio track qua `OnTrack`; đọc RTP packet từ `TrackRemote.ReadRTP()`.
- Tạo `MediaPacket` và đẩy vào Session Router.
- Tạo DataChannel `transcript` trong `newPeerSession()` để gửi AI transcript ngược về UE.
- Hỗ trợ hai chế độ proxy DataChannel (IMS AC.6): HTTP Proxy và UDP Proxy.
- ICE gathering hoàn tất inline — SDP answer trả về đã đầy đủ candidate (không cần trickle ICE).

### Input

```text
SDP Offer (HTTP POST /v1/webrtc/offer)
WebRTC audio track (SRTP/DTLS)
DataChannel messages từ UE (SCTP/DTLS)
```

### Output

```text
MediaPacket → Session.PacketQueue
DataChannel "transcript" → UE
```

### DataChannel "transcript"

DataChannel tên `transcript` được tạo trước khi `SetRemoteDescription` và được truyền cho `DataChannelSink` để dispatcher gửi AI transcript về UE. Đây là chiều **Gateway → UE**.

Chiều **UE → Gateway** được xử lý qua một trong hai chế độ proxy bên dưới.

### IMS Data Channel Proxy Modes (3GPP TS 23.228 Annex AC.6)

#### Mode 2.1 — HTTP Proxy (`dc_proxy_url`)

```text
UE → SCTP/DTLS DataChannel → Gateway → HTTP POST → DC Application Server
```

- Mỗi message nhận từ UE trên DataChannel được forward thành HTTP POST tới `dc_proxy_url`.
- Header `X-Session-ID` gắn vào mỗi request để App Server correlate.
- `Content-Type: application/json` khi message là text; `application/octet-stream` khi binary.
- Forward chạy trong goroutine riêng, không block OnMessage callback.

#### Mode 2.2 — UDP Proxy (`dc_udp_proxy_addr`) — AC.6-2

```text
UE ↔ SCTP/DTLS DataChannel ↔ Gateway UDP socket ↔ DC Application Server
```

- Gateway bind một UDP socket ngẫu nhiên (OS-assigned port).
- **UE → App Server**: `OnMessage` forward datagram tới `dc_udp_proxy_addr` (host:port của App Server).
- **App Server → UE**: goroutine `run()` đọc UDP từ socket, gửi về UE qua `DataChannel.Send()`.
- `run()` goroutine chỉ khởi động trong `dc.OnOpen` (cần channel đã Open mới gọi Send được).
- `LocalPort()` trả về UDP port gateway đang lắng nghe — gắn vào `AnswerResponse.udp_proxy_port` để App Server biết địa chỉ gửi về.
- Mode 2.2 **ưu tiên** hơn Mode 2.1 nếu cả hai được cấu hình.

### API

```http
POST /v1/webrtc/offer
```

Request:

```json
{
  "session_id": "web-001",
  "type": "offer",
  "sdp": "v=0\r\n..."
}
```

Response (không dùng UDP Proxy):

```json
{
  "session_id": "web-001",
  "type": "answer",
  "sdp": "v=0\r\n..."
}
```

Response (UDP Proxy Mode 2.2 bật):

```json
{
  "session_id": "web-001",
  "type": "answer",
  "sdp": "v=0\r\n...",
  "udp_proxy_port": 54321
}
```

`udp_proxy_port` chỉ xuất hiện khi `dc_udp_proxy_addr` được cấu hình. App Server hoặc orchestration layer dùng field này để biết phải gửi UDP datagram tới `gateway_ip:udp_proxy_port`.

### Config

```yaml
webrtc:
  stun_servers:
    - "stun:stun.l.google.com:19302"
  turn_servers: []
  nat1to1_ips: []       # public IP để override ICE candidate (K8s NodePort, cloud VM)
  ice_port_min: 0       # giới hạn port ICE (0 = không giới hạn)
  ice_port_max: 0

  # IMS Data Channel Mode 2.1 (HTTP Proxy) — mutually exclusive với dc_udp_proxy_addr
  dc_proxy_url: ""      # ví dụ: "http://dc-app-server:8090/dc/messages"

  # IMS Data Channel Mode 2.2 (UDP Proxy, AC.6-2) — ưu tiên hơn dc_proxy_url
  dc_udp_proxy_addr: "" # ví dụ: "dc-app-server:9000"
```

### Lưu ý

- WebRTC audio thường là Opus 48kHz stereo; cần resample về PCM 16kHz mono trước khi gửi AI.
- Cần session affinity cho signaling và PeerConnection (không thể load-balance stateless).
- `OnConnectionStateChange → Failed/Closed/Disconnected` cancel context, dọn dẹp PeerSession.
- UDP Proxy socket bị đóng khi context cancel, `ReadFromUDP` unblock và goroutine `run()` thoát.

---

## 5.7 Session Manager

### Vai trò

Quản lý toàn bộ lifecycle của media session.

### Chức năng

- Tạo session khi Control Plane yêu cầu.
- Tìm session theo `session_id`, `SSRC`, `peer_id`, `track_id`.
- Cleanup session khi timeout hoặc disconnect.
- Quản lý per-session queues.
- Giữ metadata codec/sample rate/channel/language/task.
- Giữ result sink: callback URL, WebSocket, DataChannel, Kafka topic.

### Session state mẫu

```go
type Session struct {
    ID          string
    SourceType  string // raw_rtp, webrtc

    SSRC        uint32
    PayloadType uint8
    Codec       string
    SampleRate  int
    Channels    int

    PacketQueue chan MediaPacket
    AudioQueue  chan AudioChunk
    ResultQueue chan RecognitionResult

    CallbackURL string
    PeerID      string
    TrackID     string

    CreatedAt    time.Time
    LastPacketAt time.Time
    Status       string
}
```

### Lifecycle

```text
CREATED -> ACTIVE -> IDLE -> CLOSING -> CLOSED
```

### Timeout đề xuất

```yaml
session:
  idle_timeout_sec: 30
  max_sessions: 10000
  per_session_packet_queue: 128
```

---

## 5.8 Session Router

### Vai trò

Route packet vào đúng session queue.

### Chức năng

- Với Raw RTP: ưu tiên route theo SSRC; fallback sang remote addr khi SSRC=0 hoặc không match.
- Với WebRTC: map theo peer ID/track ID/SSRC.
- Drop packet nếu không xác định được session; tăng counter `DroppedUnknownSSRC`.
- Ghi metric packet unknown/session missing.

### Routing priority (Raw RTP shared ingress)

```text
1. SSRC != 0  → ssrcIndex[ssrc]         — đăng ký khi CREATE với ssrc != 0
2. SSRC == 0  → addrIndex[remoteAddr]   — đăng ký khi CREATE với remote_addr != ""
3. không match → drop (DroppedUnknownSSRC++)
```

> Per-session listener bỏ qua toàn bộ routing table trên — packet đến đúng socket là đến đúng session.

### Routing key (WebRTC)

```text
peer_id + track_id + ssrc
```

### Output

```text
MediaPacket -> Session.PacketQueue
```

---

## 5.9 Common MediaPacket Model

### Vai trò

Chuẩn hóa packet từ Raw RTP và WebRTC thành một model chung cho pipeline.

### Model

```go
type MediaPacket struct {
    SessionID    string
    SourceType   string // raw_rtp, webrtc

    SSRC         uint32
    PayloadType  uint8
    Sequence     uint16
    Timestamp    uint32
    Marker       bool

    Payload      []byte
    ReceivedAtMs int64

    Codec        string
    SampleRate   int
    Channels     int
}
```

### Lợi ích

- Ingress khác nhau nhưng pipeline xử lý chung.
- Dễ thêm nguồn mới như file replay, SIPREC, WHIP.
- Giảm coupling giữa network layer và audio/AI layer.

---

## 5.10 Jitter Buffer Module

### Vai trò

Ổn định luồng RTP trước khi decode.

### Chức năng

- Reorder packet theo sequence number.
- Phát hiện packet loss.
- Drop packet quá trễ.
- Tính jitter/loss metric.
- Đẩy payload theo thứ tự sang decoder.

### Input

```text
MediaPacket theo session
```

### Output

```text
Ordered RTP payload/frame
```

### Config đề xuất

```yaml
jitter:
  buffer_ms: 60
  max_late_ms: 120
  packet_time_ms: 20
```

### Chính sách quá tải

- Drop packet quá trễ.
- Không giữ packet vô hạn để chờ đủ sequence.
- Ưu tiên latency thấp hơn phục hồi đầy đủ.

---

## 5.11 Codec Decoder Module

### Vai trò

Chuyển RTP payload thành PCM raw audio.

### Chức năng

- Nhận payload đã qua jitter buffer.
- Depacketize nếu codec cần.
- Decode thành PCM.
- Báo lỗi decode nếu payload sai.

### Codec hỗ trợ theo phase

| Phase | Codec |
|---|---|
| Phase 1 đầu tiên | PCMU, PCMA |
| Phase 1 mở rộng | Opus |
| Phase 2 WebRTC | Opus, PCMU, PCMA |
| Telecom nâng cao | AMR-WB, EVS |

### Output chuẩn

```text
PCM signed 16-bit
sample rate theo input codec
mono/stereo theo source
```

### Lưu ý

Pion giúp parse RTP/WebRTC, nhưng không thay thế toàn bộ codec decoder. Cần thư viện codec tương ứng cho Opus/AMR/EVS.

---

## 5.12 Audio Normalizer / Resampler

### Vai trò

Chuẩn hóa audio sang format AI cần.

### Chức năng

- Convert sample rate về 16kHz.
- Convert channel về mono.
- Convert sample format về PCM signed 16-bit little-endian.
- Normalize frame duration nếu cần.

### Input ví dụ

```text
PCMU 8kHz mono
Opus 48kHz mono/stereo
AMR-WB 16kHz
```

### Output chuẩn

```text
PCM S16LE
16000 Hz
mono
```

---

## 5.13 Audio Chunker

### Vai trò

Gom PCM frame nhỏ thành chunk phù hợp để gửi AI.

### Chức năng

- Gom nhiều frame 20ms/30ms thành chunk 500ms hoặc 1s.
- Gắn timestamp audio.
- Đảm bảo chunk không quá nhỏ để tránh overhead.
- Đảm bảo chunk không quá lớn để tránh latency.

### Config đề xuất

```yaml
audio:
  output_sample_rate: 16000
  output_channels: 1
  chunk_ms: 500
```

### Output

```go
type AudioChunk struct {
    SessionID   string
    StreamID    string
    PCM         []byte
    SampleRate  int
    Channels    int
    TimestampMs int64
    DurationMs  int64
}
```

---

## 5.14 Audio Pipeline Worker Pool

### Vai trò

Xử lý các tác vụ CPU-bound như jitter, decode, resample, chunk theo worker pool có giới hạn.

### Chức năng

- Nhận AudioJob từ session queue.
- Xử lý theo pipeline.
- Đưa AudioChunk sang AI Stream Manager.
- Không tạo goroutine theo từng packet.

### Model

```go
type AudioJob struct {
    SessionID string
    Packet    MediaPacket
}
```

### Config

```yaml
pipeline:
  audio_worker_count: 16
  audio_job_queue_size: 8192
```

### Quá tải

- Nếu audio job queue đầy: drop packet hoặc degrade session.
- Metric: `media_ai_pool_dropped_total`.

---

## 5.15 AI Stream Manager

### Vai trò

Quản lý gRPC stream giữa Gateway và AI service theo từng session.

### Chức năng

- Mỗi session có một gRPC bidirectional stream (mở qua `RoutingDialer`).
- Send `AudioChunk` sang AI qua send loop có bounded queue per-stream.
- Recv `RecognitionResult` từ AI qua recv loop; đẩy vào `sess.ResultQueue`.
- Reconnect tự động khi stream lỗi (exponential backoff, giới hạn `max_retry` lần).
- Enforce `max_active_streams` và `stream_timeout`.
- Backpressure recv: drop partial nếu `ResultQueue` đầy, block final.
- `first_chunk_timeout`: đóng stream nếu không nhận được AudioChunk nào sau khi mở (detect session treo trước khi gửi media).
- `recv_idle_timeout`: đóng stream nếu AI worker không trả result nào trong khoảng thời gian này (detect worker bị treo giữa chừng).

### Luồng

```text
AudioQueue (per-session)
   |
   v
per-stream send buffer  (bounded queue, drop khi đầy)
   |
   v
gRPC Send loop  → sendWithTimeout(SendTimeout)
   |
   v
AI Worker  (faster-whisper / PhoWhisper)
   |
   v
gRPC Recv loop
   |
   +- partial → ResultQueue (non-blocking, drop nếu đầy)
   +- final   → ResultQueue (blocking, đảm bảo delivery)
```

### Transport: protobuf binary

Gateway dùng protobuf binary encoding trực tiếp qua `protowire` — không cần code generation.

- Mỗi stream mở với `grpc.ForceCodec(protoCodec{})` → content-type: `application/grpc+proto`.
- `protoCodec.Marshal` encode `*audioChunkWire` → binary; `Unmarshal` decode binary → `*recognitionResultWire`.
- Field numbers theo `internal/ai/proto/speech.proto`; confidence dùng `Fixed32` (field 7).
- AI worker phải hỗ trợ content-type `application/grpc+proto` (chuẩn gRPC mặc định).

### Shared Connection Pool

`SharedConnPool` giữ một `*grpc.ClientConn` dùng chung per AI worker address:

```
Startup:
  SharedConnPool.Preconnect("ai-worker:50051")
    → grpc.NewClient(..., keepalive.ClientParameters{Time:30s, PermitWithoutStream:true})
    → conn.Connect()  (async TCP dial)
    → log "ai: gRPC connection initiated"

Per session (POST /v1/sessions):
  SharedConnPool.getOrCreate(addr) → reuse existing conn
  conn.NewStream(...)  → 1 grpc.ClientStream mới (HTTP/2 stream ID mới)
  (không tạo TCP connection mới, không đóng conn khi session kết thúc)
```

Lợi ích:
- **HTTP/2 multiplexing**: N session = N stream trên 1 TCP connection thay vì N connections.
- **Keepalive**: PING mỗi 30s (`keepalive_time_sec`), đóng conn nếu không PONG trong 10s (`keepalive_timeout_sec`). `PermitWithoutStream: true` đảm bảo PING được gửi ngay cả khi không có session active.
- **Pre-connect**: conn được thiết lập khi gateway khởi động, không chờ session đầu tiên.

### Proto contract

Định nghĩa đầy đủ tại `internal/ai/proto/speech.proto`. Tóm tắt:

```proto
service SpeechStream {
  rpc Recognize(stream AudioChunk) returns (stream RecognitionResult);
}

message AudioChunk {
  string session_id  = 1;
  string stream_id   = 2;
  bytes  pcm         = 3;   // PCM S16LE
  int32  sample_rate = 4;   // Hz (thường 16000)
  int32  channels    = 5;   // 1 = mono
  int64  timestamp_ms = 6;
  int64  duration_ms  = 7;
  bool   end_of_stream = 8;
  string language    = 9;   // BCP-47, e.g. "vi"
  string task        = 10;  // "transcribe" | "translate"
}

message RecognitionResult {
  string session_id = 1;
  string stream_id  = 2;
  string text       = 3;
  bool   is_final   = 4;
  int64  start_ms   = 5;
  int64  end_ms     = 6;
  float  confidence = 7;
  string language   = 8;
  uint64 seq        = 9;
}
```

### Wire format

`AudioChunk` được serialize theo field numbers trong `speech.proto`:

| Field | Number | Wire type | Ghi chú |
|-------|--------|-----------|---------|
| `session_id` | 1 | bytes | |
| `stream_id` | 2 | bytes | |
| `pcm` | 3 | bytes | PCM S16LE raw |
| `sample_rate` | 4 | varint | Hz |
| `channels` | 5 | varint | |
| `timestamp_ms` | 6 | varint | |
| `duration_ms` | 7 | varint | |
| `end_of_stream` | 8 | varint | bool |
| `language` | 9 | bytes | BCP-47 |
| `task` | 10 | bytes | "transcribe"\|"translate" |

`RecognitionResult` field 7 (`confidence`) dùng `fixed32` (IEEE 754 float32 little-endian).

### Config

```yaml
ai:
  grpc_target: "ai-router:50051"  # host:port của AI worker / AI router
  max_active_streams: 1000
  per_stream_queue_size: 20
  send_timeout_ms: 500
  stream_timeout_sec: 300
  max_retry: 3
  retry_backoff_ms: 1000
  keepalive_time_sec: 30          # gửi HTTP/2 PING tới AI worker sau N giây idle; 0 = tắt
  keepalive_timeout_sec: 10       # đóng conn nếu không nhận PONG trong N giây
  first_chunk_timeout_sec: 3      # đóng stream nếu không nhận chunk nào sau N giây; 0 = tắt
  recv_idle_timeout_sec: 30       # đóng stream nếu không nhận result nào trong N giây; 0 = tắt
```

### Latency Tracking

Manager tích lũy latency stats từ tất cả stream (đang chạy và đã đóng):

```go
type ManagerStats struct {
    TotalSendErrors uint64
    TotalRecvErrors uint64
    TotalRetries    uint64

    // End-to-result latency: time.Now() - result.end_ms (ms)
    LatencyCount uint64
    LatencySum   int64
    LatencyLast  int64

    // First-result latency: ms từ stream open đến result đầu tiên
    LatencyFirstCount uint64
    LatencyFirstSum   int64
}
```

- `AvgLatencyMs()` = `LatencySum / LatencyCount`
- `AvgFirstResultMs()` = `LatencyFirstSum / LatencyFirstCount`
- Chỉ tính khi AI worker set `end_ms` trong `RecognitionResult`.

---

## 5.16 AI Router

### Vai trò

Route stream từ Gateway tới AI worker phù hợp dựa trên capability và load.

### Chức năng

- `WorkerRegistry`: lưu danh sách AI worker với TTL-based expiry.
- `RoutingDialer`: chọn worker phù hợp rồi mở gRPC stream.
- Filter: loại worker không hỗ trợ language/task hoặc đã đạt `max_streams`.
- Select: worker có `loadScore` thấp nhất (stream ratio + GPU load).
- Trả `ErrNoWorkerAvailable` nếu không có worker nào đáp ứng.

### WorkerInfo

```go
type WorkerInfo struct {
    ID           string    // định danh duy nhất
    Addr         string    // gRPC address "host:port"
    Languages    []string  // ["*"] hoặc [] = hỗ trợ tất cả ngôn ngữ
    Tasks        []string  // ["transcribe","translate"]; ["*"] hoặc [] = tất cả
    Models       []string  // tên model, e.g. ["faster-whisper-medium"]
    MaxStreams    int       // giới hạn stream đồng thời; 0 = không giới hạn
    ActiveStreams int       // từ heartbeat gần nhất
    GPULoad      float64   // 0..1
    UpdatedAt    time.Time // thời điểm Register() gần nhất
}
```

### Load scoring

```text
loadScore = activeRatio × 0.7 + GPULoad × 0.3   (khi GPULoad > 0)
loadScore = activeRatio                            (khi GPULoad == 0, không tính GPU)
activeRatio = ActiveStreams / MaxStreams  (0 nếu MaxStreams == 0)

Worker bị loại khi:
  - ActiveStreams >= MaxStreams (atCapacity)
  - UpdatedAt + TTL < now  (stale)
  - không hỗ trợ language/task
```

### Routing criteria

```text
language = vi → chọn worker có "vi" hoặc "*" trong Languages
task = transcribe → worker hỗ trợ "transcribe"
GPULoad thấp + ActiveStreams thấp → ưu tiên worker đó
```

### Worker registration

Hai cách đăng ký AI worker vào `WorkerRegistry`:

**1. Static (config)** — khi khởi động Gateway:
```go
// main.go — tự động khi cfg.AI.GRPCTarget != ""
workerReg.Register(ai.WorkerInfo{
    ID:        "config:" + cfg.AI.GRPCTarget,
    Addr:      cfg.AI.GRPCTarget,
    MaxStreams: cfg.AI.MaxActiveStreams,
})
```
TTL = 0 → static worker không bao giờ expire.

**2. Dynamic (heartbeat)** — AI worker tự đăng ký định kỳ (TODO: implement endpoint):
```http
PUT /v1/ai-workers/{id}/heartbeat
```
```json
{
  "id": "ai-worker-01",
  "addr": "10.10.10.30:50051",
  "languages": ["vi", "en"],
  "tasks": ["transcribe"],
  "max_streams": 100,
  "active_streams": 42,
  "gpu_load": 0.61
}
```
TTL = 30s → worker stale nếu không heartbeat trong 30s.

### Tiện ích

- `Deregister(id string)` — xóa worker khỏi registry (dùng khi worker shutdown có chủ ý).
- `List() []WorkerInfo` — trả về tất cả worker còn fresh (trong TTL); dùng cho health API.
- `NullDialer` — Dialer giả lập cho dev/test: accept mọi `Send()`, block `Recv()` cho đến khi context cancel, không cần AI backend thật.

---

## 5.17 AI Worker Module

### Vai trò

Chạy mô hình AI/STT và trả transcript.

### Chức năng

- Nhận PCM chunk qua gRPC streaming.
- Buffer audio theo window phù hợp.
- Chạy STT bằng faster-whisper/PhoWhisper.
- Trả partial/final transcript.
- Quản lý model lifecycle.
- Theo dõi GPU/CPU/memory.

### Output

```json
{
  "session_id": "call-001",
  "stream_id": "ssrc-123456",
  "text": "xin chào tôi cần hỗ trợ",
  "is_final": true,
  "start_ms": 1000,
  "end_ms": 4200,
  "confidence": 0.91
}
```

### Lưu ý

faster-whisper gốc không phải streaming ASR realtime hoàn chỉnh. Cần thiết kế window/chunking hợp lý và có thể chấp nhận partial/final theo batch nhỏ.

---

## 5.18 Result Dispatcher

### Vai trò

Nhận RecognitionResult từ AI Stream Manager và gửi về đúng client/backend.

### Chức năng

- Route result theo `session_id`.
- Gửi qua DataChannel/WebSocket/SSE/HTTP Callback/Kafka.
- Dùng worker pool riêng.
- Drop partial khi client chậm nếu cần.
- Ưu tiên giữ final result.
- Retry callback giới hạn.

### Result model

```go
type RecognitionResult struct {
    SessionID  string  `json:"session_id"`
    StreamID   string  `json:"stream_id"`
    SourceType string  `json:"source_type"`
    Text       string  `json:"text"`
    IsFinal    bool    `json:"is_final"`
    StartMs    int64   `json:"start_ms"`
    EndMs      int64   `json:"end_ms"`
    Confidence float32 `json:"confidence,omitempty"`
    Language   string  `json:"language,omitempty"`
    Seq        uint64  `json:"seq"`
}
```

### Sink strategy

| Session type | Sink chính | Sink phụ |
|---|---|---|
| Raw RTP telecom | HTTP Callback | Kafka/WebSocket |
| WebRTC client | DataChannel | WebSocket/SSE |
| Dashboard | WebSocket/SSE | Kafka |
| Backend async | Kafka/Redis Stream | HTTP Callback |

---

## 5.19 WebRTC DataChannel Sink

### Vai trò

Trả transcript trực tiếp về WebRTC client trong cùng PeerConnection thông qua DataChannel `transcript`.

### Chức năng

- Implements `result.Sink` interface — được đăng ký vào `Result Dispatcher` sau khi PeerSession tạo xong.
- Serialize `RecognitionResult` thành `DCMessage` JSON và gửi qua `DataChannel.SendText()`.
- Kiểm tra `ReadyState == Open` trước mỗi lần gửi; trả `ErrDataChannelNotOpen` nếu channel chưa mở.
- Backpressure qua `BufferedAmount`: nếu buffer vượt ngưỡng (mặc định 16 KiB), drop message và trả `ErrDataChannelBufferFull`.
- Đếm `Sent` và `Dropped` bằng atomic counter để giám sát.

### DCMessage — JSON envelope gửi về UE

```go
type DCMessage struct {
    Type       string  `json:"type"`                  // "transcript.partial" | "transcript.final"
    SessionID  string  `json:"session_id"`
    Text       string  `json:"text"`
    IsFinal    bool    `json:"is_final"`
    StartMs    int64   `json:"start_ms"`
    EndMs      int64   `json:"end_ms"`
    Confidence float32 `json:"confidence,omitempty"`
    Language   string  `json:"language,omitempty"`
    Seq        uint64  `json:"seq"`
}
```

### Message mẫu

Partial transcript:

```json
{
  "type": "transcript.partial",
  "session_id": "web-001",
  "text": "xin chào tôi",
  "is_final": false,
  "start_ms": 1000,
  "end_ms": 2100,
  "seq": 3
}
```

Final transcript:

```json
{
  "type": "transcript.final",
  "session_id": "web-001",
  "text": "xin chào tôi cần hỗ trợ kiểm tra tài khoản",
  "is_final": true,
  "start_ms": 1000,
  "end_ms": 4200,
  "confidence": 0.91,
  "language": "vi",
  "seq": 7
}
```

### Backpressure

| Điều kiện | Hành vi | Lỗi trả về |
|-----------|---------|------------|
| `ReadyState != Open` | Drop, tăng `Dropped` | `ErrDataChannelNotOpen` |
| `BufferedAmount > maxBuffer` (mặc định 16 KiB) | Drop, tăng `Dropped` | `ErrDataChannelBufferFull` |
| Gửi thành công | Tăng `Sent` | `nil` |

`maxBuffer` có thể override qua `WithMaxBuffer(n)` (0 = tắt kiểm tra buffer).

### Tích hợp với Dispatcher

```text
Result Dispatcher
   |
   | Push(RecognitionResult)
   v
DataChannelSink.Send()
   |
   | DataChannel.SendText(JSON)
   v
UE (browser / mobile / UE IMS client)
```

`DataChannelSink` được tạo và đăng ký trong `Handler.ServeOffer()` ngay sau khi `newPeerSession()` thành công:

```go
if h.disp != nil && ps.DC != nil {
    h.disp.RegisterSink(req.SessionID, result.NewDataChannelSink(req.SessionID, ps.DC))
}
```

---

## 5.20 WebSocket/SSE Sink

### Vai trò

Trả transcript realtime cho web app hoặc dashboard.

### Chức năng

- Client subscribe theo `session_id`.
- Gateway push result khi AI trả transcript.
- Hỗ trợ reconnect nếu cần.
- Drop partial nếu client không đọc kịp.

### API

```text
GET /v1/sessions/{session_id}/transcript/ws
GET /v1/sessions/{session_id}/transcript/sse
```

---

## 5.21 HTTP Callback Sink

### Vai trò

Callback kết quả transcript về backend/call service.

### Chức năng

- POST RecognitionResult về callback URL.
- Retry giới hạn khi lỗi (cấu hình `callback.max_retry`).
- Timeout ngắn để không block dispatcher.
- Dead-letter hoặc Kafka fallback nếu callback lỗi kéo dài.
- Đếm tổng retry bằng atomic counter; `callback_retry_total` tại `/metrics` đọc từ tất cả sink đã đăng ký.

### Lifecycle & metrics registration

`Coordinator.Start()` tạo `*HTTPCallbackSink` nếu `sess.CallbackURL != ""` và trả nó về cùng với `error`. Handler `createSession` gọi `server.RegisterCallbackSink(sink)` để sink được tính vào `callback_retry_total` tại `/metrics`. Nếu `callback_url` không cung cấp, sink là `nil` và không cần đăng ký.

```
coord.Start(sess)
  → *HTTPCallbackSink (hoặc nil)
  → server.RegisterCallbackSink(sink)   // chỉ khi != nil
  → /metrics: callback_retry_total += sink.Retries()
```

### Request mẫu

```http
POST /api/v1/asr/result
Content-Type: application/json
```

```json
{
  "event_type": "asr.transcript.final",
  "session_id": "call-001",
  "stream_id": "ssrc-123456",
  "text": "xin chào tôi cần hỗ trợ",
  "is_final": true,
  "start_ms": 1000,
  "end_ms": 4200,
  "confidence": 0.91
}
```

---

## 5.22 Kafka/Redis Stream Sink

### Vai trò

Đưa transcript/event vào message broker cho hệ thống lớn hoặc xử lý async.

### Chức năng

- Publish transcript events.
- Cho nhiều consumer đọc cùng lúc.
- Hỗ trợ replay trong một khoảng thời gian.
- Tách Gateway khỏi business service.

### Topic đề xuất

```text
asr.transcript.partial
asr.transcript.final
media.session.started
media.session.ended
media.session.degraded
```

---

## 5.23 Backpressure Controller

### Vai trò

Bảo vệ hệ thống khi tải cao.

### Chức năng

- Theo dõi queue usage, CPU, memory, AI latency, packet drop.
- Đưa ra quyết định drop/degrade/reject.
- Từ chối session mới khi vượt capacity.
- Drop partial result khi client chậm.
- Drop packet quá trễ khi realtime.

### Policy đề xuất

```text
Packet quá trễ -> drop
Packet queue đầy -> drop packet mới hoặc cũ theo policy
AI queue đầy -> mark session degraded hoặc close nếu kéo dài
Result queue đầy -> drop partial, giữ final
Callback lỗi -> retry giới hạn, sau đó dead-letter
Gateway quá tải -> reject session mới
```

---

## 5.24 Worker Pool Manager

### Vai trò

Quản lý tất cả worker pool trong Gateway.

### Worker pool chính

| Pool | Chức năng | Scale theo |
|---|---|---|
| RTP receiver workers | Đọc UDP packet | số port/socket, PPS |
| WebRTC track workers | Đọc audio track | peer connection |
| Audio pipeline workers | Decode/resample/chunk | CPU, queue |
| AI stream workers | Send/recv gRPC stream | active sessions |
| Result dispatcher workers | Gửi result | result QPS |
| Callback workers | HTTP callback retry | callback latency |

### Nguyên tắc

- Worker pool có size cố định hoặc scale trong giới hạn.
- Queue đầu vào bounded.
- Không dùng `go func()` không kiểm soát theo packet/result.
- Mỗi goroutine có owner, context cancel, lifecycle rõ ràng.

---

## 5.25 Metrics & Observability Module

### Vai trò

Cung cấp metric, log, trace để vận hành và autoscaling.

### Metrics RTP

```text
rtp_packets_total
rtp_packets_lost_total
rtp_packets_late_total
rtp_jitter_ms
rtp_sessions_active
rtp_queue_dropped_total
```

### Metrics Audio

```text
audio_decode_errors_total
audio_chunks_total
audio_chunk_latency_ms
audio_resample_errors_total
audio_job_queue_usage
```

### Metrics AI

```text
ai_streams_active
ai_send_errors_total
ai_recv_errors_total
ai_latency_ms
ai_queue_size
ai_worker_gpu_usage
```

### Metrics Result

```text
result_partial_total
result_final_total
result_dispatch_errors_total
result_queue_dropped_total
callback_retry_total        ← tổng retry của tất cả HTTPCallbackSink đã đăng ký;
                               chỉ khác 0 khi session được tạo với callback_url
```

### Metrics System

```text
goroutines_current
memory_usage_bytes
worker_queue_size
session_count
cpu_usage
```

### HTTP Access Log (ginLogger middleware)

Mỗi HTTP request được ghi bằng `slog` với các fields sau:

| Field | Type | Mô tả |
|-------|------|-------|
| `method` | string | HTTP method (GET, POST, …) |
| `path` | string | URL path + query string nếu có |
| `status` | int | HTTP status code |
| `latency_ms` | int64 | Thời gian xử lý request (ms) |
| `ip` | string | Client IP (hỗ trợ X-Forwarded-For) |
| `bytes` | int | Response body size (bytes); -1 = no body |
| `request_id` | string | Giá trị header `X-Request-ID` nếu có |
| `error` | string | Lỗi handler gắn vào `c.Errors` nếu có |

Log level tự động theo status code: `DEBUG` (2xx/3xx) · `WARN` (4xx) · `ERROR` (5xx).

---

## 5.26 Health Check Module

### Vai trò

Cho Control Plane và Kubernetes biết trạng thái service.

### Endpoint

```http
GET /health/live
GET /health/ready
GET /metrics
```

### Readiness nên kiểm tra

- Gateway còn nhận session mới không.
- Queue usage dưới ngưỡng không.
- AI Router reachable không.
- Memory/CPU không vượt ngưỡng.
- Với RTP Gateway: còn port available không.

---

## 5.27 Config Module

### Vai trò

Quản lý cấu hình runtime cho Gateway và AI.

### Config mẫu

Config đầy đủ — xem `internal/config/config.go Default()` để biết giá trị mặc định. Tham khảo `config/gateway.yaml` để xem tất cả field có comment.

```yaml
gateway:
  name: "media-ai-gateway"
  id: "gw-01"
  raw_rtp_enabled: true
  webrtc_enabled: true

server:
  http_addr: ":8080"
  public_url: ""             # địa chỉ public của DCAS, dùng làm callbackUrl trong CALL_CTRL gửi DCSF
                             # ví dụ: "http://10.0.0.1:8080" hoặc "https://dcas.internal:8443"
  cert_file: ""              # để trống → h2c cleartext
  key_file: ""
  metrics_addr: ""           # "" = phục vụ /metrics trên cùng http_addr
  shutdown_timeout_sec: 10

rtp:
  listen_addr: ":5004"       # shared UDP ingress
  public_ip: ""              # IP advertise cho caller (per-session port)
  bind_ip: ""                # IP bind per-session socket; "" = all
  port_start: 0              # 0 = tắt per-session port pool
  port_end: 0
  socket_read_buffer: 4194304  # SO_RCVBUF 4 MiB

webrtc:
  stun_servers:
    - "stun:stun.l.google.com:19302"
  turn_servers: []
  nat1to1_ips: []
  ice_port_min: 0
  ice_port_max: 0
  dc_proxy_url: ""           # IMS DataChannel Mode 2.1 (HTTP Proxy)
  dc_udp_proxy_addr: ""      # IMS DataChannel Mode 2.2 (UDP Proxy, AC.6-2)

session:
  max_sessions: 10000
  idle_timeout_sec: 30
  gc_interval_sec: 5
  per_session_packet_queue: 128
  per_session_audio_queue: 128
  per_session_result_queue: 64

pipeline:
  audio_worker_count: 16
  audio_job_queue_size: 8192
  jitter_buffer_ms: 60
  max_packet_late_ms: 120
  packet_time_ms: 20

audio:
  output_sample_rate: 16000
  output_channels: 1
  chunk_ms: 500
  pcm_dump_dir: ""           # ghi raw PCM decode ra file để verify; "" = tắt

ai:
  grpc_target: "ai-router:50051"
  max_active_streams: 1000
  per_stream_queue_size: 20
  send_timeout_ms: 500
  stream_timeout_sec: 300
  max_retry: 3
  retry_backoff_ms: 1000
  keepalive_time_sec: 30     # gửi HTTP/2 PING tới AI worker sau N giây idle; 0 = tắt
  keepalive_timeout_sec: 10  # đóng conn nếu không nhận PONG trong N giây

result:
  dispatcher_workers: 16
  queue_size: 4096
  drop_partial_when_full: true
  send_timeout_ms: 2000

callback:
  url: ""                    # pre-connect target khi khởi động; "" = không pre-connect
  timeout_ms: 1000
  max_retry: 3
  retry_backoff_ms: 200
  read_idle_timeout_ms: 30000  # gửi H/2 PING sau N ms idle; 0 = tắt
  ping_timeout_ms: 15000       # đóng conn nếu không nhận PONG trong N ms

dcsf:
  hosts: []                    # pre-warm H/2 pool khi khởi động; ví dụ: [{host: "dcsf.internal", port: 9090}]
  call_control_timeout_ms: 30000  # timeout POST CALL_CTRL (ms); cần lớn vì DCSF block chờ CALL_RESULT từ IMS-AS

log:
  level: "info"    # debug | info | warn | error
  format: "json"
  monitor_interval_sec: 60   # in thống kê định kỳ mỗi N giây; 0 = tắt
```

---

## 6. Source code structure đề xuất

```text
cmd/
  media-control-plane/
    main.go
  media-ai-gateway/
    main.go
  ai-router/
    main.go
  ai-worker/
    main.go

internal/
  config/
    config.go

  controlplane/
    server.go             — Server struct, NewServer, routes() (gin.Engine), ListenAndServe
    handler.go            — gin handlers (notifyEvent, handleAnswer, ctrlResult, getCallSession, deleteCallSession, …) + metricsWrite
    middleware.go         — ginLogger: slog request logging middleware
    types.go              — SessionEvent, CtrlResultRequest, SessionResponse, ErrorResponse, StatsResponse
    gateway_registry.go   — GatewayRegistry (TTL-based, thread-safe)
    gateway_selector.go   — GatewaySelector (load-aware routing)
    admission_controller.go — AdmissionController (6 điều kiện reject)
    port_allocator.go     — PortAllocator (free-list, atomic, O(1))

  ingress/
    rawrtp/
      udp_server.go       — shared UDP listener (:5004)
      packet_handler.go   — parse RTP (pion/rtp), route vào session
      port_manager.go     — per-session UDP listener (port pool 40000-40999)
      manager_router.go   — ManagerRouter: bridge SessionManager → rawrtp routing
      testutil_test.go    — buildRTP helper dùng chung trong tests

    webrtc/
      api.go              — NewAPI: pion MediaEngine + InterceptorRegistry + SettingEngine
      handler.go          — ServeOffer: SDP offer/answer, PeerConnection lifecycle
      peer_session.go     — PeerSession: track reader → MediaPacket
      rtp_loop.go         — RTP read loop từ pion TrackRemote
      dc_proxy.go         — DataChannel HTTP proxy (Mode 2.1)
      udp_proxy.go        — DataChannel UDP proxy (Mode 2.2, AC.6-2)
      config.go           — WebRTC Config struct

  session/
    manager.go
    session.go
    router.go
    lifecycle.go
    state_store.go

  pipeline/
    media_packet.go
    audio_pipeline.go
    audio_job.go
    worker_pool.go

  jitter/
    buffer.go
    sequence.go

  codec/
    decoder.go
    g711.go
    opus.go
    amr.go
    evs.go

  audio/
    pcm.go
    resampler.go
    chunker.go

  ai/
    grpc_client.go      — StreamClient interface + Stream/Manager/Config
    grpc_dialer.go      — SharedConnPool (protobuf binary, shared conn, keepalive)
    null_dialer.go      — NullDialer (dev/smoke-test)
    routing_dialer.go   — RoutingDialer: WorkerRegistry.Select → GRPCDialFunc
    worker_info.go      — WorkerInfo, load scoring, capability matching
    worker_registry.go  — WorkerRegistry (TTL-based, thread-safe)
    proto/
      speech.proto      — contract cho AI worker (documentation, không compile)

  result/
    dispatcher.go       — Dispatcher: fan-out per-session Sinks
    sink.go             — Sink interface + SinkFunc helper
    http_callback.go    — HTTPCallbackSink (HTTP/2 POST, retry)
    callback_pool.go    — CallbackPool (dedicated worker pool per-sink)
    datachannel.go      — DataChannelSink (WebRTC DataChannel, JSON, backpressure)
    websocket.go        — WebSocket/SSE Sink (TODO: §5.20)

  monitor/
    monitor.go          — Monitor: periodic slog stats (sessions, AI streams, pool, dispatcher, gRPC state, callback H/2, RTP ports)
```

---

## 7. API chính

## 7.1 Notify Event — DCSF → DCAS

```http
POST /v1/vonras/call-sessions/{callId}/notify-event
```

### BEGIN event

Request:

```json
{
  "callId": "p2uc31@[FC00:0DB8::]",
  "event": "BEGIN",
  "direction": "MT",
  "role": "terminator",
  "bearerCapability": "VIDEO",
  "calling": "86156****5398",
  "called": "86156****5399"
}
```

Response: `200 OK {}` — DCAS ghi nhận, không tạo session.

### ANSWER event

Request:

```json
{
  "callId": "p2uc31@[FC00:0DB8::]",
  "event": "ANSWER",
  "selectedService": "speech_to_text",
  "direction": "MT",
  "role": "terminator",
  "bearerCapability": "VIDEO",
  "location": {
    "areaNumber": "0755",
    "ncgi": "46070094F34050",
    "tac": "000002"
  },
  "calling": "86156****5398",
  "called": "86156****5399",
  "callbackUrl": "http://dcsf.ims.internal:9090/v1/dcsf/call-sessions/p2uc31%40%5BFC00%3A0DB8%3A%3A%5D/call-control"
}
```

Response `201 Created`:

```json
{
  "session_id": "p2uc31@[FC00:0DB8::]",
  "gateway_id": "gw-02",
  "rtp_ip": "10.10.10.22",
  "tcore_rtp_port": 40028,
  "taccess_rtp_port": 40029,
  "status": "CREATED",
  "source_type": "raw_rtp",
  "codec": "PCMU",
  "sample_rate": 8000,
  "channels": 1,
  "task": "speech_to_text",
  "created_at": "2026-06-27T08:00:00Z",
  "tcore_local_non_dc_media": {
    "sdpmLine": "audio 40028 RTP/AVP 0",
    "sdpaLines": ["rtpmap:0 PCMU/8000", "ptime:20", "maxptime:20", "recvonly"]
  },
  "taccess_local_non_dc_media": {
    "sdpmLine": "audio 40029 RTP/AVP 0",
    "sdpaLines": ["rtpmap:0 PCMU/8000", "ptime:20", "maxptime:20", "recvonly"]
  }
}
```

> Mỗi ANSWER tạo **2 internal session** (`{callId}-tcore`, `{callId}-taccess`) với 2 UDP port riêng.  
> `session_id` trong response là `callId` (không phải internal ID).  
> `tcore/taccess_local_non_dc_media` chỉ có khi `rtp.port_start > 0`.  
> DCSF dùng 2 field này làm `remoteNonDcMedia` riêng cho từng termination khi gọi MF MRM `POST /contexts` (TS29.176).  
> Service chưa xử lý (ví dụ `fun_calling`) → `200 OK {}`, không tạo session.

**Codec mapping theo `selectedService`:**

| selectedService | codec | sample_rate |
|---|---|---|
| `speech_to_text` | PCMU | 8000 |
| `realtime_translation` | PCMU | 8000 |
| khác (fun_calling…) | — (ACK không tạo session) | — |

## 7.2 Ctrl Result — DCSF forward kết quả SDP negotiation

```http
POST /v1/vonras/call-sessions/{callId}/ctrl-result
```

Request:

```json
{
  "callId": "p2uc31@[FC00:0DB8::]",
  "mediaResources": {
    "tCore": {
      "contextId": "ctx-001",
      "termination": { "terminationId": "term-core-001", "medias": [...] },
      "callbackUrl": "http://dcsf.example.com/callback/tcore"
    },
    "tAccess": {
      "contextId": "ctx-002",
      "termination": { "terminationId": "term-access-001", "medias": [...] },
      "callbackUrl": "http://dcsf.example.com/callback/taccess"
    }
  }
}
```

Response `200 OK`: SessionResponse (cùng schema với ANSWER response, `session_id = callId`).

> `contextId` và `terminationId` được lưu vào session để enrich HTTP callback payload.  
> `callbackUrl` per-termination được gán vào `sess.CallbackURL` tương ứng — coordinator tạo `HTTPCallbackSink` khi giá trị này không rỗng.  
> `medias` theo schema 3GPP MRM (TS29.176) — được chấp nhận nhưng không parse (forward-compatible).

## 7.3 Lấy thông tin session (debug)

```http
GET /v1/vonras/call-sessions/{callId}
```

Response `200 OK`: SessionResponse với `session_id = callId`. Trả về thông tin của session `{callId}-taccess` (subscriber); nếu không tồn tại thì trả `{callId}-tcore`. Không có `tcore/taccess_rtp_port` hay `local_non_dc_media` trong response này.

## 7.4 Đóng session (RELEASE)

```http
DELETE /v1/vonras/call-sessions/{callId}
```

DCSF gọi khi nhận RELEASE event từ IMS-AS. Đóng cả 2 internal session `{callId}-tcore` và `{callId}-taccess`. Response: `204 No Content`. `404` nếu cả 2 không tồn tại.

## 7.5 WebRTC offer/answer

```http
POST /v1/webrtc/offer
```

```json
{
  "session_id": "web-001",
  "type": "offer",
  "sdp": "v=0\r\n..."
}
```

Response:

```json
{
  "session_id": "web-001",
  "type": "answer",
  "sdp": "v=0\r\n...",
  "udp_proxy_port": 0
}
```

## 7.6 Gateway heartbeat

```http
PUT /v1/gateways/{id}/heartbeat
```

## 7.7 Health / Metrics

```http
GET /health/live    — liveness probe (luôn 200)
GET /health/ready   — readiness probe (503 khi quá tải / no AI worker)
GET /metrics        — Prometheus text format
GET /v1/stats       — aggregate stats JSON
GET /v1/connections — trạng thái AI gRPC, callback H/2, RTP ports
```

---

## 8. Scaling model

## 8.1 Scale Gateway

Media AI Gateway nên scale theo session, không scale random theo packet.

```text
Một media session -> một gateway tại một thời điểm
```

Scale Gateway theo:

- active sessions
- RTP packets per second
- WebRTC peer connections
- audio job queue usage
- packet drop rate
- CPU/memory

## 8.2 Scale AI Worker

AI Worker scale theo:

- active AI streams
- GPU utilization
- GPU memory
- inference latency
- AI queue depth

## 8.3 Tách deployment khi tải cao

```text
media-control-plane
media-gateway-rtp
media-gateway-webrtc
ai-router
ai-worker-faster-whisper
ai-worker-phowhisper
result-broker
```

---

## 9. High Availability và Failover

## 9.1 Gateway failure

Nếu gateway chết, các media session đang gắn với gateway đó thường bị mất vì jitter/decoder/WebRTC state nằm trong gateway.

Policy:

- Stop route session mới vào gateway lỗi.
- Mark session interrupted.
- Notify Call Service.
- WebRTC client reconnect.
- RTP source reconfigure/reINVITE nếu telecom flow hỗ trợ.

## 9.2 AI Worker failure

AI Worker lỗi có thể recover tốt hơn.

Policy:

- Gateway reconnect sang AI Worker khác.
- Tạo AI stream mới.
- Tiếp tục từ audio chunk mới.
- Không buffer audio quá dài để replay.

---

## 10. Roadmap triển khai

## Phase 1A - Raw RTP receiver minimal

- Mở UDP port.
- Parse RTP bằng `pion/rtp`.
- Log SSRC/PT/sequence/timestamp.

## Phase 1B - RTP to PCM

- Decode PCMU/PCMA.
- Ghi WAV file để kiểm tra audio.

## Phase 1C - RTP to AI

- Resample PCM 16kHz mono.
- Gửi gRPC stream sang AI.
- Nhận transcript.
- Callback về backend.

## Phase 1D - Production hardening

- Jitter buffer.
- Worker pool.
- Backpressure.
- Metrics.
- Session timeout.
- Retry callback.

## Phase 2A - WebRTC basic

- HTTP signaling Offer/Answer.
- Pion PeerConnection.
- OnTrack audio.
- ReadRTP đưa vào common pipeline.

## Phase 2B - Browser demo

- getUserMedia microphone.
- Gửi audio qua WebRTC.
- Nhận transcript qua DataChannel/WebSocket.

## Phase 2C - WebRTC production

- STUN/TURN.
- ICE restart.
- Connection state handling.
- DataChannel result.
- Horizontal scaling.

## Phase 3 - High load architecture

- Control Plane HA.
- RTP Gateway Pool riêng.
- WebRTC Gateway Pool riêng.
- AI Router.
- AI Worker Pool GPU/CPU.
- Kafka/Redis result layer.
- Autoscaling theo custom metrics.

---

## 11. Kết luận thiết kế

Thiết kế đề xuất là mô hình nhiều lớp:

```text
Control Plane
  -> chọn gateway, cấp port, admission control

Gateway Data Plane
  -> nhận Raw RTP/WebRTC
  -> session routing
  -> audio pipeline
  -> gRPC stream sang AI

AI Worker Pool
  -> chạy STT/translation

Result Layer
  -> trả transcript qua DataChannel/WebSocket/Callback/Kafka
```

Điểm quan trọng nhất:

- Raw RTP và WebRTC chỉ khác nhau ở ingress.
- Pipeline audio và AI bridge phải dùng chung.
- Mỗi session phải được quản lý chặt chẽ.
- Tất cả queue phải bounded.
- Xử lý nặng phải qua worker pool.
- Gateway và AI Worker phải scale độc lập.
- Khi quá tải phải reject/degrade/drop có kiểm soát, không để hệ thống sập.
