# Test gesture với file video

Server gRPC thật và luồng RTPGW/MF dùng hai endpoint được mô tả riêng tại
[gesture gRPC](grpc.md) và [E2E video/PCAP external](../../../docs/video-external-e2e.md).
Phần dưới dành cho demo xử lý file độc lập.

Chạy từ repository root. Cấu trúc source hiện tại dùng lệnh
`python -m workers.gesture.src`; nếu cài package bằng pip thì dùng
`mediapipe-gesture` hoặc `python -m mediapipe_gesture`.

## Video và kết quả

```bash
run_mediapipe/libs/venv/bin/python -m workers.gesture.src \
  --config workers/gesture/etc/config.yaml \
  --source data/gesture_sample/static_gesture.mp4 \
  --output /tmp/static_result.mp4 \
  --events /tmp/static_events.jsonl \
  --hands-log /tmp/static_hands.jsonl \
  --draw-hands --no-preview
```

- --source bắt buộc là file video; không mở webcam hoặc nhận camera index.
- --mode video là mặc định: MediaPipe VIDEO, xử lý mọi frame tuần tự, không mất kết
  quả cuối clip. Output FPS và timestamp lấy từ video, thời gian xử lý có thể lâu hơn clip.
- --events lưu các gesture đã qua lọc ổn định. --hands-log lưu kết quả từng frame,
  kể cả frame không có tay. Khi không chỉ định hands-log, tự đặt cạnh output/events
  với đuôi .hands.jsonl. Các đường dẫn input/output/log phải khác nhau.
- --draw-hands thêm bbox, track ID và nhãn thô/score lên video để debug.
- Bỏ --no-preview để xem GUI; q/Esc để thoát. --max-frames N giới hạn bài test.
- --mirror chỉ lật hiển thị; vị trí sticker được chuyển đổi theo ảnh đã lật.
- MP4 không chứa audio. Không sửa worker segmentation hoặc RTPGW.

## Nhận diện và lọc

Config [etc/config.yaml](../etc/config.yaml) có model, gesture, wave, overlay.
Đường dẫn model/assets resolve theo YAML. CLI luôn bật gesture; API GesturePool
tôn trọng gesture.enabled. --gesture-model, --num-hands, --score-threshold ghi đè YAML.

Num hands mặc định 4, giới hạn 1..8. Không dùng thứ tự detection làm ID. Tracker ghép
theo tâm/kích thước và handedness, reset khi mất quá track_max_gap_ms hoặc ghép mơ hồ.
ID là của bàn tay trong stream, không phải định danh người; khi tay ra/vào hoặc giao
nhau có thể nhận ID mới.

Các nhãn: Thumb_Up, Thumb_Down, Victory, ILoveYou, Closed_Fist, Pointing_Up.
Open_Palm là điều kiện cho Wave. Không finetune hoặc giả lập nhãn từ confidence thấp.

Ngưỡng mặc định 0,60; cần giữ 200 ms và ít nhất 3 mẫu dương. Một khoảng không chắc
chắn ngắn tối đa static_gap_ms không xóa ngay tiến trình giữ tư thế.
Nhả/đổi đủ static_release_ms và hết cooldown mới tái kích hoạt. Không phát event liên
tục khi giữ nguyên tư thế. Mỗi tay có state độc lập; Wave ở tay này không chặn Thumb_Up
ở tay khác.

Trong static_gesture.mp4, đoạn 4–6,5 giây có Thumb_Up chủ yếu 0,64–0,68: ngưỡng
0,70 trước đây loại bỏ nhãn đúng. Với cấu hình mới, lần chạy kiểm tra phát Thumb_Up
ở 4300 ms, score khoảng 0,635. Nhận diện hai Thumb_Up ở cuối clip và render hai
sticker riêng cũng đã được kiểm tra. Các con số này không thay cho benchmark tổng quát.

Wave dùng lịch sử tâm tay, biên độ theo palm size và số lần đổi hướng; có làm mượt
và cooldown. Camera rung/người di chuyển, che khuất/giao tay, FPS/ảnh nhỏ vẫn có thể
gây lỗi. Điều chỉnh ngưỡng dựa trên log thô, không chỉ nhìn event.

## Tọa độ log

Mỗi dòng hands JSONL có session_id, stream_id, frame_id, timestamp_ms,
frame_width, frame_height và danh sách hands. Mỗi tay gồm:

