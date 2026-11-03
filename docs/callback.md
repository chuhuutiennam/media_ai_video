# Callback Message Reference — media-ai-gateway

Khi tạo session với `callback_url`, gateway sẽ **POST** kết quả nhận dạng tới URL đó theo từng kết quả trả về từ AI worker.

---

## 1. Tổng quan luồng

```
DCAS (IMS AS)
  │  POST /v1/sessions
  │  { "callback_url": "http://mf-host:port/asr-results", ... }
  ▼
RTP Gateway ◄──────────────────── RTP ────────────────────── MF
     │                                                        ▲
     │  gRPC (AudioChunk)                                     │
     ▼                                          HTTP/2 POST   │
AI Worker ────── gRPC (RecognitionResult) ──► Dispatcher ─────┘
                                                         (callback)
```

Vai trò từng thành phần:

| Thành phần | Vai trò |
|---|---|
| **DCAS** (IMS Application Server) | Tạo session trên gateway (`POST /v1/sessions`) và cung cấp `callback_url` trỏ đến HTTP/2 server của MF |
| **MF** (Media Function) | Gửi RTP audio tới gateway; đồng thời expose HTTP/2 server để nhận callback kết quả ASR |
| **RTP Gateway** | Nhận RTP từ MF → decode → gửi AI qua gRPC → nhận kết quả → gọi HTTP/2 callback về MF |
| **AI Worker** | Nhận AudioChunk qua gRPC bidirectional stream, trả RecognitionResult về gateway |

- Gateway gọi callback **ngay khi nhận được kết quả** từ AI worker (partial hoặc final).
- Mỗi lần gọi = **một POST request** với body JSON chứa một kết quả nhận dạng.
- Không có batching — mỗi `RecognitionResult` là một request riêng biệt.

---

## 2. HTTP Request

### Method & URL

```
POST {callback_url}
```

`callback_url` được cung cấp khi tạo session, ví dụ: `http://backend.svc/api/asr/callback`

### Headers

| Header | Giá trị |
|--------|---------|
| `Content-Type` | `application/json` |

### Transport

| Scheme | Protocol |
|--------|----------|
| `http://` | HTTP/2 cleartext (H2C, prior-knowledge — không upgrade từ HTTP/1.1) |
| `https://` | HTTP/2 over TLS (ALPN negotiation) |

> **Lưu ý**: Callback server **phải hỗ trợ HTTP/2**. HTTP/1.1 không được hỗ trợ.  
> Với H2C, server phải chấp nhận connection plain TCP với HTTP/2 prior-knowledge (không cần Upgrade header).

### Response mong đợi

Gateway coi request thành công khi nhận HTTP `2xx`. Bất kỳ status nào khác đều được xem là lỗi.

---

## 3. Body JSON

### Schema

```jsonc
{
  "event_type":     string,   // loại sự kiện
  "session_id":     string,   // định danh session
  "stream_id":      string,   // định danh stream trong session
  "source_type":    string,   // nguồn audio
  "text":           string,   // transcript nhận dạng được
  "is_final":       boolean,  // true = kết quả xác nhận cuối
  "start_ms":       int64,    // Unix timestamp ms đầu đoạn audio
  "end_ms":         int64,    // Unix timestamp ms cuối đoạn audio
  "confidence":     float32,  // độ tin cậy [0.0, 1.0] — omit nếu = 0
  "language":       string,   // ngôn ngữ — omit nếu rỗng
  "seq":            uint64,   // số thứ tự tăng dần per stream
  "contextId":      string,   // IMS context ID — omit nếu chưa set qua PATCH
  "terminationId":  string    // IMS termination ID — omit nếu chưa set qua PATCH
}
```

### Mô tả từng field

