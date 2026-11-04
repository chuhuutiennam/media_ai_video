# Kiểm tra gesture trên video mẫu

Đầu vào: `data/gesture_sample/static_gesture.mp4`, 1280×720, 30 FPS, 862 frame.
Chạy trên CPU với checkpoint `workers/checkpoint/gesture_recognizer.task`.

## Nguyên nhân Thumb_Up bị bỏ sót

Kiểm tra kết quả model thô bằng VIDEO, lấy mỗi hai frame: đoạn 3,5–6,5 giây có
37 observation mang nhãn Thumb_Up, confidence khoảng 0,588–0,688. Ngưỡng 0,70 trước
đây loại bỏ cả chuỗi, dù phần lớn mẫu có nhãn đúng khoảng 0,64–0,68.
Ngoài ra, cấu hình trước chỉ nhận một tay và bộ lọc reset khi gặp mẫu không chắc chắn.

Hiện dùng ngưỡng 0,60, hold 200 ms, ít nhất ba mẫu dương và tolerance 150 ms.
Tracking giữ state từng tay; confidence thấp không tự biến thành một nhãn được xác nhận.
VIDEO xử lý toàn bộ frame để đánh giá offline; LIVE_STREAM replay vẫn có thể bỏ frame.

## Kết quả tham chiếu trên video mẫu

| Timestamp | Gesture | Track ID | Confidence |
| --- | --- | --- | --- |
| 4300 ms | Thumb_Up | 7 | 0,635 |
| 7133 ms | Thumb_Down | 7 | 0,926 |
| 14567 ms | Victory | 10 | 0,901 |
| 14633 ms | Victory | 9 | 0,872 |
| 25467 ms | Pointing_Up | 15 | 0,653 |
| 27533 ms | Thumb_Up | 15 | 0,654 |
| 27633 ms | Thumb_Up | 10 | 0,633 |

Log có đủ 862 dòng tương ứng 862 frame. Model có frame trả ba detection; đây là số
detection, không phải xác nhận có ba tay thật. Đã kiểm tra trực quan hai tay giơ
Thumb_Up ở cuối clip và hai sticker được đặt riêng gần mỗi tay.

Frame xác nhận Thumb_Up tại 4300 ms có center_normalized khoảng [0,3965; 0,6449],
tương ứng center_px [508,464]. Bbox/center trong log luôn theo ảnh nguồn trước mirror.
Không ghi toàn bộ 21 landmarks.

## Wave và giới hạn

`data/gesture_sample/wave_hand.mp4`: 72 frame được xử lý, Wave được xác nhận tại
1367 ms, track 1.

Các kết quả trên là kiểm thử hồi quy trên video mẫu; chưa có ground truth đủ để
tính precision/recall hay cam kết độ chính xác cho mọi video. Track ID có thể đổi
khi tay ra/vào hoặc giao nhau. Video/PCAP qua RTPGW đã có runner riêng; xem
[E2E external](../../../docs/video-external-e2e.md). Kết quả offline này chưa
đánh giá giảm chất lượng do JPEG/resize của gateway.

## Chạy lại

Dùng Python environment có dependencies gesture, từ repository root:

```bash
python -m workers.gesture.src \
  --config workers/gesture/etc/config.yaml \
  --source data/gesture_sample/static_gesture.mp4 \
  --output /tmp/static-gesture-result.mp4 \
  --events /tmp/static-gesture-events.jsonl \
  --hands-log /tmp/static-gesture-hands.jsonl --no-preview
```

Đổi source sang `data/gesture_sample/wave_hand.mp4` và dùng đường dẫn output/log
khác để kiểm tra Wave. So sánh event, score, track và center/bbox trong log với
video tại timestamp tương ứng. Các mốc trên là kết quả tham chiếu của lần kiểm
tra trước, không phải metric precision/recall hay assert cho mọi phiên bản model.
