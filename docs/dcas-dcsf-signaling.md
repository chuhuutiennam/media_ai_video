# Giao tiếp DCAS ↔ DCSF — Signaling Flow

Tài liệu này mô tả 4 bản tin tín hiệu giữa **DCAS** (media-ai-gateway) và **DCSF** trong luồng thiết lập và kiểm soát cuộc gọi.

---

## Tổng quan luồng

```
DCSF                                    DCAS (media-ai-gateway)
  │                                              │
  │── POST notify-event {BEGIN} ───────────────>│
  │<─ 200 OK {} ────────────────────────────────│
  │                                              │
  │── POST notify-event {ANSWER} ──────────────>│  [tạo 2 session + cấp RTP port]
  │<─ 200 OK {tcore_rtp_port, taccess_rtp_port} │
  │                                              │
  │<── POST call-control {duplicate} ───────────│  [gửi async sau khi flush response]
  │─── 200 OK ─────────────────────────────────>│
  │                                              │
  │── POST ctrl-result {mediaResources} ───────>│  [cập nhật termination IDs + callback URLs]
  │<─ 200 OK {session_id, status, ...} ─────────│
```

---

## 1. BEGIN

### Chiều: DCSF → DCAS

**Endpoint:** `POST /v1/vonras/call-sessions/{callId}/notify-event`

**Mô tả:** Bản tin báo hiệu cuộc gọi bắt đầu. DCAS chỉ ACK, không tạo session.

### Request body

```json
{
  "callId": "call-abc123",
  "event": "BEGIN",
  "direction": "MT",
  "role": "terminator",
  "bearerCapability": "AUDIO",
  "calling": "0901234567",
  "called": "1800123456",
  "location": {
    "areaNumber": "01",
    "ncgi": "452040123456789",
    "tac": "1234"
  }
}
```

| Field | Type | Mô tả |
|---|---|---|
| `callId` | string | ID cuộc gọi duy nhất từ DCSF |
| `event` | string | `"BEGIN"` (case-insensitive) |
| `direction` | string | `"MO"` \| `"MT"` |
| `role` | string | `"originator"` \| `"terminator"` |
| `bearerCapability` | string | `"AUDIO"` \| `"VIDEO"` |
| `calling` | string \| array | Số gọi đến (DCAS bỏ qua nếu là array) |
| `called` | string \| array | Số được gọi |
| `location` | object | Thông tin vị trí UE; optional |

### Response

```
HTTP/1.1 200 OK
Content-Type: application/json

{}
```

DCAS không xử lý gì thêm với BEGIN. Không có action nội bộ.

---

## 2. ANSWER

### Chiều: DCSF → DCAS

**Endpoint:** `POST /v1/vonras/call-sessions/{callId}/notify-event`

**Mô tả:** DCSF thông báo cuộc gọi đã được trả lời và cung cấp dịch vụ AI cần xử lý. DCAS tạo 2 session nội bộ (`{callId}-tcore` và `{callId}-taccess`), cấp phát 2 RTP port, khởi động pipeline AI, rồi trả về thông tin RTP endpoint.

### Request body

```json
{
  "callId": "call-abc123",
  "event": "ANSWER",
  "selectedService": "speech_to_text",
  "direction": "MT",
  "role": "terminator",
  "bearerCapability": "AUDIO",
  "calling": "0901234567",
  "called": "1800123456",
  "dcasId": "dcas_stt",
  "callbackKey": "a81bc91f92ab1122",
  "callbackUrl": "http://dcsf-host:9090/v1/dcsf/call-sessions/call-abc123/call-control/dcas_stt/a81bc91f92ab1122"
}
```

| Field | Type | Mô tả |
|---|---|---|
| `callId` | string | ID cuộc gọi |
| `event` | string | `"ANSWER"` |
| `selectedService` | string | `"speech_to_text"` \| `"realtime_translation"` — xác định codec và task AI |
| `direction` | string | `"MO"` \| `"MT"` |
| `role` | string | `"originator"` \| `"terminator"` |
| `dcasId` | string | Định danh DCAS module: `"dcas_stt"` \| `"dcas_rt"` \| `"dcas_fc"` \| `"dcas_vvc"` |
| `callbackKey` | string | Random 16-char hex — DCSF dùng để route CALL_CTRL vào đúng Erlang process |
| `callbackUrl` | string | URL DCAS POST CALL_CTRL về DCSF; format: `.../call-control/{dcasId}/{callbackKey}` |

