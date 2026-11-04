# Gesture worker cho video

Triển khai bằng bộ script `install/check/start/stop/restart/status/upgrade` tại
`workers/gesture/scripts`: xem [hướng dẫn triển khai](docs/deployment.md).
Mặc định dùng gRPC `50053`, metrics `9102`, venv/PID/log riêng với segmentation.
Các thao tác vận hành chạy trực tiếp bằng Bash; worker có JSON log xoay vòng,
log theo stream/gesture và thống kê metrics định kỳ như segmentation.

Nhận diện gesture tĩnh bằng MediaPipe và vẫy tay bằng landmark tracking, hỗ trợ
nhiều tay và sticker hoạt hình bám theo từng tay. Worker nằm riêng với segmentation.

```text
workers/gesture/
├── pyproject.toml, setup.py, MANIFEST.in
├── etc/config.yaml
├── src/                       # package mediapipe_gesture khi cài đặt
│   ├── video.py               # VIDEO: xử lý tuần tự, không bỏ frame
│   ├── server.py              # gRPC JPEG: MediaPipe LIVE_STREAM cho RTPGW
│   ├── contract.py, proto/    # dùng contract chung internal/ai/proto/video.proto
│   ├── gesture.py             # LIVE_STREAM dispatcher cho realtime replay
│   ├── gesture_events.py      # Lọc ổn định, event và tọa độ từng tay
│   ├── hand_tracker.py
│   ├── wave_detector.py
│   ├── overlay.py             # Sticker gần từng tay
│   ├── demo.py, __main__.py    # CLI video
│   ├── config.py, metrics.py, log.py
│   └── segmentation_adapter.py # background demo tùy chọn
├── assets/emoji/
├── scripts/                   # Bash lifecycle scripts độc lập và asset generator
├── tests/
└── docs/usage.md
```

Chạy từ repository root với Python có dependencies của worker:

```bash
python -m workers.gesture.src \
  --config workers/gesture/etc/config.yaml \
  --source data/gesture_sample/static_gesture.mp4 \
  --output /tmp/static_gesture_result.mp4 \
  --events /tmp/static_gesture_events.jsonl \
  --hands-log /tmp/static_gesture_hands.jsonl \
  --draw-hands --no-preview
```

Mặc định xử lý file bằng VIDEO, mọi frame khớp kết quả của chính nó. Không dùng
webcam. Num hands mặc định 4, cấu hình được 1..8. Log có nhãn thô, score, nhãn ổn định,
track ID, tâm và bbox normalized/pixel; không ghi toàn bộ landmarks.
Bỏ --draw-hands để video chỉ có sticker, bỏ --no-preview để xem cửa sổ.

Đã kiểm tra đoạn Thumb_Up từ giây 4 trong static_gesture.mp4: confidence model chủ
yếu 0,64–0,68, dưới ngưỡng cũ 0,70. Ngưỡng mới 0,60 kết hợp hold/min samples và
tolerance cho kết quả không chắc chắn ngắn; phát event ở 4,30 giây trên lần kiểm tra.
Đây là kiểm tra trên video mẫu, không phải cam kết độ chính xác cho mọi video.

Cài package riêng nếu cần entry point (Python 3.11+):

```bash
python -m pip install ./workers/gesture
mediapipe-gesture --config workers/gesture/etc/config.yaml --source data/gesture_sample/static_gesture.mp4
```

Server gRPC thật cho RTPGW, chạy trên endpoint riêng với segmentation:

```bash
python -m workers.gesture.src.server \
  --config workers/gesture/etc/config.yaml --addr 127.0.0.1:50053
```

Xem [server gRPC](docs/grpc.md), [E2E hai worker external](../../docs/video-external-e2e.md)
cho video/PCAP qua RTPGW → hai model → MF → MP4. Package có entry point
`mediapipe-gesture-server`. Model dùng chung ở workers/checkpoint.
[Hướng dẫn demo](docs/usage.md) mô tả schema log, realtime replay và kiểm thử video riêng.

## Đánh giá Wave

Xem [capture/replay và so sánh Wave](docs/wave-evaluation.md) để chọn tín hiệu cổ
tay/tâm bàn tay, cách đếm đảo chiều và confidence bằng cùng dữ liệu landmarks.

