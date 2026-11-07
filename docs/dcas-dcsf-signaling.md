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
  │── POST notify-event {ANSWER} ──────────────>│  [tạo session theo leg + cấp RTP port]
  │<─ 201 Created {rtp_ip, *_rtp_port, ...} ────│
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

**Mô tả:** DCSF thông báo cuộc gọi đã được trả lời và cung cấp dịch vụ AI cần xử lý. DCAS tạo session nội bộ theo dịch vụ, cấp RTP port, khởi động pipeline AI, rồi trả về thông tin RTP endpoint (HTTP **201**):

- Dịch vụ audio: 2 session `{callId}-tcore` và `{callId}-taccess`, mỗi session 1 port audio.
- Dịch vụ video: theo `legs` của danh mục `video.services`. Mặc định (`tcore`) chỉ có session
  `{callId}-tcore` với 1 port video; `legs: both` thêm `{callId}-taccess`.

Thứ tự kiểm tra trước khi tạo session: tra `selectedService` (không phân biệt hoa thường) →
kiểm tra tải gateway → điều kiện riêng của video (`video.enabled`, dải port, AI của dịch vụ còn
slot). Lỗi xảy ra sau khi đã tạo session thì DCAS đóng các session đó và trả port trước khi
trả lỗi.

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
| `callId` | string | ID cuộc gọi. DCAS dùng `{callId}` trên URL, không so với giá trị trong body |
| `event` | string | `"ANSWER"` |
| `selectedService` | string | Dịch vụ audio `"speech_to_text"` \| `"realtime_translation"`, hoặc dịch vụ video trong danh mục `video.services` (mặc định `"change_background"`, `"video_effects"`) — xác định media và AI |
| `direction` | string | `"MO"` \| `"MT"` |
| `role` | string | `"originator"` \| `"terminator"` |
| `dcasId` | string | Định danh DCAS module: `"dcas_stt"` \| `"dcas_rt"` \| `"dcas_fc"` \| `"dcas_vvc"` |
| `callbackKey` | string | Random 16-char hex — DCSF dùng để route CALL_CTRL vào đúng Erlang process |
| `callbackUrl` | string | URL DCAS POST CALL_CTRL về DCSF; format: `.../call-control/{dcasId}/{callbackKey}`. Bỏ trống → DCAS không gửi CALL_CTRL |

**Mapping `selectedService` → codec:**

| selectedService | Codec | Sample Rate | AI Task |
|---|---|---|---|
| `speech_to_text` | PCMU | 8000 Hz | `speech_to_text` |
| `realtime_translation` | PCMU | 8000 Hz | `realtime_translation` |
| *(khác)* | — | — | ACK 200 `{}`, không tạo session |

Dịch vụ audio không mở video kể cả khi `bearerCapability: VIDEO`.

Gateway chỉ video (`audio.enabled: false`, các config `gateway-video-*.yaml`): `speech_to_text` /
`realtime_translation` trả **400** `{"error":"service <tên> is not supported by this gateway (audio.enabled=false)"}`,
không tạo session, không cấp cổng — để DCSF biết đã gửi dịch vụ audio nhầm gateway (khác dịch vụ lạ: 200 `{}`).

**Mapping `selectedService` video** (danh mục `video.services`, mỗi dịch vụ **đúng một AI
service** — xem [giai-phap-video-service-catalog.md](giai-phap-video-service-catalog.md)):

| selectedService (mặc định) | Media | Leg | AI task |
|---|---|---|---|
| `change_background` | H264/90000 | chỉ tcore; kết quả về `callbackUrl` tAccess của ctrl-result | `video_segment` (repo AI mediapipe) |
| `video_effects` | H264/90000 | chỉ tcore; kết quả về `callbackUrl` tAccess của ctrl-result | `gesture_recognition` (repo AI gesture) |
| dịch vụ khai báo thêm trong `video.services` | H264/90000 | theo `legs` (mặc định `tcore`) | theo `ai.task` |
| dịch vụ tắt (`enabled: false`) | — | — | ACK 200 `{}`, không tạo session |
| AI của dịch vụ hết slot / mọi target mất kết nối / không có worker riêng | — | — | HTTP 503 `no AI worker available for service <tên>` |

