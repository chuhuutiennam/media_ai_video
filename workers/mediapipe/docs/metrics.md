# Hướng dẫn giám sát — mediapipe-video

Tài liệu mô tả từng metric được expose tại `http://<server>:9101/metrics`,
ý nghĩa vận hành, ngưỡng cảnh báo khuyến nghị, và hướng xử lý khi vượt ngưỡng.

---

## Mục lục

1. [Tổng quan kiến trúc](#1-tổng-quan-kiến-trúc)
2. [Streams](#2-streams)
3. [Segmenters (GPU)](#3-segmenters-gpu)
4. [Frames](#4-frames)
5. [Latency](#5-latency)
6. [Frame characteristics](#6-frame-characteristics)
7. [Ngưỡng cảnh báo tổng hợp](#7-ngưỡng-cảnh-báo-tổng-hợp)
8. [Dashboard Grafana](#8-dashboard-grafana)

---

## 1. Tổng quan kiến trúc

```
Gateway ──stream──► gRPC handler (thread) ──frame──► MediaPipe ImageSegmenter
                         │                                      │
                    ACTIVE_STREAMS                    ACTIVE_SEGMENTERS
                    STREAMS_TOTAL                     FRAMES_TOTAL
                                                      INFERENCE_LATENCY
                                                      FRAME_DROPS_TOTAL
```

Mỗi gRPC stream tạo **một** `ImageSegmenter` riêng (một thread MediaPipe). Các metric
chia thành hai lớp: **stream-level** (lifecycle kết nối) và **frame-level** (từng frame
xử lý bên trong stream).

---

## 2. Streams

### `mediapipe_video_active_streams` — Gauge

**Đo:** Số stream gRPC đang xử lý **tại thời điểm hiện tại**.

**Tăng** khi gateway mở kết nối mới. **Giảm** khi stream kết thúc (bình thường, lỗi,
hoặc bị hủy).

| Trạng thái | Ý nghĩa |
|---|---|
| 0 | Không có client kết nối — server nhàn rỗi |
| 1–`max_streams` | Bình thường |
| = `max_streams` | Đạt giới hạn — stream mới bị từ chối (gRPC `RESOURCE_EXHAUSTED`) |

**Ngưỡng cảnh báo:**

| Mức | Điều kiện | Hành động |
|---|---|---|
| WARNING | > `max_streams × 0.8` trong > 5 phút | Chuẩn bị tăng `max_streams` hoặc scale out |
| CRITICAL | = `max_streams` liên tục > 1 phút | Gateway đang bị từ chối — scale out ngay |

```promql
# Alert: gần đạt giới hạn stream
mediapipe_video_active_streams / <max_streams> > 0.8
```

---

### `mediapipe_video_streams_total` — Counter

**Đo:** Tổng số stream **đã hoàn thành** kể từ khi server khởi động (cả bình thường lẫn lỗi).

**Dùng để tính:** throughput stream (số session/giây), tổng tải tích lũy.

```promql
# Stream throughput (sessions/phút)
rate(mediapipe_video_streams_total[5m]) * 60
```

**Lưu ý:** Counter này không phân biệt thành công / lỗi. Kết hợp với
`mediapipe_video_frames_total{status="error"}` để suy luận quality.

---

## 3. Segmenters (GPU)

### `mediapipe_video_active_segmenters` — Gauge

**Đo:** Số `ImageSegmenter` instance đang giữ tài nguyên GPU/CPU **tại thời điểm hiện tại**.

**Ý nghĩa với GPU:** Mỗi segmenter chiếm một **EGL context**. Driver GPU có giới hạn
số EGL contexts per process (thường 8–16 tùy model). Nếu vượt giới hạn, segmenter
mới không tạo được → stream trả lỗi `RESOURCE_EXHAUSTED`.

> Thông thường `active_segmenters == active_streams`. Giá trị lớn hơn cho thấy
> một số segmenter chưa được giải phóng (cleanup chậm sau khi stream kết thúc).

| Trạng thái | Ý nghĩa |
|---|---|
| = `active_streams` | Bình thường |
| > `active_streams` | Cleanup trễ — theo dõi thêm |
| Tăng vô hạn | Leak segmenter — cần điều tra ngay |

**Ngưỡng cảnh báo:**

| Mức | Điều kiện | Hành động |
|---|---|---|
| WARNING | > `max_concurrent_segmenters × 0.8` | Sắp đạt giới hạn EGL |
| CRITICAL | = `max_concurrent_segmenters` liên tục | Giảm `max_streams` hoặc tăng `max_concurrent_segmenters` |
| CRITICAL | `active_segmenters > active_streams + 2` trong > 30s | Khả năng leak — restart service |

```promql
# Segmenter leak detector
mediapipe_video_active_segmenters - mediapipe_video_active_streams > 2
```

---

## 4. Frames

### `mediapipe_video_frames_total{status="ok"|"error"}` — Counter

**Đo:** Tổng số frame đã xử lý, phân theo kết quả.

- `status="ok"`: MediaPipe inference thành công, mask đã được gửi về gateway.
- `status="error"`: Frame bị lỗi trong quá trình xử lý (xem chi tiết bên dưới).

**Nguyên nhân `status="error"` thường gặp:**

| Lỗi | Nguyên nhân | Xử lý |
|---|---|---|
| `encoding must be jpeg` | Gateway gửi frame không phải JPEG | Kiểm tra codec phía gateway |
| `JPEG dimensions do not match config` | Kích thước frame không khớp `inference_width/height` trong VideoConfig | Gateway cần resize đúng trước khi gửi |
| `timestamp_ms must increase` | Gateway gửi frame với timestamp lùi hoặc bằng frame trước | Kiểm tra logic timestamp phía gateway |
| `MediaPipe inference error` | Lỗi nội bộ MediaPipe | Xem log chi tiết, có thể do model incompatible |

**Ngưỡng cảnh báo:**

| Mức | Điều kiện | Hành động |
|---|---|---|
| WARNING | Error rate > 1% trong 5 phút | Kiểm tra log lỗi cụ thể |
| CRITICAL | Error rate > 5% trong 2 phút | Khả năng lỗi hệ thống — điều tra ngay |

```promql
# Error rate (%)
rate(mediapipe_video_frames_total{status="error"}[5m])
  / rate(mediapipe_video_frames_total[5m]) * 100

# Throughput (frame/s)
rate(mediapipe_video_frames_total{status="ok"}[1m])
```

---

### `mediapipe_video_frame_drops_total` — Counter

**Đo:** Số frame bị **MediaPipe LIVE_STREAM drop** — tức là callback không được gọi cho
frame đó (MediaPipe bỏ qua frame khi đang bận xử lý frame trước).

**Ý nghĩa:**

LIVE_STREAM mode của MediaPipe được thiết kế để ưu tiên real-time: nếu inference của
frame T chưa xong mà frame T+1 đến, T+1 có thể bị drop. Đây là hành vi **dự kiến** khi
GPU overload hoặc frame rate quá cao so với inference speed.

> **Lưu ý sau khi chuyển sang xử lý tuần tự từng frame:** mỗi stream chỉ có tối đa một
> frame trên GPU (submit → chờ callback → trả response rồi mới lấy frame kế tiếp), nên Worker
> không còn tự gây drop bằng cách submit chồng. Metric này được kỳ vọng gần như luôn bằng 0
> (suy luận từ code, chưa kiểm chứng trên GPU). Khi quá tải, tín hiệu chính là
> `inference_latency_seconds` tăng (hàng đợi `decode_queue` đầy, producer bị chặn, flow
> control gRPC làm chậm Gateway) chứ không phải `frame_drops_total`. Hiện chưa có metric
> theo dõi độ sâu `decode_queue`.
>
> **Phân biệt với `status="error"`:** Drop xảy ra bên trong MediaPipe trước khi
> inference bắt đầu. Frame dropped không tính vào `frames_total` — chỉ tính riêng
> tại đây.

| Mức drop | Ý nghĩa |
|---|---|
| 0 | Lý tưởng — GPU đủ sức xử lý mọi frame |
| Thỉnh thoảng | Bình thường khi có burst traffic |
| Liên tục > 5% tổng frame | GPU overload — cần giảm FPS hoặc scale |

**Ngưỡng cảnh báo:**

| Mức | Điều kiện | Hành động |
|---|---|---|
| WARNING | Drop rate > 2% tổng frame trong 5 phút | Xem xét giảm `inference_fps` phía gateway |
| CRITICAL | Drop rate > 10% trong 2 phút | GPU overload — giảm số stream hoặc thêm server |

```promql
# Drop rate so với tổng frame gửi vào
rate(mediapipe_video_frame_drops_total[5m])
  / (rate(mediapipe_video_frames_total[5m]) + rate(mediapipe_video_frame_drops_total[5m]))
  * 100
```

---

## 5. Latency

### `mediapipe_video_inference_latency_seconds` — Histogram

**Đo:** Thời gian xử lý **end-to-end mỗi frame** tính từ lúc worker nhận frame đến
lúc yield `VideoResponse`. Bao gồm:

```
JPEG decode  +  numpy convert  +  MediaPipe inference  +  RLE encode
```

Đồng hồ bắt đầu ngay khi producer nhận frame nên giá trị còn gồm **thời gian frame nằm
chờ trong `decode_queue`** khi consumer đang bận frame trước (tối đa khoảng 2–3 frame).
Khi frame đến nhanh hơn khả năng xử lý, latency tăng lên do hàng đợi, không chỉ do
inference chậm. Response được yield ngay sau khi frame xử lý xong nên không cộng thêm
thời gian chờ frame kế tiếp.

**Không bao gồm:** thời gian frame nằm chờ trong network buffer của gRPC.

**Buckets (giây):** `0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0`

**Số đo thực tế (2026-09-29, CPU Intel i5-13420H, delegate CPU, `selfie_segmenter.tflite`,
frame đẩy hết tốc lực, 200 frame/stream):**

| Độ phân giải | Số stream | FPS mỗi stream | `latency_ms` P50 / P95 |
|---|---|---|---|
| 256×256 | 1 | 206,2 | 15,9 / 18,3 |
| 256×256 | 4 | 127,5 | 24,8 / 28,7 |
| 640×360 | 1 | 121,7 | 20,7 / 97,7 |
| 640×360 | 4 | 62,1 | 39,8 / 89,3 |
| 1280×720 | 1 | 46,0 | 73,2 / 119,4 |
| 1280×720 | 4 | 26,5 | 130,3 / 173,8 |

Chi tiết và giới hạn phép đo: `docs/technical.md` mục 7.4. Chưa có số đo trên GPU server.

**Ước tính ban đầu (chưa được đo, không dùng làm chuẩn; số CPU bên dưới cao hơn 3–5 lần so
với số đo thực ở trên):**

| Percentile | GPU (A10, 640×360) (ms) | CPU (ms) |
|---|---|---|
| P50 | 15–25 | 80–150 |
| P95 | 30–50 | 150–300 |
| P99 | 50–100 | 300–600 |

> Giá trị thực tế phụ thuộc: model size, resolution, GPU model, số stream đồng thời.
> Các ngưỡng cảnh báo bên dưới cần được hiệu chỉnh lại sau khi có số đo trên GPU server.

**Ngưỡng cảnh báo (GPU mode, 640×360):**

| Mức | Điều kiện | Hành động |
|---|---|---|
| WARNING | P95 > 100ms trong 5 phút | GPU bắt đầu quá tải — kiểm tra `active_streams` |
| CRITICAL | P99 > 500ms trong 2 phút | Độ trễ ảnh hưởng real-time — giảm tải ngay |
| CRITICAL | P50 > 200ms | GPU gặp vấn đề nghiêm trọng — kiểm tra driver/temperature |

```promql
# P50 latency (ms)
histogram_quantile(0.50,
  rate(mediapipe_video_inference_latency_seconds_bucket[5m])
) * 1000

# P95 latency (ms)
histogram_quantile(0.95,
  rate(mediapipe_video_inference_latency_seconds_bucket[5m])
) * 1000

# P99 latency (ms)
histogram_quantile(0.99,
  rate(mediapipe_video_inference_latency_seconds_bucket[5m])
) * 1000
```

**Nguyên nhân latency tăng đột biến:**

| Triệu chứng | Nguyên nhân | Xử lý |
|---|---|---|
| P99 tăng, P50 ổn | Outlier đơn lẻ (GC, context switch) | Bình thường, không cần xử lý |
| P50 + P95 cùng tăng | GPU overload (nhiều stream đồng thời) | Giảm `max_streams`, kiểm tra `active_segmenters` |
| Latency tăng dần theo thời gian | GPU thermal throttling | Kiểm tra nhiệt độ GPU: `nvidia-smi -q -d TEMPERATURE` |
| Đột ngột tăng sau deploy | Model mới nặng hơn hoặc resolution tăng | So sánh `frame_bytes` trước/sau deploy |

---

## 6. Frame characteristics

### `mediapipe_video_frame_bytes` — Histogram

**Đo:** Kích thước JPEG input mỗi frame (bytes).

**Buckets (bytes):** `4K, 16K, 64K, 128K, 256K, 512K, 1M`

**Ý nghĩa:**

Frame lớn hơn không nhất thiết chậm hơn (inference time phụ thuộc resolution, không
phụ thuộc JPEG file size). Tuy nhiên frame size lớn báo hiệu:
- Tỉ lệ nén JPEG thấp → gateway có thể tối ưu
- Resolution đầu vào lớn → tăng thời gian decode

**Dùng để:** Phát hiện thay đổi cấu hình gateway (resolution, JPEG quality thay đổi),
điều tra nguyên nhân latency tăng.

```promql
# Kích thước frame trung bình (KB)
rate(mediapipe_video_frame_bytes_sum[5m])
  / rate(mediapipe_video_frame_bytes_count[5m]) / 1024

# Tổng băng thông JPEG nhận vào (KB/s)
rate(mediapipe_video_frame_bytes_sum[1m]) / 1024
```

---

### `mediapipe_video_foreground_ratio` — Histogram

**Đo:** Tỉ lệ pixel được phân loại là **foreground** (người) trong mask kết quả, từ 0.0 đến 1.0.

**Buckets:** `0.05, 0.10, 0.20, 0.30, 0.40, 0.50, 0.60, 0.70, 0.80, 0.90, 0.95`

**Giá trị tham chiếu:**

| Giá trị | Ý nghĩa |
|---|---|
| 0.05–0.40 | Bình thường — người chiếm phần nhỏ frame (ảnh full body từ xa) |
| 0.40–0.70 | Người chiếm phần lớn frame (bust shot, close-up) |
| > 0.90 | Toàn bộ frame là foreground — nghi ngờ model/threshold không đúng |
| ≈ 0.0 liên tục | Không detect được người — kiểm tra `threshold`, model, hoặc input sai |

**Ngưỡng cảnh báo:**

| Mức | Điều kiện | Hành động |
|---|---|---|
| WARNING | P50 foreground ratio > 0.85 liên tục | Threshold quá thấp hoặc model không phù hợp scene |
| WARNING | P50 foreground ratio < 0.01 liên tục | Không detect được người — kiểm tra input và model |

```promql
# Foreground ratio trung bình
rate(mediapipe_video_foreground_ratio_sum[5m])
  / rate(mediapipe_video_foreground_ratio_count[5m])
```

---

## 7. Ngưỡng cảnh báo tổng hợp

Bảng này tóm tắt tất cả ngưỡng cho Prometheus Alertmanager:

```yaml
# alerting_rules.yml
groups:
  - name: mediapipe-video
    rules:

      # Stream capacity
      - alert: StreamCapacityHigh
        expr: mediapipe_video_active_streams / <max_streams> > 0.8
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Gần đạt giới hạn stream ({{ $value | humanizePercentage }})"

      - alert: StreamCapacityFull
        expr: mediapipe_video_active_streams >= <max_streams>
        for: 1m
        labels:
          severity: critical
        annotations:
          summary: "Đạt giới hạn stream — gateway đang bị từ chối"

      # GPU EGL context
      - alert: SegmenterLeak
        expr: mediapipe_video_active_segmenters - mediapipe_video_active_streams > 2
        for: 30s
        labels:
          severity: critical
        annotations:
          summary: "Nghi ngờ leak ImageSegmenter (segmenters={{ $value }})"

      # Frame errors
      - alert: FrameErrorRateHigh
        expr: >
          rate(mediapipe_video_frames_total{status="error"}[5m])
            / rate(mediapipe_video_frames_total[5m]) > 0.01
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Frame error rate {{ $value | humanizePercentage }}"

      - alert: FrameErrorRateCritical
        expr: >
          rate(mediapipe_video_frames_total{status="error"}[2m])
            / rate(mediapipe_video_frames_total[2m]) > 0.05
        for: 2m
        labels:
          severity: critical
        annotations:
          summary: "Frame error rate cao bất thường {{ $value | humanizePercentage }}"

      # Frame drops
      - alert: FrameDropRateHigh
        expr: >
          rate(mediapipe_video_frame_drops_total[5m])
            / (rate(mediapipe_video_frames_total[5m])
               + rate(mediapipe_video_frame_drops_total[5m])) > 0.02
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "GPU có thể overload — frame drop {{ $value | humanizePercentage }}"

      - alert: FrameDropRateCritical
        expr: >
          rate(mediapipe_video_frame_drops_total[2m])
            / (rate(mediapipe_video_frames_total[2m])
               + rate(mediapipe_video_frame_drops_total[2m])) > 0.10
        for: 2m
        labels:
          severity: critical
        annotations:
          summary: "GPU overload — frame drop {{ $value | humanizePercentage }}"

      # Latency
      - alert: LatencyP95High
        expr: >
          histogram_quantile(0.95,
            rate(mediapipe_video_inference_latency_seconds_bucket[5m])) > 0.1
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "P95 latency {{ $value | humanizeDuration }} (ngưỡng 100ms)"

      - alert: LatencyP99Critical
        expr: >
          histogram_quantile(0.99,
            rate(mediapipe_video_inference_latency_seconds_bucket[2m])) > 0.5
        for: 2m
        labels:
          severity: critical
        annotations:
          summary: "P99 latency {{ $value | humanizeDuration }} — ảnh hưởng real-time"

      # Service down
      - alert: ServiceDown
        expr: up{job="mediapipe-video"} == 0
        for: 30s
        labels:
          severity: critical
        annotations:
          summary: "mediapipe-video không phản hồi Prometheus scrape"
```

---

## 8. Dashboard Grafana

### Cấu trúc dashboard khuyến nghị

**Row 1 — Health overview**

| Panel | Query | Viz |
|---|---|---|
| Active streams | `mediapipe_video_active_streams` | Stat (threshold: 0=green, `max_streams×0.8`=yellow, `max_streams`=red) |
| Active segmenters | `mediapipe_video_active_segmenters` | Stat |
| Frame throughput | `rate(mediapipe_video_frames_total{status="ok"}[1m])` | Stat (frame/s) |
| Error rate | `rate(...error...)/ rate(...total...) * 100` | Stat (threshold: 0=green, 1%=yellow, 5%=red) |

**Row 2 — Latency**

| Panel | Query | Viz |
|---|---|---|
| Latency P50/P95/P99 | `histogram_quantile(0.5/0.95/0.99, rate(...bucket[5m])) * 1000` | Time series (ms) |
| Latency heatmap | `rate(mediapipe_video_inference_latency_seconds_bucket[1m])` | Heatmap |

**Row 3 — Throughput & drops**

| Panel | Query | Viz |
|---|---|---|
| Frame rate ok vs error | `rate(mediapipe_video_frames_total[1m])` by status | Time series (stacked) |
| Drop rate | `rate(mediapipe_video_frame_drops_total[1m])` | Time series |
| Stream rate | `rate(mediapipe_video_streams_total[5m]) * 60` | Time series (sessions/min) |

**Row 4 — Frame characteristics**

| Panel | Query | Viz |
|---|---|---|
| Frame size distribution | `rate(mediapipe_video_frame_bytes_bucket[5m])` | Heatmap |
| Foreground ratio distribution | `rate(mediapipe_video_foreground_ratio_bucket[5m])` | Heatmap |
| Avg frame size | `rate(mediapipe_video_frame_bytes_sum[5m]) / rate(mediapipe_video_frame_bytes_count[5m]) / 1024` | Stat (KB) |

### Variables Grafana khuyến nghị

```
$job       = mediapipe-video        # filter theo job nếu có nhiều instance
$interval  = 5m                     # time window cho rate()
```