| Trường | Ý nghĩa |
| --- | --- |
| track_id, handedness | ID track và Left/Right từ model; không phải ID người. |
| gesture, score | Nhãn thô và confidence model trước bộ lọc. |
| stable_gesture | Nhãn đã được bộ lọc xác nhận; null khi chưa ổn định. |
| center_normalized | [x,y] của tâm lòng bàn tay, trong khoảng 0..1. |
| bbox_normalized | [xmin,ymin,xmax,ymax] của bàn tay, clamp vào ảnh. |
| center_px | [x,y] theo pixel ảnh nguồn. |
| bbox_px | [xmin,ymin,xmax,ymax] theo pixel ảnh nguồn. |

Các tọa độ luôn thuộc ảnh nguồn trước mirror; gốc ở góc trên trái, x sang phải, y
xuống dưới. Bbox là vùng bao landmarks, không phải confidence bbox từ palm detector.
Không ghi 21 landmarks vào log.

Gesture event có event_id, frame/timestamp nguồn, track ID, gesture/kind, score,
emoji_key, duration_ms, center_normalized, bbox_normalized, handedness và kích thước ảnh.
Wave dùng luật nên score là null. Tọa độ event là snapshot lúc xác nhận; overlay lấy
vị trí mới nhất của cùng track để di chuyển sticker.

## Sticker

PNG alpha hoạt hình trong assets/emoji, có mặt cười/nháy mắt và viền sticker.
Mặc định position: near_hand, sticker đặt trên hoặc cạnh bbox, clamp theo khung hình
và cố gắng tránh chồng nhau. Mỗi tay có sticker riêng. Dùng assets_dir cho bộ PNG khác;
mapping theo emoji_mapping, không kèm đuôi .png.

Sticker tĩnh còn hiển thị khi cùng tay tiếp tục giữ tư thế đã xác nhận, dù chỉ có
một event. Khi mất tay quá hand_lost_ms thì ẩn; Wave hết hạn theo duration_ms. Tất cả
thời lượng render dùng thời gian video, nên xuất offline không làm sticker hết hạn
sớm theo tốc độ CPU. position góc cố định cũ vẫn có thể cấu hình để so sánh.

Tạo lại icon từ code vẽ gốc của dự án:

```bash
python workers/gesture/scripts/generate-emoji-assets.py
```

## Realtime replay và API

```bash
python -m workers.gesture.src \
  --config workers/gesture/etc/config.yaml --source data/gesture_sample/static_gesture.mp4 \
  --mode realtime --events /tmp/realtime_events.jsonl --no-preview
```

Chế độ này cấp frame theo nhịp video và dùng LIVE_STREAM/mailbox giới hạn; có thể
bỏ frame khi chậm. Dùng để kiểm tra áp lực realtime, không dùng thay VIDEO khi đánh
giá đầy đủ từng tư thế. Chưa đọc PCAP; bước sau có thể decode PCAP và submit mp.Image
với timestamp tăng nghiêm ngặt vào GesturePool/GestureStream hiện có.

API VideoGestureProcessor.process trả (events, HandFrame) đồng bộ. API GesturePool.open
trả stream async: submit, drain_events, latest_hand_frame và close. Mỗi stream sở hữu
task/state riêng. Callback/error timeout không tự restart; caller kiểm tra stream.error.
Native close chậm giữ slot đến khi hoàn tất. Không có transport gesture vào cuộc gọi.

## Background demo và metrics

```bash
PYTHONPATH=workers/mediapipe/src python -m workers.gesture.src \
  --config workers/gesture/etc/config.yaml --source data/gesture_sample/static_gesture.mp4 \
  --effect bg_blur --model workers/checkpoint/selfie_segmenter.tflite
```

Segmentation adapter chỉ được import khi yêu cầu background effect. Emoji ghép sau
đổi nền. bg_remove dùng nền đen trong preview/MP4 vì định dạng này không có alpha.

--metrics-port 9102 mở Prometheus. Tiền tố mediapipe_gesture_: active_recognizers,
frames_total{status}, errors_total{reason}, events_total{gesture,kind}, latency_seconds.
Video mode ghi processed cho mọi frame; realtime có thể sampled_out/replaced/stale.

## Kiểm thử

```bash
python -m unittest discover -s workers/gesture/tests -v
MEDIAPIPE_NATIVE_SMOKE=1 PYTHONPATH=workers/mediapipe/src \
  python -m unittest discover -s workers/gesture/tests -v
```

Tests gồm nhận diện tĩnh/tolerance, Wave, nhiều tay, tracking, tọa độ, renderer bám tay,
mirror, mất tay, thời gian video, queue/timeout và boundary package độc lập.
Native smoke dùng checkpoint thật; việc kiểm chứng chất lượng cần các video có nhãn.
