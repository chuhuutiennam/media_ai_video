# Metrics Reference — media-ai-gateway

Endpoint: `GET /metrics` (Prometheus text format v0.0.4)

Tổng cộng **37 metric**, chia theo 9 subsystem.

> Để kiểm tra **trạng thái kết nối** tức thì (AI gRPC state, callback H/2, RTP ports đang mở), dùng `GET /v1/connections` — xem [Connection Status API](#connection-status-api) ở cuối tài liệu.

---

## 1. Session Manager

Theo dõi vòng đời session (tạo / đóng / đang hoạt động).

| Metric | Type | Ý nghĩa |
|--------|------|---------|
| `media_ai_sessions_active` | Gauge | Số session đang active tại thời điểm scrape. Tăng khi POST `/v1/sessions`, giảm khi DELETE hoặc GC thu hồi session idle. |
| `media_ai_sessions_max` | Gauge | Giới hạn session tối đa được cấu hình (`max_sessions`). Dùng để tính **% utilization** = `active / max`. |
| `media_ai_sessions_created_total` | Counter | Tổng lũy kế số session đã được tạo thành công kể từ khi khởi động. Không giảm. |
| `media_ai_sessions_closed_total` | Counter | Tổng lũy kế số session đã bị đóng (bao gồm DELETE thủ công và GC idle). Không giảm. |

**Cách dùng:**
- `sessions_active / sessions_max` → % capacity, alert khi > 80%.
- `sessions_created_total - sessions_closed_total` → cross-check với `sessions_active`; chênh lệch lớn có thể là dấu hiệu session bị "rò rỉ".
- Rate của `sessions_created_total` → lưu lượng call đến.

---

## 2. AI gRPC Stream Manager

Theo dõi gRPC stream tới AI worker. Gateway dùng **shared connection pool** — một `grpc.ClientConn` per worker address dùng chung cho tất cả session (HTTP/2 multiplexing). Mỗi session chỉ mở thêm một `grpc.ClientStream` mới trên connection chung đó. Keepalive PING được gửi mỗi `keepalive_time_sec` giây để giữ connection sống qua đoạn im lặng hoặc firewall NAT.

| Metric | Type | Ý nghĩa |
|--------|------|---------|
| `media_ai_ai_streams_active` | Gauge | Số gRPC stream đang mở tới AI worker. Bằng `sessions_active` trong điều kiện bình thường; thấp hơn nếu có session bị lỗi kết nối AI. |
| `media_ai_ai_streams_max` | Gauge | Giới hạn stream tối đa (`max_active_streams`). |
| `media_ai_ai_send_errors_total` | Counter | Tổng lỗi khi gửi `AudioChunk` lên AI worker qua gRPC. Tăng khi mạng giữa gateway và AI bị gián đoạn hoặc AI worker bị quá tải. Không đếm lỗi context-canceled. |
| `media_ai_ai_recv_errors_total` | Counter | Tổng lỗi khi nhận `RecognitionResult` từ AI worker. Tăng khi stream bị cắt ngang bất thường. Không đếm context-canceled (session delete bình thường). |
| `media_ai_ai_reconnects_total` | Counter | Tổng số lần reconnect gRPC stream (retry sau lỗi). Mỗi lần tăng nghĩa là một stream đã bị đứt và được dial lại thành công. |

**Cách dùng:**
- `ai_send_errors_total` hoặc `ai_recv_errors_total` tăng đột biến → vấn đề mạng hoặc AI worker crash.
- `ai_reconnects_total` tăng liên tục → AI worker không ổn định; kết hợp với `ai_send_errors_total` để phân biệt lỗi mạng vs quá tải.
- `ai_streams_active` thấp hơn `sessions_active` → một số session đang không có AI stream (chờ retry hoặc hết retry).
- `ai_streams_active = 0` khi có session → shared connection bị mất (keepalive timeout, AI worker down); gRPC tự reconnect khi có stream mới.

> **Connection state**: Kiểm tra trạng thái shared connection qua log startup `"ai: gRPC connection initiated"` (state `CONNECTING` → `READY` sau handshake). Không có metric riêng cho connection-level state — theo dõi qua `ai_send_errors_total` và `ai_recv_errors_total`.

---

## 3. Worker Pool (Audio Pipeline)

Theo dõi hàng đợi decode/resample/chunk của worker pool.

| Metric | Type | Ý nghĩa |
|--------|------|---------|
| `media_ai_pool_sessions_active` | Gauge | Số session đang được đăng ký trong worker pool (có pipeline decode đang chạy). |
| `media_ai_pool_queue_len` | Gauge | Số AudioJob đang chờ xử lý trong queue. Tăng khi các worker không kịp decode. |
| `media_ai_pool_queue_cap` | Gauge | Dung lượng tối đa của queue (`pool_queue_size`). Không đổi theo thời gian. |
| `media_ai_pool_submitted_total` | Counter | Tổng số AudioJob đã được submit vào pool (mỗi RTP packet sau jitter buffer = 1 job). |
| `media_ai_pool_dropped_total` | Counter | Tổng số job bị drop vì queue đầy. Mỗi drop = 1 RTP packet bị mất trước khi decode. |
| `media_ai_pool_processed_total` | Counter | Tổng số job đã được worker pool xử lý thành công (decode → resample → chunk → send AI). |
| `media_ai_pool_decode_errors_total` | Counter | Tổng lỗi decode payload (codec không hợp lệ, dữ liệu corrupt). Không bao gồm drop do queue đầy. |

**Cách dùng:**
- `pool_queue_len / pool_queue_cap` → % sử dụng queue; alert khi > 70% (sắp drop).
- `pool_dropped_total / pool_submitted_total` → drop rate; > 1% là dấu hiệu worker pool cần scale up.
- `pool_decode_errors_total` tăng → kiểm tra codec config và RTP payload từ nguồn gửi.
- `pool_processed_total` ≈ `pool_submitted_total - pool_dropped_total` (bình thường).

---

## 4. Jitter Buffer

Theo dõi chất lượng mạng RTP. Số liệu được tổng hợp (aggregate) từ tất cả session đang active.

| Metric | Type | Ý nghĩa |
|--------|------|---------|
| `media_ai_jitter_received_total` | Counter | Tổng RTP packet nhận vào jitter buffer (qua tất cả session). Bao gồm cả packet đến trễ. |
| `media_ai_jitter_released_total` | Counter | Tổng packet được jitter buffer phát ra đúng thứ tự (sequence in-order). Đây là packet thực sự được decode. |
| `media_ai_jitter_dropped_total` | Counter | Tổng packet bị jitter buffer loại bỏ vì: (1) đến trễ hơn `max_late_ms`, (2) trùng sequence number (duplicate), hoặc (3) channel output đầy. |
| `media_ai_jitter_lost_total` | Counter | Tổng số gap được phát hiện — packet không bao giờ đến trước khi timeout `buffer_ms`. Mỗi đơn vị = 1 packet bị mất trên đường truyền. |

**Cách dùng:**
- `jitter_lost_total` → tỉ lệ mất gói mạng; > 1% là mạng kém.
- `jitter_dropped_total` cao nhưng `jitter_lost_total` thấp → packet đến nhưng trễ quá; cân nhắc tăng `buffer_ms` hoặc `max_late_ms`.
- `jitter_released_total / jitter_received_total` → hiệu suất ordering; lý tưởng ≈ 1.0.
- Vì là aggregate của tất cả session, rate tăng đột biến tương quan với số session active.

---

## 5. Result Dispatcher

Theo dõi luồng kết quả nhận dạng (ASR) từ AI worker tới callback / DataChannel.

| Metric | Type | Ý nghĩa |
|--------|------|---------|
| `media_ai_dispatcher_queue_len` | Gauge | Số `RecognitionResult` đang chờ trong queue của dispatcher. |
| `media_ai_dispatcher_pushed_total` | Counter | Tổng kết quả được push vào dispatcher queue (bao gồm cả những cái bị drop sau đó). |
| `media_ai_dispatcher_sent_total` | Counter | Tổng kết quả đã được deliver thành công tới sink (callback + DataChannel cộng lại). |
| `media_ai_dispatcher_send_errors_total` | Counter | Tổng lỗi khi sink (HTTP callback, DataChannel) không nhận được sau tất cả retry. |
| `media_ai_result_partial_total` | Counter | Tổng kết quả **partial** (`is_final=false`) đã deliver thành công. Partial là transcript trung gian, cập nhật liên tục trong lúc nói. |
| `media_ai_result_final_total` | Counter | Tổng kết quả **final** (`is_final=true`) đã deliver thành công. Final là transcript xác nhận cuối của một đoạn lời nói. |
| `media_ai_result_queue_dropped_total` | Counter | Tổng kết quả bị drop khỏi queue dispatcher khi queue đầy. Partial bị drop ưu tiên trước final (drop policy mặc định). |

**Cách dùng:**
- `dispatcher_pushed_total - dispatcher_sent_total - result_queue_dropped_total` → số kết quả đang in-flight.
- `result_queue_dropped_total` tăng → dispatcher queue bị tắc, callback endpoint phản hồi quá chậm.
- `dispatcher_send_errors_total` tăng → callback URL không phản hồi hoặc trả về lỗi 5xx liên tục.
- `result_final_total` là số transcript hoàn chỉnh đã giao đến backend; đây là metric nghiệp vụ quan trọng nhất.

---

## 6. HTTP Callback

Theo dõi retry khi gửi kết quả tới callback URL.

| Metric | Type | Ý nghĩa |
|--------|------|---------|
| `media_ai_callback_retry_total` | Counter | Tổng số lần retry HTTP/2 POST tới callback URL (không tính lần gửi đầu tiên). Tăng khi callback endpoint trả về 5xx hoặc lỗi mạng. |

**Cách dùng:**
- `callback_retry_total` tăng liên tục → callback endpoint không ổn định; kết hợp `dispatcher_send_errors_total` để xác định bao nhiêu kết quả bị mất hẳn.
- Nếu `callback_retry_total` tăng nhưng `dispatcher_send_errors_total` không tăng → retry thành công (transient error).

---

## 7. RTP Ingress (Raw RTP)

Theo dõi luồng datagram UDP đầu vào từ nguồn RTP.

| Metric | Type | Ý nghĩa |
|--------|------|---------|
| `media_ai_rtp_packets_total` | Counter | Tổng datagram UDP nhận được trên socket RTP (bao gồm cả packet bị lọc/drop sau đó). |
| `media_ai_rtp_packets_routed_total` | Counter | Tổng RTP packet đã được route thành công vào `PacketQueue` của session tương ứng. |
| `media_ai_rtp_queue_dropped_total` | Counter | Packet bị drop vì `PacketQueue` của session đầy (jitter buffer chưa kịp drain). |
| `media_ai_rtp_unknown_ssrc_total` | Counter | Packet bị drop vì SSRC không khớp session nào đang active. Thường gặp khi packet đến sau khi session đã đóng. |
| `media_ai_rtp_parse_errors_total` | Counter | Packet bị drop vì lỗi parse RTP header (datagram quá ngắn hoặc malformed). |

**Cách dùng:**
- `rtp_packets_routed_total / rtp_packets_total` → tỉ lệ route thành công; lý tưởng ≈ 1.0.
- `rtp_queue_dropped_total` tăng → jitter buffer hoặc worker pool đang bottleneck; kết hợp `pool_queue_len`.
- `rtp_unknown_ssrc_total` tăng → nguồn RTP gửi sai SSRC, hoặc session bị đóng trước khi nguồn dừng gửi.
- `rtp_parse_errors_total` khác 0 → vấn đề phía nguồn gửi (SIP B2BUA, SBC).

> Các metric RTP chỉ xuất hiện khi dùng **shared ingress** (`source_type=raw_rtp`, single UDP socket).  
> Per-session listener (port pool) không expose metric riêng.

---

## 8. RTP Port Pool

Theo dõi pool port UDP dành riêng cho từng session (`source_type=raw_rtp`).

| Metric | Type | Ý nghĩa |
|--------|------|---------|
| `media_ai_rtp_ports_available` | Gauge | Số port UDP còn trống trong pool. |
| `media_ai_rtp_ports_total` | Gauge | Tổng port trong pool (`rtp_port_end - rtp_port_start + 1`). Không đổi theo thời gian. |

**Cách dùng:**
- `rtp_ports_available / rtp_ports_total` → % còn trống; alert khi < 20%.
- `rtp_ports_available = 0` → POST `/v1/sessions` sẽ trả về `503 no RTP ports available`.

> Chỉ xuất hiện khi `rtp_port_start > 0` được cấu hình.

---

## 9. System

Theo dõi tài nguyên process.

| Metric | Type | Ý nghĩa |
|--------|------|---------|
| `media_ai_goroutines_current` | Gauge | Số goroutine Go đang chạy tại thời điểm scrape. Mỗi session tạo ~6 goroutine; tăng tuyến tính với `sessions_active`. |
| `media_ai_memory_usage_bytes` | Gauge | Heap allocation hiện tại (bytes) theo `runtime.MemStats.Alloc`. Không bao gồm RSS hay stack. |
| `media_ai_gateway_nodes_registered` | Gauge | Số gateway node đang được đăng ký trong in-memory registry (heartbeat còn fresh). Dùng cho multi-node deploy. |
| `media_ai_scrape_timestamp_ms` | Gauge | Unix timestamp (milliseconds) của lần scrape này. Dùng để phát hiện stale metrics (scrape bị block). |

**Cách dùng:**
- `goroutines_current` / `sessions_active` ≈ 6–8 goroutine/session (bình thường); đột biến lớn hơn gợi ý goroutine leak.
- `memory_usage_bytes` tăng liên tục không có GC plateau → memory leak.
- `gateway_nodes_registered` < số node thực → một node đã tắt hoặc mạng bị phân mảnh.
- `scrape_timestamp_ms` dùng để tính `scrape_age = now - scrape_timestamp_ms`; alert khi > 2× scrape interval.

---

## Tổng hợp: drop rate toàn pipeline

Bảng theo dõi tỉ lệ mất dữ liệu qua từng tầng:

| Tầng | Metric drop | Metric tổng | Tỉ lệ lý tưởng |
|------|-------------|-------------|----------------|
| UDP ingress | `rtp_queue_dropped + rtp_unknown_ssrc + rtp_parse_errors` | `rtp_packets_total` | < 0.1% |
| Jitter buffer | `jitter_dropped + jitter_lost` | `jitter_received` | < 1% (dropped), < 0.5% (lost) |
| Worker pool | `pool_dropped` | `pool_submitted` | < 0.1% |
| Dispatcher | `result_queue_dropped` | `dispatcher_pushed` | 0% |
| Callback | `dispatcher_send_errors` | `dispatcher_sent` | 0% |

---

## Connection Status API

`GET /v1/connections` trả về trạng thái kết nối tức thì — không cần đợi scrape Prometheus.

### Request

```bash
curl -s --http2-prior-knowledge http://gateway:8080/v1/connections | python3 -m json.tool
```

### Response

```json
{
  "ai_workers": [
    {
      "addr": "127.0.0.1:50051",
      "state": "READY"
    }
  ],
  "callback": {
    "url": "http://127.0.0.1:9999/",
    "connected": true,
    "preconnect_at": "2026-06-26T10:00:00Z"
  },
  "rtp": {
    "per_session_open": 3,
    "per_session_capacity": 100,
    "shared_ingress": true
  }
}
```

### Giải thích từng field

**`ai_workers[]`**

| Field | Ý nghĩa |
|-------|---------|
| `addr` | Địa chỉ AI worker đang có conn trong pool |
| `state` | Trạng thái gRPC connection: `IDLE` (chưa dùng), `CONNECTING` (đang dial), `READY` (kết nối sẵn sàng), `TRANSIENT_FAILURE` (lỗi tạm thời, đang retry), `SHUTDOWN` (đã đóng) |

`state` là `CONNECTING` ngay sau startup và chuyển sang `READY` sau khi mock-ai-worker/AI worker chấp nhận TCP. Nếu AI worker down, state chuyển về `TRANSIENT_FAILURE` sau timeout.

**`callback`**

| Field | Ý nghĩa |
|-------|---------|
| `url` | URL đã thực hiện preconnect |
| `connected` | `true` nếu HEAD request lúc startup thành công (HTTP 2xx) |
| `error` | Lý do thất bại nếu `connected: false` |
| `preconnect_at` | Thời điểm preconnect được thực hiện (RFC3339 UTC) |

> Nếu `callback.url` không được cấu hình (`callback: url: ""`), toàn bộ object `callback` trả về rỗng.

**`rtp`**

| Field | Ý nghĩa |
|-------|---------|
| `per_session_open` | Số port UDP đang cấp phát = số session đang dùng per-session RTP port |
| `per_session_capacity` | Tổng số port trong pool (`rtp_port_end - rtp_port_start + 1`); `0` nếu pool không cấu hình |
| `shared_ingress` | `true` nếu shared UDP ingress (`:5004`) đang chạy |