**Mapping `selectedService` → codec:**

| selectedService | Codec | Sample Rate | AI Task |
|---|---|---|---|
| `speech_to_text` | PCMU | 8000 Hz | `speech_to_text` |
| `realtime_translation` | PCMU | 8000 Hz | `realtime_translation` |
| *(khác)* | — | — | ACK 200 `{}`, không tạo session |

### Response

```json
{
  "session_id": "call-abc123",
  "status": "active",
  "source_type": "raw_rtp",
  "codec": "PCMU",
  "sample_rate": 8000,
  "channels": 1,
  "task": "speech_to_text",
  "created_at": "2026-08-10T10:00:00.000000+07:00",
  "gateway_id": "gw-ims-01",
  "rtp_ip": "10.0.1.5",
  "tcore_rtp_port": 40000,
  "taccess_rtp_port": 40001,
  "tcore_local_non_dc_media": {
    "sdpmLine": "audio 40000 RTP/AVP 0",
    "sdpaLines": [
      "rtpmap:0 PCMU/8000",
      "ptime:20",
      "maxptime:20",
      "sendrecv"
    ]
  },
  "taccess_local_non_dc_media": {
    "sdpmLine": "audio 40001 RTP/AVP 0",
    "sdpaLines": [
      "rtpmap:0 PCMU/8000",
      "ptime:20",
      "maxptime:20",
      "sendrecv"
    ]
  }
}
```

| Field | Mô tả |
|---|---|
| `session_id` | Echo lại `callId` (không phải internal session ID) |
| `rtp_ip` | Public IP của gateway (MF gửi RTP đến đây) |
| `tcore_rtp_port` | UDP port nhận RTP từ tCore (TDM gateway phía mạng lõi) |
| `taccess_rtp_port` | UDP port nhận RTP từ tAccess (TDM gateway phía thuê bao) |
| `tcore_local_non_dc_media` | SDP mô tả RTP endpoint của DCAS cho tCore stream; DCAS dùng làm `remoteNonDcMedia` khi gọi MF MRM API |
| `taccess_local_non_dc_media` | SDP mô tả RTP endpoint của DCAS cho tAccess stream |

**Lưu ý:** Response được flush ngay trước khi DCAS gửi CALL_CTRL (bước tiếp theo). DCSF không cần chờ CALL_CTRL để xử lý response ANSWER.

**Lỗi:**

| HTTP | Nguyên nhân |
|---|---|
| `400` | JSON không hợp lệ hoặc event không nhận dạng được |
| `409` | `callId` đã tồn tại trong session manager |
| `503` | Không đủ RTP port hoặc gateway đạt giới hạn session |
| `500` | Lỗi khởi động RTP listener hoặc pipeline |

---

## 3. CALL_CONTROL (CALL_CTRL)

### Chiều: DCAS → DCSF

**Endpoint:** `POST {callbackUrl}` — URL lấy từ trường `callbackUrl` trong ANSWER request body

**Mô tả:** Sau khi flush response ANSWER, DCAS gửi CALL_CTRL bất đồng bộ (goroutine riêng) để yêu cầu DCSF thiết lập tài nguyên H.248 trên Media Function (MF). DCAS yêu cầu action `"duplicate"` để MF tạo context với 2 termination (tCore + tAccess) và duplicate RTP stream về DCAS endpoint.

**Thời điểm gửi:** Ngay sau khi `c.Writer.Flush()` trả về trong handler ANSWER. Timeout mặc định 30s (cấu hình qua `dcsf.call_control_timeout_ms`).

### Request body

```json
{
  "callId": "call-abc123",
  "actions": "duplicate",
  "callbackUrl": "http://dcas-host:8080/v1/vonras/call-sessions/call-abc123/ctrl-result"
}
```

| Field | Type | Mô tả |
|---|---|---|
| `callId` | string | ID cuộc gọi |
| `actions` | string | Luôn là `"duplicate"` |
| `callbackUrl` | string | URL DCSF dùng để POST ctrl-result trở lại DCAS (xem §4) |