### Response

`HTTP/1.1 201 Created`. Dịch vụ audio:

```json
{
  "session_id": "call-abc123",
  "status": "CREATED",
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

Dịch vụ video 1 leg (mặc định, ví dụ `video_effects`). Không có field audio (`codec`,
`sample_rate`, `*_rtp_port` audio):

```json
{
  "session_id": "call-abc123",
  "status": "CREATED",
  "source_type": "raw_rtp",
  "channels": 1,
  "task": "video_effects",
  "created_at": "2026-11-06T04:46:17.409384609Z",
  "gateway_id": "gw-ims-01",
  "rtp_ip": "10.0.1.5",
  "tcore_video_rtp_port": 38740,
  "tcore_local_non_dc_video_media": {
    "sdpmLine": "video 38740 RTP/AVP 96",
    "sdpaLines": [
      "rtpmap:96 H264/90000",
      "fmtp:96 packetization-mode=1;profile-level-id=42e01f",
      "sendrecv"
    ]
  }
}
```

| Field | Mô tả |
|---|---|
| `tcore_video_rtp_port` | UDP port nhận RTP video H264 của leg tcore |
| `tcore_local_non_dc_video_media` | SDP video của DCAS cho leg tcore |
| `taccess_video_rtp_port`, `taccess_local_non_dc_video_media` | Chỉ có với dịch vụ `legs: both` |

`status` là `CREATED` khi trả ANSWER; session chuyển `ACTIVE` khi nhận gói RTP đầu tiên.

**Lưu ý:** Response được flush ngay trước khi DCAS gửi CALL_CTRL (bước tiếp theo). DCSF không cần chờ CALL_CTRL để xử lý response ANSWER.

**Thời gian trả lời:** ANSWER video mở stream AI đồng bộ; mỗi leg chờ tối đa `video.ai.open_timeout_ms`
(mặc định 5000, gồm mọi lần thử target), hết hạn → 502. `Load` bắt buộc số leg × `open_timeout_ms` <
`server.write_timeout_sec` (mặc định 10), vì response trễ hơn `write_timeout_sec` bị HTTP server bỏ và
DCSF không nhận được gì. `open_timeout_ms: 0` tắt hạn chờ (AI không phản hồi có thể giữ ANSWER ~20s).

**Lỗi** (body `{"error": "...", "retry_after_ms": N}`; `retry_after_ms` chỉ có ở một số lỗi 503):

| HTTP | Nguyên nhân |
|---|---|
| `400` | JSON không hợp lệ; event không nhận dạng được; dịch vụ video khi `video.enabled=false` (`service <tên> requires video.enabled=true`); dịch vụ audio trên gateway chỉ video (`service <tên> is not supported by this gateway (audio.enabled=false)`) |
| `409` | `callId` đã tồn tại trong session manager |
| `503` | Gateway quá tải (`service at capacity, try again later`: ≥ 90% session, hàng đợi audio ≥ 80%, AI stream audio ≥ 95%, không có worker AI audio, heap vượt ngưỡng, hết port); đạt giới hạn session; hết RTP port (`no RTP ports available`); video không có dải port (`video requires a per-session RTP port pool`); AI của dịch vụ video hết slot hoặc mọi target mất kết nối (`no AI worker available for service <tên>`, `retry_after_ms: 3000`) |
| `502` | Lỗi khởi động pipeline hoặc kết nối AI: audio (`pipeline: ...`), video (`video pipeline: ...`), kể cả hết hạn mở stream AI video (`... timed out after 5s ...`, `video.ai.open_timeout_ms`) |
| `500` | Lỗi mở RTP listener hoặc lỗi tạo session khác |

---

## 3. CALL_CONTROL (CALL_CTRL)

### Chiều: DCAS → DCSF

**Endpoint:** `POST {callbackUrl}` — URL lấy từ trường `callbackUrl` trong ANSWER request body

**Mô tả:** Sau khi flush response ANSWER, DCAS gửi CALL_CTRL bất đồng bộ (goroutine riêng) để yêu cầu DCSF thiết lập tài nguyên H.248 trên Media Function (MF). DCAS yêu cầu action `"duplicate"` để MF tạo context với 2 termination (tCore + tAccess) và duplicate RTP stream về DCAS endpoint.

**Thời điểm gửi:** Ngay sau khi `c.Writer.Flush()` trả về trong handler ANSWER. Timeout mặc định 30s (cấu hình qua `dcsf.call_control_timeout_ms`). Chỉ gửi khi ANSWER có `callbackUrl`; nếu không, session vẫn mở tới khi có DELETE hoặc hết `session.idle_timeout_sec` không nhận gói RTP.

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

Bất kỳ status nào ngoài 2xx (hoặc timeout) → DCAS log warning và đóng các session của cuộc gọi (`{callId}-tcore`, và `{callId}-taccess` nếu có).

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
- Dịch vụ video 1 leg (`legs: tcore`, mặc định `change_background`, `video_effects`): chỉ có
  session `{callId}-tcore`. Session này nhận `callbackUrl` của **tAccess** và
  `mediaResources` của cả tCore lẫn tAccess. `callbackUrls` (URL riêng theo task, xem
  [luong-xu-ly-video-realtime-rtpgw-ai-mf.md](luong-xu-ly-video-realtime-rtpgw-ai-mf.md))
  vẫn lấy từ **tCore**; `callbackUrls` đặt trong tAccess bị bỏ qua.

### Response

```json
{
  "session_id": "call-abc123",
  "status": "ACTIVE",
  "source_type": "raw_rtp",
  "codec": "PCMU",
  "sample_rate": 8000,
  "channels": 1,
  "task": "speech_to_text",
  "created_at": "2026-08-10T10:00:00.000000+07:00"
}
```

`status` là `CREATED` nếu session chưa nhận gói RTP nào.

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
ANSWER          → Tạo {callId}-tcore (+ {callId}-taccess với audio hoặc video legs: both), status CREATED
RTP đầu tiên    → status ACTIVE
CALL_CONTROL    → DCAS → DCSF (async, chỉ khi ANSWER có callbackUrl)
CTRL_RESULT     → Cập nhật callback URL + termination ID
DELETE          → Đóng mọi session của cuộc gọi: DELETE /v1/vonras/call-sessions/{callId}
Idle            → Không nhận gói RTP quá session.idle_timeout_sec → DCAS tự đóng session
```