| Field | Type | Required | Ý nghĩa |
|-------|------|----------|---------|
| `event_type` | string | ✓ | Loại sự kiện. Xem bảng Event Types bên dưới. |
| `session_id` | string | ✓ | ID session (khớp với `id` trong lúc tạo session). Dùng để phân biệt nguồn khi một backend nhận callback từ nhiều session. |
| `stream_id` | string | ✓ | ID stream. Với `source_type=raw_rtp`: bằng `session_id`. Với `source_type=webrtc`: bằng WebRTC track ID. Dùng để phân biệt nhiều track trong một session. |
| `source_type` | string | ✓ | Nguồn audio: `"raw_rtp"` hoặc `"webrtc"`. |
| `text` | string | ✓ | Transcript nhận dạng. Với partial: có thể rỗng hoặc là kết quả tạm thời. Với final: kết quả xác nhận cuối cho đoạn âm thanh. |
| `is_final` | boolean | ✓ | `false` = partial (trung gian, có thể thay đổi). `true` = final (xác nhận, không thay đổi). |
| `start_ms` | int64 | ✓ | Unix timestamp (ms) của thời điểm bắt đầu đoạn audio trong recognition window hiện tại. Reset về `end_ms` của final trước sau mỗi kết quả final. |
| `end_ms` | int64 | ✓ | Unix timestamp (ms) khi AI worker trả về kết quả này. |
| `confidence` | float32 | — | Độ tin cậy [0.0, 1.0] do AI worker cung cấp. Bỏ qua (omit) nếu bằng 0. |
| `language` | string | — | Ngôn ngữ nhận dạng (ví dụ: `"vi"`, `"en"`). Bỏ qua (omit) nếu không được AI worker trả về. |
| `seq` | uint64 | ✓ | Số thứ tự tăng dần (monotonic) per stream, bắt đầu từ 1. Dùng để phát hiện callback bị mất hoặc đến lộn thứ tự. Partial và final chia sẻ cùng dãy seq. |
| `contextId` | string | — | IMS Context ID. Lấy từ `mediaResources.tAccess.contextId` khi session được cập nhật qua `PATCH /v1/sessions/{id}`. Bỏ qua (omit) nếu chưa được set. |
| `terminationId` | string | — | IMS Termination ID. Lấy từ `mediaResources.tAccess.terminationId` khi session được cập nhật qua `PATCH /v1/sessions/{id}`. Bỏ qua (omit) nếu chưa được set. |

### Event Types

| `event_type` | `is_final` | Ý nghĩa |
|---|---|---|
| `asr.transcript.partial` | `false` | Kết quả trung gian — transcript tạm thời của đoạn đang nói. Có thể bị thay thế bởi partial hoặc final tiếp theo. Backend nên hiển thị "live" nhưng không lưu trữ. |
| `asr.transcript.final` | `true` | Kết quả xác nhận — transcript hoàn chỉnh cho một đoạn lời nói. Backend nên lưu trữ và xử lý. |

---

## 4. Ví dụ

### Partial result

```json
{
  "event_type":  "asr.transcript.partial",
  "session_id":  "call-abc123",
  "stream_id":   "call-abc123",
  "source_type": "raw_rtp",
  "text":        "xin chào...",
  "is_final":    false,
  "start_ms":    1782193409000,
  "end_ms":      1782193411500,
  "confidence":  0,
  "language":    "vi",
  "seq":         1
}
```

### Final result

```json
{
  "event_type":    "asr.transcript.final",
  "session_id":    "call-abc123",
  "stream_id":     "call-abc123",
  "source_type":   "raw_rtp",
  "text":          "Xin chào, tôi cần hỗ trợ.",
  "is_final":      true,
  "start_ms":      1782193409000,
  "end_ms":        1782193415200,
  "confidence":    0.92,
  "language":      "vi",
  "seq":           3,
  "contextId":     "ctx-001",
  "terminationId": "term-001"
}
```

> `contextId` và `terminationId` chỉ xuất hiện khi session đã được cập nhật qua `PATCH /v1/sessions/{id}` với `mediaResources.tAccess`.

### PATCH session sau SDP negotiation

Sau khi tạo session (`POST /v1/sessions`), DCAS hoàn tất SDP negotiation với MF rồi gửi PATCH để cập nhật:

```bash
curl -s --http2-prior-knowledge \
  -X PATCH http://gateway:8080/v1/sessions/call-abc123 \
  -H "Content-Type: application/json" \
  -d '{
    "callback_url": "http://mf-host:8090/asr-results",
    "mediaResources": {
      "tCore": {
        "contextId":     "ctx-001",
        "terminationId": "term-core-001",
        "endpoint":      "10.0.0.1:5004"
      },
      "tAccess": {
        "contextId":     "ctx-001",
        "terminationId": "term-access-001",
        "endpoint":      "10.0.1.2:5004"
      }
    }
  }'
```

