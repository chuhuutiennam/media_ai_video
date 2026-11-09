# Giải pháp: nhận RTP video từ MF — jitter riêng cho video và số liệu mất gói

Trạng thái: **đã triển khai** (chốt và code 2026-11-09, nhánh `feat/video-jitter-observability`).
Kết quả kiểm chứng ở §5.
Phạm vi đợt này: **A** (jitter riêng cho video) và **D** (số liệu decoder). Chưa làm: **B**
(reset khi đổi SSRC / seq nhảy lùi) và **C** (gửi PLI/FIR về MF).

## 1. Hiện trạng và kết quả đo

Luồng: MF → cổng UDP riêng mỗi leg (`StartVideoSessionListener`) → `VideoPacketQueue` →
jitter buffer (dùng chung config và thuật toán với audio) → `jitterOut` → `VideoWorkerPool`
→ `H264RtpParser` → libavcodec → lấy mẫu → JPEG → AI.

Đo bằng test tạm trên bản copy repo (WSL, CGO, FFmpeg 7.1.1), cấu hình như
`gateway-video-ims.yaml` (`jitter_buffer_ms` 80, `max_packet_late_ms` 200, `packet_time_ms` 20):

| # | Tình huống | Kết quả |
| --- | --- | --- |
| 1 | Mất 1 gói ở frame 12, IDR mỗi 10 frame | Frame 12→19 không ra; chạy lại từ IDR frame 20 |
| 2 | Mất liên tiếp 1 / 5 / 20 gói | Gói kế tiếp được nhả sau 20 ms / 420 ms / **1,92 s** |
| 3 | Seq mới lùi 10000 (giống SSRC mới) | 300/300 gói bị loại vì coi là trùng |
| 4 | Seq mới tiến 1000 | ~100 s mới tới gói mới; heap tăng không giới hạn |
| 5 | IDR 120 gói, `jitter_out_queue_size` 16 | Mất ~85% gói (1024: 0) |
| 6 | Từ lần nhả cuối tới gói kế 40 / 70 / 79 / 100 ms, gói đầu frame đến trễ 2 ms | Nhận / nhận / **mất** / **mất** |
| 7 | Mất gói ở decoder | `PacketGaps` = 0 dù có mất; `WaitIDRDrops` không bao giờ tăng |

Nguyên nhân chính:

- Jitter đếm hạn chờ gói thiếu từ **lần nhả gần nhất** + `buffer_ms` (#6), không từ lúc gói
  đứng sau khoảng trống đến. Video gửi theo từng loạt mỗi frame, nghỉ giữa 2 frame ≥ `buffer_ms`
  (≤ ~12 fps) thì mọi gói đảo thứ tự ở đầu frame đều bị coi là mất → decoder chờ IDR.
- Mỗi gói thiếu liên tiếp được khai báo mất từng gói một, mỗi gói chờ thêm một cửa sổ (#2, #4).
- Parser luôn bỏ hình tới IDR kế tiếp sau khi mất gói (#1); cờ `DropUntilIDROnLoss` không
  được C++ dùng, Go còn vô tình truyền 0.

## 2. Quyết định đã chốt

| # | Quyết định |
| --- | --- |
| 1 | Jitter video có cấu hình riêng `video.jitter.*`, **mặc định bật**. `enabled: false` giữ nguyên hành vi cũ (video dùng `pipeline.*` + `session.per_session_packet_queue`, thuật toán audio). Audio không đổi. |
| 2 | Khi bật: hạn chờ khoảng trống = lúc gói đứng sau khoảng trống đến + `buffer_ms`; hết hạn thì khai báo mất **cả khoảng trống một lần**; trần `max_packets` gói chờ mỗi session (vượt thì bỏ gói mới, đếm riêng). |
| 3 | **Không** bỏ gói quá cũ lúc nhả: bỏ gói lúc nhả tạo khoảng trống seq → đứt chuỗi tham chiếu → decoder chờ IDR (tệ hơn). Khi đã bỏ qua cả khoảng trống thì gói trễ tối đa ~`buffer_ms` + 1 chu kỳ flush. `max_late_ms` vẫn kiểm tra lúc vào jitter như cũ. |
| 4 | Số liệu decoder: đếm seq đứt trong parser, frame bị bỏ khi chờ IDR, số lần và tổng thời gian chờ IDR, số session đang chờ. Không thêm cờ bật/tắt (chỉ đếm và log). |
| 5 | **Xoá** cờ `DropUntilIDROnLoss` / `drop_until_idr_on_loss` (không được dùng). Hành vi giữ nguyên: mất gói luôn chờ IDR. |
| 6 | Làm trên worktree riêng từ HEAD (`bd50956`), nhánh `feat/video-jitter-observability`, không lẫn thay đổi chưa commit trên `main`. |
| 7 | Thêm cờ `--drop-packets` cho `mock-video-rtp-sender` để chạy thật kịch bản mất gói. |

## 3. Thiết kế

### 3.1 Cấu hình `video.jitter`

```yaml
video:
  jitter:
    enabled: true            # false = như cũ: dùng pipeline.* + session.per_session_packet_queue, thuật toán audio
    buffer_ms: 80            # chờ gói đảo thứ tự/thiếu, tính từ lúc gói sau khoảng trống đến (10..1000)
    max_late_ms: 200         # gói nằm trong hàng đợi trước jitter quá lâu thì bỏ (20..5000)
    packet_time_ms: 20       # chu kỳ flush (5..100, ≤ buffer_ms)
    packet_queue_size: 1024  # listener → jitter, mỗi session (16..65536)
    out_queue_size: 1024     # jitter → worker pool, mỗi session (16..65536)
    max_packets: 1024        # trần gói chờ trong jitter mỗi session (64..65536)
```

Chỉ kiểm tra giá trị khi `video.enabled` và `video.jitter.enabled`. Cấu hình chỉ đọc từ YAML
(`--config` / `GATEWAY_CONFIG`); không có env riêng. `deploy/k8s/configmap.yaml` không có mục
video nên không đổi.

### 3.2 Jitter (`internal/jitter`)

Thêm vào `jitter.Config` (giá trị 0 = hành vi cũ, audio dùng):

- `SkipGaps bool`: hạn chờ khoảng trống = `arrivedAt` của gói đầu heap + `BufferMs`; hết hạn
  thì `Lost += số seq thiếu`, nhảy thẳng tới gói đầu heap.
- `MaxPackets int`: `Push` khi heap đầy → bỏ gói, đếm `OverflowDropped`.

Coordinator dùng `VideoJitterConfig` / `VideoJitterOutSize` cho video; session manager dùng
`video.jitter.packet_queue_size` cho `VideoPacketQueue`.

### 3.3 Decoder (`internal/codec/video`)

- `H264RtpParser`: đếm seq đứt; đếm AU đã ghép xong nhưng bị bỏ vì đang chờ IDR (không tính AU
  hỏng trực tiếp do mất gói); đếm số lần chuyển sang chờ IDR do lỗi (không tính lần chờ IDR đầu
  stream); báo trạng thái đang chờ.
- `VideoDecoderStats`: `packet_gaps` gồm seq đứt; `wait_idr_drops` thật; thêm
  `wait_idr_episodes`, `waiting_for_idr`. `PushRTP` trả `PACKET_GAP` khi thấy seq đứt.

### 3.4 Pipeline và metric

`sessionVideoPipeline` theo dõi lúc vào/ra một đợt chờ IDR theo giờ nhận gói tại listener
(`ReceivedAtMs`). Hết chờ: log INFO `pipeline: video: đã nhận IDR sau khi mất gói`
(`session_id`, `wait_ms`, `dropped_frames`); bắt đầu chờ: log DEBUG.

| Metric | Loại | Ý nghĩa |
| --- | --- | --- |
| `media_ai_video_jitter_overflow_dropped_total` | counter | Gói bị bỏ vì jitter đầy `max_packets` |
| `media_ai_video_decoder_packet_gaps_total` | counter | Số lần depacketizer thấy seq đứt (gồm cả gói rơi ở hàng đợi nội bộ) |
| `media_ai_video_decoder_wait_idr_episodes_total` | counter | Số lần bắt đầu chờ IDR do mất gói/AU hỏng |
| `media_ai_video_decoder_wait_idr_dropped_frames_total` | counter | AU đã ghép xong nhưng bị bỏ khi chờ IDR (frame mất ≈ giá trị này + AU hỏng) |
| `media_ai_video_decoder_wait_idr_ms_total` | counter | Tổng ms chờ IDR (giờ nhận gói); chia cho episodes ra trung bình |
| `media_ai_video_decoder_sessions_waiting_idr` | gauge | Session đang chờ IDR do mất gói |

Lưu ý: khi seq nhảy tiến lớn (đổi SSRC), `jitter_lost_total` tăng đúng bằng số seq bị bỏ qua.

## 4. Kế hoạch kiểm chứng

- Unit (WSL, CGO): jitter (chính sách mới; test audio cũ pass nguyên), config (mặc định, kiểm tra,
  map), coordinator, decoder (fixture 1 slice/frame: `-threads 1`), pipeline, metric.
- `go vet`, `go test ./...` toàn repo.
- E2E thật: `scripts/test-video-e2e.sh` (mock) không hồi quy; chạy gateway + `mock-video-rtp-sender
  --drop-packets` đọc log và `/metrics`, so với HEAD cùng kịch bản.

## 5. Kết quả kiểm chứng (2026-11-09)

Môi trường: WSL Ubuntu, Go 1.26.4, `CGO_ENABLED=1`, FFmpeg 7.1.1.

- `go vet ./...` sạch; `go test ./...` toàn repo pass. File đã sửa sạch `gofmt` (kèm sửa 2 dòng
  lệch có sẵn trong `video_pipeline.go`).
- `scripts/test-video-e2e.sh --duration 10` (mock): `change_background` PASS (765 gói, 50 mask) và
  `--gesture-only --gesture-mode mock` PASS (765 gói, 150 gesture); 0 lost, 0 sequence đứt.
- Chạy thật 1 cuộc gọi `change_background` (mock AI, 640x480, 10 fps, IDR mỗi 10 frame, 537 gói),
  cùng cấu hình jitter 80/200/20, so với HEAD `bd50956`. Poll `/metrics` ~20 ms để đo khoảng
  `jitter_released_total` không tăng:

| Kịch bản | Jitter đứng lâu nhất (mới / HEAD) | Số liệu mới |
| --- | --- | --- |
| Không mất gói | 141 / 120 ms | 0 gap, 0 lần chờ IDR, 30 frame gửi AI |
| `--drop-packets 250` | 136 / 123 ms | gap 1, chờ IDR 1 lần, 8 frame bỏ, 901 ms, log INFO |
| `--drop-packets 250..269` | **271 / 1971 ms** | gap 1, chờ IDR 1 lần, 6 frame bỏ, 701 ms |

  Mất 20 gói: HEAD giữ toàn bộ gói phía sau ~2 s rồi nhả dồn (frame tới AI/MF trễ ~2 s); bản mới
  chỉ còn khoảng trống thật (~2 frame không có gói) + `buffer_ms`. Kịch bản gói đảo thứ tự sau
  khoảng nghỉ giữa 2 frame được kiểm bằng unit test (`TestSkipGaps_ReorderAfterInterFramePause`);
  sender chưa có tuỳ chọn đảo thứ tự gói.

## 6. Chưa làm

- **B**: khoá SSRC / phát hiện seq nhảy lùi → reset jitter + `decoder.Reset()`; timestamp tracker
  xử lý timestamp lùi (hiện ra `TimestampMs` rác ~2e14).
- **C**: PLI/FIR — cần MF hỗ trợ `RTP/AVPF` + `rtcp-fb`, chốt RTCP cùng cổng hay cổng+1.
- Fixture `coordinator/video_e2e_test.go` cũng tách 2 slice/frame thành 2 AU (chưa sửa).
