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
Call Service
   |
   | POST /api/v1/rtp/sessions
   v
Media Control Plane
   |
   | chọn Gateway + allocate RTP port
   v
RTP Gateway
   |
   | trả IP/port
   v
SBC/MGW gửi RTP tới Gateway
   |
   v
Raw RTP Ingress
   |
   v
Session Router
   |
   v
Jitter Buffer
   |
   v
Codec Decoder
   |
   v
Audio Normalizer
   |
   v
AI gRPC Stream
   |
   v
AI Worker
   |
   v
RecognitionResult
   |
   v
HTTP Callback / Kafka / WebSocket
```

### 4.2 Luồng WebRTC Phase 2

```text
Browser/Mobile Client
   |
   | POST /api/v1/webrtc/offer
   v
Control Plane / WebRTC Gateway
   |
   | SDP Answer
   v
Client thiết lập PeerConnection
   |
   | Audio Track RTP qua WebRTC
   v
Pion WebRTC Ingress
   |
   | TrackRemote.ReadRTP()
   v
Session Router
   |
   v
Common Audio Pipeline
   |
   v
AI gRPC Stream
   |
   v
AI Worker
   |
   v
RecognitionResult
   |
   v
WebRTC DataChannel / WebSocket / SSE
```

---

## 5. Mô tả chi tiết từng module

## 5.1 Media Control Plane

### Vai trò

Media Control Plane là bộ điều phối session và tài nguyên. Module này không xử lý packet RTP/audio trực tiếp, mà quyết định session được tạo ở đâu và đi vào gateway nào.

### Chức năng

- Nhận yêu cầu tạo session từ Call Service hoặc client.
- Chọn gateway phù hợp dựa trên tải hiện tại.
- Cấp RTP port cho Raw RTP session.
- Trả thông tin endpoint cho client/SBC/MGW.
- Lưu mapping `session_id -> gateway_id`.
- Theo dõi health/load của các gateway.
- Từ chối session mới nếu hệ thống quá tải.

### Input

```json
{
  "session_id": "call-001",
  "source_type": "raw_rtp",
  "codec": "PCMU",
  "sample_rate": 8000,
  "channels": 1,
  "callback_url": "http://call-service/api/v1/asr/result",
  "language": "vi",
  "task": "transcribe"
}
```

### Output

```json
{
  "session_id": "call-001",
  "gateway_id": "gw-02",
  "rtp_ip": "10.10.10.22",
  "rtp_port": 40028,
  "status": "created"
}
```

### State quản lý

- Gateway registry.
- Session mapping.
- Port allocation table.
- Gateway capacity/load.
- Session status.

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

Quản lý port UDP cho Raw RTP trên từng gateway.

### Chức năng

- Cấp port rảnh khi tạo session.
- Release port khi session kết thúc.
- Tránh cấp trùng port.
- Theo dõi port range theo gateway.

### Cách phân bổ đề xuất

```text
Gateway A: 40000-40999
Gateway B: 41000-41999
Gateway C: 42000-42999
```

### Lưu ý

Với Raw RTP, source phải gửi RTP tới đúng IP/port đã được allocate. Không nên dùng random UDP load balancing nếu gateway giữ state.

---

## 5.5 Raw RTP Ingress Module

### Vai trò

Nhận RTP packet trực tiếp qua UDP từ SBC/MGW/C++ Media Plane.

### Chức năng

- Mở UDP socket theo port được cấp.
- Set socket read buffer lớn.
- Đọc packet nhanh, không xử lý nặng tại read loop.
- Parse RTP header bằng `github.com/pion/rtp`.
- Tạo `MediaPacket` chuẩn hóa.
- Đẩy packet vào Session Router.

### Input

```text
UDP RTP packet
```

### Output

```go
MediaPacket{
    SessionID: "call-001",
    SourceType: "raw_rtp",
    SSRC: 123456,
    PayloadType: 0,
    Sequence: 1001,
    Timestamp: 96000,
    Payload: []byte{...},
    Codec: "PCMU",
    SampleRate: 8000,
    Channels: 1,
}
```

### Worker model

- Một hoặc vài goroutine receiver theo UDP socket/port group.
- Không tạo goroutine theo từng packet.
- Đẩy packet vào bounded queue.

### Quá tải

- Nếu packet queue đầy: drop packet mới hoặc packet quá cũ.
- Tăng metric `rtp_queue_dropped_total`.
- Mark session degraded nếu drop kéo dài.

---

## 5.6 WebRTC Ingress Module

### Vai trò

Nhận audio từ browser/mobile/web client thông qua WebRTC.

### Chức năng

- Xử lý HTTP signaling Offer/Answer.
- Tạo Pion `PeerConnection`.
- Cấu hình ICE/STUN/TURN.
- Nhận audio track qua `OnTrack`.
- Đọc RTP packet từ `TrackRemote.ReadRTP()`.
- Tạo `MediaPacket` và đẩy vào Session Router.
- Quản lý DataChannel nếu dùng để trả transcript.

### Input

```text
SDP Offer
ICE Candidate
WebRTC audio track
```

### Output

```text
MediaPacket từ TrackRemote.ReadRTP()
```

### API mẫu

```http
POST /api/v1/webrtc/offer
```

Request:

```json
{
  "session_id": "web-001",
  "type": "offer",
  "sdp": "...",
  "metadata": {
    "language": "vi",
    "task": "transcribe"
  }
}
```

Response:

```json
{
  "session_id": "web-001",
  "type": "answer",
  "sdp": "..."
}
```

### Lưu ý

- WebRTC audio mặc định thường là Opus 48kHz.
- Cần resample về PCM 16kHz mono trước khi gửi AI.
- Cần session affinity cho signaling và PeerConnection.

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

- Với Raw RTP: map theo source IP/source port/SSRC hoặc theo port đã allocate.
- Với WebRTC: map theo peer ID/track ID/SSRC.
- Tạo session động nếu policy cho phép.
- Drop packet nếu không xác định được session.
- Ghi metric packet unknown/session missing.

### Routing key

```text
Raw RTP:
  gateway_port + remote_addr + ssrc