**Hiệu lực của mỗi field:**

| Field | Tác động |
|-------|---------|
| `callback_url` | Thay thế HTTP/2 callback sink cho session này ngay lập tức. Các kết quả tiếp theo gửi đến URL mới. |
| `mediaResources.tAccess.endpoint` | Cập nhật `remote_addr` của session — dùng làm địa chỉ nguồn RTP fallback khi SSRC=0 trong shared ingress mode. |
| `mediaResources.tAccess.contextId` | Gán `contextId` vào session; xuất hiện ở top-level trong tất cả callback kể từ lúc PATCH. |
| `mediaResources.tAccess.terminationId` | Gán `terminationId` vào session; xuất hiện ở top-level trong tất cả callback kể từ lúc PATCH. |
| `mediaResources.tCore.*` | Lưu vào session (dùng để identify IMS core termination), nhưng **không** xuất hiện trong callback payload. |

Tất cả field đều optional — chỉ field có giá trị mới được cập nhật. Response trả về session state hiện tại (HTTP 200).

**Callback sau PATCH** sẽ có thêm `contextId` và `terminationId` từ `tAccess`:

```json
{
  "event_type":    "asr.transcript.final",
  "session_id":    "call-abc123",
  "stream_id":     "call-abc123",
  "source_type":   "raw_rtp",
  "text":          "Xin chào, tôi cần hỗ trợ.",
  "is_final":      true,
  "start_ms":      1782193409000,
  "end_ms":        1782193415200,
  "confidence":    0.92,
  "language":      "vi",
  "seq":           3,
  "contextId":     "ctx-001",
  "terminationId": "term-access-001"
}
```

### Chuỗi ví dụ trong một cuộc gọi 6-chunk (3s audio)

```
seq=1  partial  "xin chào..."                      ← sau chunk 3
seq=2  partial  "đây là kết quả..."                ← sau chunk 6 (trước final)
seq=3  FINAL    "Xin chào, tôi cần hỗ trợ."        ← sau chunk 6 (final)
seq=4  partial  "đang nhận dạng..."                ← sau chunk 9 (window mới)
```

---

## 5. Connection Persistence (H/2 Connection Pool)

Gateway dùng **shared HTTP/2 client** duy nhất cho toàn bộ callback — tất cả session cùng callback host tái sử dụng 1 kết nối H/2 thay vì mỗi session mở kết nối riêng.

### Pre-connect khi khởi động

Nếu `callback.url` được cấu hình, gateway tự động thiết lập kết nối H/2 ngay lúc start, trước khi có session đầu tiên:

```
{"level":"INFO","msg":"callback: preconnected via HTTP/2","url":"http://mf-callback.internal:8090/","status":200}
```

Nếu callback server chưa sẵn sàng lúc gateway start, log sẽ là:

```
{"level":"WARN","msg":"callback: preconnect failed — will retry on first callback","url":"...","err":"..."}
```

Điều này **không fatal** — gateway vẫn khởi động bình thường và retry kết nối khi có callback đầu tiên.

### Keep-alive H/2 PING

`read_idle_timeout_ms` và `ping_timeout_ms` đảm bảo kết nối H/2 không bị timeout bởi firewall/NAT khi idle:

```
callback server ←── H/2 PING (sau 30s idle) ───── gateway
callback server ───► H/2 PONG ──────────────────── gateway
```

Nếu PONG không đến trong `ping_timeout_ms` (15s mặc định), connection bị đóng và tự động reconnect ở request tiếp theo.

---

## 6. Retry Policy

Khi callback server không phản hồi hoặc trả về lỗi, gateway tự động retry.

| Tình huống | Hành vi |
|-----------|---------|
| HTTP `5xx` | Retry — lỗi phía server, có thể tự phục hồi |
| HTTP `4xx` | **Không retry** — lỗi phía client (sai URL, auth fail), retry vô ích |
| Network error (timeout, connection refused) | Retry |
| HTTP `2xx` | Thành công, không retry |

### Tham số (cấu hình trong `callback:` section của YAML)

