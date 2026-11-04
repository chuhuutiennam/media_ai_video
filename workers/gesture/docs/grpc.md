# Gesture gRPC server

Để cài vào APP_DIR, chạy nền và quản lý lifecycle bằng script như segmentation,
xem [triển khai dịch vụ](deployment.md).
Lifecycle được quản lý trực tiếp bởi `.sh`; worker ghi JSON log, traceback,
thống kê định kỳ và event stream/hand/gesture để theo dõi vận hành.

Server thật tại [src/server.py](../src/server.py), nằm riêng với segmentation.
Core MediaPipe, tracking, static hold và Wave được tái sử dụng từ worker gesture;
server không render video. MF ghép sticker gần từng tay sau callback.

## Chạy từ root repository

```bash
python -m pip install ./workers/gesture
python -m workers.gesture.src.server \
  --config workers/gesture/etc/config.yaml --addr 127.0.0.1:50053
```

Package cài riêng có entry point `mediapipe-gesture-server`. Khi không có YAML,
truyền `--model workers/checkpoint/gesture_recognizer.task`. Các flag khác:
`--delegate cpu|gpu`, `--max-streams N`, `--result-timeout SECONDS`,
`--metrics-port PORT` (0 tắt HTTP metrics), `--log-level INFO`.
Model được đọc một lần và thử tạo recognizer trước khi mở cổng.

## Contract và vòng đời mỗi stream

1. Gateway mở `/video.VideoStream/Segment`, gửi config với session/stream ID,
   `ai_service=gesture_recognition`, `input_format=jpeg`, `media_epoch`,
   `inference_fps` (1..60), `max_hands` (1..8). Dimensions config có thể bỏ trống;
   nếu có phải khớp JPEG mỗi frame.
2. Worker lấy slot recognizer rồi tạo một MediaPipe GestureRecognizer ở chế độ
   **LIVE_STREAM** cho RPC đó. Tracker, static/Wave state và recognizer epoch riêng;
   model bytes dùng chung. Không trộn state giữa call/leg.
3. Mỗi frame có JPEG, frame ID tăng, timestamp media ms tăng, RTP/extended RTP
   timestamp và original dimensions. Kiểm tra JPEG SOF trước decode; giới hạn
   receive message mặc định 8 MiB, cạnh inference tối đa 1920 pixel.
4. Worker decode JPEG thành RGB nội bộ, gọi `recognize_async`, chờ callback của
   đúng timestamp rồi xử lý frame kế tiếp. Một inference đang chờ trên mỗi RPC;
   callback chỉ sao chép observation và báo hoàn tất, không gửi mạng/render.
5. Tracker gán ID từng tay; engine lọc nhãn tĩnh bằng score/hold/min samples,
   phát Wave bằng landmark history. Reaction tĩnh được gia hạn khi giữ cử chỉ;
   Wave ưu tiên đến hết TTL. Tay biến mất xóa reaction khỏi snapshot.
6. Một `VideoResponse` cho mỗi JPEG: `ai_service`, `seq`, `gesture` có source
   identity nguyên vẹn, media/recognizer epoch, response ID, hands/reactions.
   Mỗi hand có track ID, handedness, raw/stable label, score, normalized center
   và bbox `[x_min,y_min,x_max,y_max]`; không truyền landmarks. Không có tay vẫn
   trả snapshot `status=ok`, danh sách rỗng để MF xóa overlay cũ.
7. EOS hoặc client half-close kết thúc sau các frame trước đó; cancellation đóng
   recognizer rồi trả slot. Callback timeout đóng RPC với DEADLINE_EXCEEDED,
   không giả lập kết quả rỗng. Config/JPEG lỗi trả INVALID_ARGUMENT; thiếu slot
   trả RESOURCE_EXHAUSTED; lỗi model/runtime trả INTERNAL.

Contract chính là `internal/ai/proto/video.proto`. Checkout đọc trực tiếp nguồn;
`src/proto/video.proto` là snapshot để phân phối, wheel/sdist mang cùng schema.
Chạy `bash scripts/generate-video-proto.sh` để đồng bộ cả hai worker sau khi sửa
proto nguồn. Không sửa snapshot riêng.

## Capacity, metrics và log

`server.max_streams=8` giới hạn RPC, `gesture.max_concurrent_recognizers=4` giới
hạn native recognizer; số stream chạy inference đồng thời tối đa là giá trị nhỏ
hơn. CLI `--max-streams` chỉ đổi giới hạn RPC. Mỗi call hai leg dùng hai recognizer.
Cần điều chỉnh capacity registry phù hợp worker; reservation registry là local
trong từng gateway, semaphore worker mới là giới hạn trên process.

Metrics mặc định `http://127.0.0.1:9102/metrics`: active streams/recognizers,
processed frames, latency, events và errors. Log `hand_observation` chứa từng
hand và center/bbox normalized/pixel dựa trên kích thước video gốc, không chứa
toàn bộ landmarks. Log mỗi frame có tay phù hợp debug, cần cân nhắc mức log khi
đo tải. GPU là tùy chọn môi trường; các bài E2E hiện đã chạy bằng CPU.

## Kiểm thử

```bash
PYTHONPATH=workers/mediapipe/src python -m pytest \
  workers/gesture/tests workers/mediapipe/tests
MEDIAPIPE_NATIVE_SMOKE=1 PYTHONPATH=workers/mediapipe/src python -m pytest \
  workers/gesture/tests/test_native.py
```

Unit test kiểm tra contract/identity, static reaction/clear, ưu tiên Wave, epoch
mỗi RPC, config/JPEG hỏng, timeout và trả slot sau lỗi/hủy. Native smoke cần
checkpoint/video mẫu; E2E thật đi qua gateway và MF theo
[hướng dẫn hai endpoint external](../../../docs/video-external-e2e.md).