Nếu CALL_CTRL thất bại (non-2xx hoặc timeout), DCAS tự đóng các session của cuộc gọi mà không chờ DCSF.

---

## Cấu hình liên quan

```yaml
dcsf:
  hosts:
    - host: dcsf-host
      port: 8080
  call_control_timeout_ms: 30000

server:
  public_url: "http://dcas-host:8080"   # dùng để build callbackUrl gửi trong CALL_CTRL
  write_timeout_sec: 10                 # response ANSWER trễ hơn bị bỏ; > số leg × video.ai.open_timeout_ms

video:
  ai:
    open_timeout_ms: 5000               # hạn chờ mở stream AI khi ANSWER (0 = không giới hạn)

rtp:
  public_ip: "10.0.1.5"                 # IP trong SDP response ANSWER
  port_start: 20000                     # phải chẵn khi even_ports: true
  port_end: 29999
  even_ports: true                      # (mặc định) cổng RTP trong ANSWER luôn chẵn; cổng lẻ kế tiếp dành cho RTCP
```

Cấu hình mẫu triển khai IMS cho dịch vụ video: [config/gateway-video-ims.yaml](../config/gateway-video-ims.yaml).
Cổng video chỉ nhận RTP; gói RTCP gửi vào cổng này bị bỏ (metric
`media_ai_video_ingress_rtcp_dropped_total`).