| Tham số YAML | Mặc định | Ý nghĩa |
|---|---|---|
| `url` | `""` | URL pre-connect khi gateway khởi động (`scheme://host:port`). Để trống = không pre-connect. Ví dụ: `"http://mf-callback.internal:8090"`. |
| `timeout_ms` | `1000` | Timeout mỗi request (ms). Tính từ lúc bắt đầu gửi đến khi nhận response. |
| `max_retry` | `3` | Số lần retry **sau lần gửi đầu** (tổng tối đa = `1 + max_retry = 4 lần`). |
| `retry_backoff_ms` | `200` | Thời gian chờ cố định giữa các lần retry (ms). Không tăng dần (constant backoff). |
| `read_idle_timeout_ms` | `30000` | Gửi H/2 PING tới callback server sau N ms không có traffic. `0` = tắt PING. Giúp phát hiện connection chết sớm. |
| `ping_timeout_ms` | `15000` | Đóng connection nếu không nhận PONG trong N ms sau khi gửi PING. |

**Ví dụ timeline với default config:**

```
t=0ms    Lần 1: POST → 503
t=200ms  Lần 2: POST → timeout (>1000ms)
t=1400ms Lần 3: POST → timeout
t=2600ms Lần 4: POST → 200 OK ✓
```

Nếu hết tất cả retry vẫn lỗi: kết quả bị **drop** và `media_ai_dispatcher_send_errors_total` tăng 1.

---

## 7. Drop Policy

Khi dispatcher queue bị đầy:

| Loại kết quả | Hành vi |
|---|---|
| Partial (`is_final=false`) | **Bị drop ngay** — không đưa vào queue. Không retry. `media_ai_result_queue_dropped_total` tăng. |
| Final (`is_final=true`) | **Block** cho đến khi có chỗ trong queue hoặc context bị cancel. |

> Partial được ưu tiên drop để đảm bảo final luôn được deliver.

---

## 8. Kích hoạt Callback

### Tạo session với `callback_url`

```bash
curl -s --http2-prior-knowledge \
  -X POST http://gateway:8080/v1/sessions \
  -H "Content-Type: application/json" \
  -d '{
    "id":           "call-abc123",
    "source_type":  "raw_rtp",
    "codec":        "PCMU",
    "sample_rate":  8000,
    "ssrc":         12345,
    "callback_url": "http://mf-host:port/asr-results"
  }'
```

`callback_url` trỏ đến HTTP/2 server của **MF** (Media Function) — thành phần vừa gửi RTP đến gateway, vừa expose endpoint để nhận kết quả ASR ngược lại.  
`callback_url` là tùy chọn. Nếu bỏ qua, kết quả chỉ gửi qua DataChannel (WebRTC) nếu có.  
Có thể đặt cả hai — gateway gửi song song tới tất cả sink đã đăng ký.

---

## 9. Triển khai Callback Server (phía MF)

**MF** (Media Function) phải implement một HTTP/2 server để nhận callback từ gateway. Server này nằm tại địa chỉ được cung cấp trong `callback_url` khi DCAS tạo session.

### Tối thiểu

```python
# Python (h2 library)
from h2.connection import H2Connection
# ...
# Nhận POST /asr-results, parse JSON, trả 200
```

```go
// Go (golang.org/x/net/http2/h2c)
import "golang.org/x/net/http2/h2c"

handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    var payload CallbackBody
    json.NewDecoder(r.Body).Decode(&payload)
    // xử lý payload ASR từ gateway...
    w.WriteHeader(http.StatusOK)
})
srv := &http.Server{Handler: h2c.NewHandler(handler, &http2.Server{})}
srv.ListenAndServe()
```

### Lưu ý khi implement

1. **Server phải hỗ trợ H2C** — không thể dùng reverse proxy HTTP/1.1 trước MF callback server nếu callback URL dùng `http://`.
2. **Idempotency** — do retry, cùng một `(session_id, seq)` có thể nhận nhiều lần. Nên deduplicate theo `seq` nếu cần.
3. **Thứ tự không được đảm bảo** — các partial của cùng một final có thể đến không đúng thứ tự nếu mạng có reordering. Dùng `seq` để sort.
4. **Phản hồi nhanh** — timeout mặc định 1s. Callback server nên acknowledge ngay (200 OK) rồi xử lý async. Nếu xử lý blocking, tăng `callback.timeout_ms`.
5. **Partial là best-effort** — không được đảm bảo deliver khi load cao. Chỉ `is_final=true` được đảm bảo.
