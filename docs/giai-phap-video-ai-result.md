# Giải pháp: vòng đời stream AI video (nhận kết quả từ `../mediapipe`, `../gesture`)

Trạng thái: **đã triển khai** (chốt và code 2026-11-09, nhánh `fix/video-ai-stream-lifetime`). Kết quả
kiểm chứng ở §5.
Phạm vi: chế độ gọi thẳng worker (`video.services`, `video.ai.workers`, `video.ai.grpc_target`).
AI Engine nằm ngoài phạm vi.

## 1. Luồng nhận kết quả hiện tại

1. Mỗi session mở 1 stream gRPC `/video.VideoStream/Segment` cho đúng 1 AI task, gửi `VideoConfig`
   rồi JPEG frame.
2. Worker trả đúng 1 kết quả mỗi frame, theo thứ tự. Segmentation (`../mediapipe`): `VideoMask`,
   lỗi từng frame trả `status:"error"`, callback MediaPipe quá hạn 10 s → abort `DEADLINE_EXCEEDED`.
   Gesture (`../gesture`): `GestureResult`, quá hạn 2 s → abort `DEADLINE_EXCEEDED`, frame sai →
   `INVALID_ARGUMENT`.
3. Gateway (`internal/ai/video_client.go` `runRecv`): gesture được kiểm tra chặt (sai → đóng stream);
   mask đưa vào `VideoMaskResultQueue` (đầy thì chờ, áp lực ngược về worker), gesture vào
   `GestureResultQueue` (đầy thì bỏ).
4. `coordinator.pumpResults` bỏ mask `status != ok`, đẩy dispatcher theo lane → HTTP callback MF.
5. Lỗi stream → `runWithReconnect`: mã vĩnh viễn dừng hẳn, mã khác thử lại tối đa `video.ai.max_retry`.

## 2. Lỗi đã xác nhận bằng chạy thật (main `2fec84f`, WSL, mock AI)

| Kịch bản | Kết quả |
| --- | --- |
| `video.ai.stream_timeout_sec: 5`, gửi RTP 12 s | Kết quả cuối ở giây 5,08 (24/~60); **không log** |
| Tắt mock AI giây 4, bật lại giây 6, `max_retry: 3` | Kết quả cuối giây 4,03; **0 kết quả sau khi AI chạy lại**; chỉ 1 Warn `ai: video recv error` |

Nguyên nhân:

1. `video.ai.stream_timeout_sec` mặc định 300 là deadline cứng của stream ở chế độ gọi thẳng worker;
   hết hạn thì stream kết thúc không thử lại, không log. Các config mẫu `gateway-video-{services,
   workers,external}.yaml`, `gateway-mock-video.yaml` không đặt → AI bị cắt sau 5 phút.
2. Khi kết nối lại, nếu lần mở stream đầu tiên lỗi (worker chưa lên) thì bỏ cuộc ngay, không dùng các
   lượt thử còn lại.
3. Bộ đếm lượt thử không bao giờ đặt lại → `max_retry` tính cho cả đời stream.
4. Stream dừng hẳn không có log/metric.

## 3. Quyết định đã chốt

| # | Quyết định |
| --- | --- |
| S1 | Mặc định `video.ai.stream_timeout_sec` = **0** (stream sống theo cuộc gọi). > 0 vẫn là giới hạn cứng; hết hạn thì log + metric (S3, lý do `stream_timeout`). `ai.stream_timeout_sec` của audio không đổi. |
| S2 | Kết nối lại: mở stream lỗi → tính một lượt, chờ backoff, thử tiếp. Backoff = `retry_backoff_ms × 2^(lượt−1)`, tối đa 30 s. Lượt đặt lại về 0 khi stream mới nhận được kết quả đầu tiên → `max_retry` là số lần lỗi **liên tiếp**. Mẫu `gateway-video-services.yaml` đổi `max_retry` 0 → 3. |
| S3 | Stream dừng hẳn bất thường (`stream_timeout`, `permanent_error`, `retries_exhausted`) → Warn `ai: video stream stopped` (`session_id`, `stream_id`, `ai_service`, `reason`, `retries`, `error`) + counter `media_ai_video_ai_streams_stopped_total`. Session đóng hoặc kết thúc bình thường thì không log. |
| — | Để sau: kiểm tra hợp lệ mask segmentation; hạn chờ kết quả cho frame đang gửi (worker treo không abort). |

## 4. Kế hoạch kiểm chứng

- Unit test `internal/ai`: mở lại lỗi rồi thành công vẫn tiếp tục; lượt đặt lại sau kết quả; lỗi vĩnh
  viễn dừng ngay; hết lượt / hết hạn có lý do đúng và tăng metric. Config: mặc định 0.
- `go vet`, `go test ./...`, E2E `scripts/test-video-e2e.sh` (segmentation + gesture-only).
- Chạy lại 2 kịch bản ở §2 sau khi sửa.

## 5. Kết quả kiểm chứng (2026-11-09, WSL, Go 1.26.4, CGO, mock AI)

- gofmt/`go vet` sạch; `go test ./...` toàn repo pass; test kết nối lại pass với `-race`.
- `scripts/test-video-e2e.sh --duration 10`: `change_background` PASS (765 gói, 50 mask),
  `--gesture-only --gesture-mode mock` PASS (150 gesture); không có log dừng/mở lại stream.
- Chạy thật cùng kịch bản, bản cũ (`main` `2fec84f`) so với bản sửa:

| Kịch bản | Bản cũ | Bản sửa |
| --- | --- | --- |
| `stream_timeout_sec: 5`, gửi 12 s | Dừng ở 5,08 s, không log | Dừng ở 5,05 s (giới hạn cứng đúng như cấu hình), Warn `ai: video stream stopped reason=stream_timeout`, `ai_streams_stopped_total` = 1 |
| `change_background`: tắt AI giây 4, bật lại giây 6 | 0 kết quả sau khi AI chạy lại | Mở lại lần 1 lỗi (AI chưa lên) → lần 2 thành công sau ~3 s; 39 kết quả sau khi AI chạy lại, kết quả tới hết cuộc gọi (14,2 s) |
| `video_effects` (gesture): như trên | 0 kết quả sau khi AI chạy lại | 74 kết quả sau khi AI chạy lại, tới hết cuộc gọi (14,45 s) |

Chưa cập nhật `docs/video-testing.md` (dòng "`stream_timeout_sec` … mặc định 300s") vì file đang có
thay đổi chưa commit trên `main`; cần sửa thành mặc định 0 khi commit phần đó.