**Transport:** HTTP/2 cleartext (h2c), connection pool per DCSF host.

### Response kỳ vọng

```
HTTP/1.1 200 OK
```

Bất kỳ status nào ngoài 2xx → DCAS log warning và đóng cả 2 session (`{callId}-tcore` + `{callId}-taccess`).

**Lỗi từ DCSF:**

```json
{
  "cause": "mô tả lỗi từ DCSF"
}
```

DCAS đọc trường `cause` để log rõ nguyên nhân khi DCSF trả non-2xx.

---

## 4. CTRL_RESULT (CALL_RESULT)

### Chiều: DCSF → DCAS

**Endpoint:** `POST /v1/vonras/call-sessions/{callId}/ctrl-result`

**Mô tả:** DCSF gửi kết quả thiết lập tài nguyên H.248 sau khi xử lý CALL_CTRL. Body chứa thông tin context ID, termination ID của tCore và tAccess trên MF, kèm theo callback URL để DCAS gửi kết quả nhận dạng (ASR result) về từng termination.

### Request body

```json
{
  "callId": "call-abc123",
  "mediaResources": {
    "tCore": {
      "contextId": "ctx-0001",
      "termination": {
        "terminationId": "term-tcore-001",
        "medias": [...]
      },
      "callbackUrl": "http://mf-host:8080/v1/mf/contexts/ctx-0001/terminations/term-tcore-001/result"
    },
    "tAccess": {
      "contextId": "ctx-0002",
      "termination": {
        "terminationId": "term-taccess-001",
        "medias": [...]
      },
      "callbackUrl": "http://mf-host:8080/v1/mf/contexts/ctx-0002/terminations/term-taccess-001/result"
    }
  }
}
```

| Field | Type | Mô tả |
|---|---|---|
| `callId` | string | ID cuộc gọi |
| `mediaResources.tCore.contextId` | string | H.248 context ID của tCore trên MF |
| `mediaResources.tCore.termination.terminationId` | string | H.248 termination ID của tCore |
| `mediaResources.tCore.termination.medias` | array | MediaInfo per TS29.176; DCAS giữ nguyên raw, không parse |
| `mediaResources.tCore.callbackUrl` | string | URL DCAS POST ASR result cho tCore stream |
| `mediaResources.tAccess.*` | — | Tương tự cho tAccess stream |

**Xử lý nội bộ của DCAS:**
- Cập nhật `session.CallbackURL` cho cả `{callId}-tcore` và `{callId}-taccess`
- Cập nhật `session.MediaResources` (contextId + terminationId) để attach vào ASR result
- Đăng ký/cập nhật HTTP callback sink cho từng session

### Response

```json
{
  "session_id": "call-abc123",
  "status": "active",
  "source_type": "raw_rtp",
  "codec": "PCMU",
  "sample_rate": 8000,
  "channels": 1,
  "task": "speech_to_text",
  "created_at": "2026-08-10T10:00:00.000000+07:00"
}
```

**Lưu ý:** Response dùng thông tin của session `{callId}-taccess` (subscriber side). Nếu taccess không tồn tại thì fallback sang `{callId}-tcore`.

**Lỗi:**

| HTTP | Nguyên nhân |
|---|---|
| `400` | JSON không hợp lệ |
| `404` | Không tìm thấy session cho `callId` |

---

## Lifecycle session

```
BEGIN           → Không tạo session
ANSWER          → Tạo {callId}-tcore + {callId}-taccess (status: active)
CALL_CONTROL    → DCAS → DCSF (async)
CTRL_RESULT     → Cập nhật callback URL + termination ID
DELETE          → Đóng cả 2 session: DELETE /v1/vonras/call-sessions/{callId}
```

Nếu CALL_CTRL thất bại (non-2xx hoặc timeout), DCAS tự đóng cả 2 session mà không chờ DCSF.

---

## Cấu hình liên quan

```yaml
dcsf:
  hosts:
    - host: dcsf-host
      port: 8080
  call_control_timeout_ms: 30000

gateway:
  public_url: "http://dcas-host:8080"   # dùng để build callbackUrl gửi trong CALL_CTRL
  rtp_public_ip: "10.0.1.5"             # IP trong SDP response ANSWER
```