WebRTC:
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
- Metric: `audio_job_queue_dropped_total`.

---

## 5.15 AI Stream Manager

### Vai trò

Quản lý gRPC stream giữa Gateway và AI service theo từng session.

### Chức năng

- Mỗi session có một gRPC bidirectional stream.
- Send audio chunk sang AI.
- Recv RecognitionResult từ AI.
- Reconnect khi AI worker lỗi nếu policy cho phép.
- Quản lý per-stream send queue bounded.
- Enforce timeout và max active stream.

### Luồng

```text
AudioChunk
   |
   v
AI Send Queue per session
   |
   v
gRPC Send loop
   |
   v
AI Worker
   |
   v
gRPC Recv loop
   |
   v
Result Dispatcher
```

### Proto đề xuất

```proto
syntax = "proto3";

service SpeechStream {
  rpc Recognize(stream AudioChunk) returns (stream RecognitionResult);
}

message AudioChunk {
  string session_id = 1;
  string stream_id = 2;
  bytes pcm = 3;
  int32 sample_rate = 4;
  int32 channels = 5;
  int64 timestamp_ms = 6;
  int64 duration_ms = 7;
  bool end_of_stream = 8;
  string language = 9;
  string task = 10;
}

message RecognitionResult {
  string session_id = 1;
  string stream_id = 2;
  string text = 3;
  bool is_final = 4;
  int64 start_ms = 5;
  int64 end_ms = 6;
  float confidence = 7;
  string language = 8;
  uint64 seq = 9;
}
```

### Config

```yaml
ai:
  max_active_streams: 1000
  per_stream_queue_size: 20
  send_timeout_ms: 500
  stream_timeout_sec: 300
```

---

## 5.16 AI Router

### Vai trò

Route stream từ Gateway tới AI worker phù hợp.

### Chức năng

- Chọn AI worker theo model/language/task.
- Theo dõi load AI worker.
- Giới hạn active streams trên từng worker.
- Tránh gửi stream mới vào worker quá tải.
- Hỗ trợ nhiều model: faster-whisper, PhoWhisper, translation.

### Routing criteria

```text
language = vi -> PhoWhisper pool
task = transcribe -> STT pool
task = translate -> STT + translation pipeline
model = faster-whisper-medium -> worker pool medium
gpu_load thấp -> ưu tiên worker đó
```

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

Trả transcript trực tiếp về WebRTC client trong cùng PeerConnection.

### Chức năng

- Quản lý DataChannel `transcript`.
- Serialize RecognitionResult thành JSON.
- Gửi partial/final transcript về client.
- Kiểm tra channel open trước khi send.
- Drop partial nếu client chậm.

### Message mẫu

```json
{
  "type": "transcript.final",
  "session_id": "web-001",
  "text": "xin chào tôi cần hỗ trợ kiểm tra tài khoản",
  "is_final": true,
  "start_ms": 1000,
  "end_ms": 4200
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
GET /api/v1/sessions/{session_id}/transcript/ws
GET /api/v1/sessions/{session_id}/transcript/sse
```

---

## 5.21 HTTP Callback Sink

### Vai trò

Callback kết quả transcript về backend/call service.

### Chức năng

- POST RecognitionResult về callback URL.
- Retry giới hạn khi lỗi.
- Timeout ngắn để không block dispatcher.
- Dead-letter hoặc Kafka fallback nếu callback lỗi kéo dài.

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
callback_retry_total
```

### Metrics System

```text
goroutines_current
memory_usage_bytes
worker_queue_size
session_count
cpu_usage
```

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

```yaml
gateway:
  name: "media-ai-gateway"
  raw_rtp_enabled: true
  webrtc_enabled: true

server:
  http_addr: ":8080"
  metrics_addr: ":9090"
  shutdown_timeout_sec: 10

rtp:
  listen_ip: "0.0.0.0"
  port_start: 40000
  port_end: 40100
  socket_read_buffer: 4194304
  receiver_workers: 2

webrtc:
  enabled: true
  stun_servers:
    - "stun:stun.l.google.com:19302"
  turn_servers: []
  max_peer_connections: 5000
  audio_codecs:
    - "opus"
    - "PCMU"
    - "PCMA"
  enable_datachannel_result: true

session:
  max_sessions: 10000
  idle_timeout_sec: 30
  per_session_packet_queue: 128
  per_session_result_queue: 64

pipeline:
  audio_worker_count: 16
  audio_job_queue_size: 8192
  jitter_buffer_ms: 60
  max_packet_late_ms: 120

audio:
  output_sample_rate: 16000
  output_channels: 1
  chunk_ms: 500

ai:
  grpc_target: "ai-router:50051"
  max_active_streams: 1000
  per_stream_queue_size: 20
  send_timeout_ms: 500
  stream_timeout_sec: 300
  max_retry: 3

result:
  dispatcher_workers: 16
  queue_size: 4096
  drop_partial_when_full: true
  keep_final_when_possible: true

callback:
  timeout_ms: 1000
  max_retry: 3
  retry_backoff_ms: 200
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
    session_api.go
    gateway_registry.go
    gateway_selector.go
    admission_controller.go
    port_allocator.go

  ingress/
    rawrtp/
      udp_server.go
      packet_reader.go
      packet_handler.go
      port_manager.go

    webrtc/
      peer_server.go
      signaling_http.go
      track_handler.go
      datachannel.go
      ice_config.go
      whip_handler.go

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
    grpc_client.go
    stream_manager.go
    router_client.go
    proto/
      speech.proto

  result/
    dispatcher.go
    sink.go
    http_callback.go
    websocket.go
    datachannel.go
    kafka.go

  runtime/
    backpressure.go
    health.go
    metrics.go
    graceful_shutdown.go
```

---

## 7. API chính

## 7.1 Tạo Raw RTP session

```http
POST /api/v1/rtp/sessions
```

Request:

```json
{
  "session_id": "call-001",
  "codec": "PCMU",
  "sample_rate": 8000,
  "channels": 1,
  "callback_url": "http://call-service/api/v1/asr/result",
  "language": "vi",
  "task": "transcribe"
}
```

Response:

```json
{
  "session_id": "call-001",
  "gateway_id": "gw-02",
  "listen_ip": "10.10.10.22",
  "listen_port": 40028,
  "status": "created"
}
```

## 7.2 Tạo WebRTC session

```http
POST /api/v1/webrtc/offer
```

Request:

```json
{
  "session_id": "web-001",
  "type": "offer",
  "sdp": "...",
  "metadata": {
    "language": "vi",
    "task": "transcribe"
  }
}
```

Response:

```json
{
  "session_id": "web-001",
  "type": "answer",
  "sdp": "..."
}
```

## 7.3 Đóng session

```http
DELETE /api/v1/sessions/{session_id}
```

## 7.4 Subscribe transcript WebSocket

```text
GET /api/v1/sessions/{session_id}/transcript/ws
```

## 7.5 Health/metrics

```http
GET /health/live
GET /health/ready
GET /metrics
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
