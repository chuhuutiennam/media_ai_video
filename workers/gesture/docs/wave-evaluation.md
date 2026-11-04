# Wave: capture, replay và đánh giá

Worker giữ tracker riêng từng stream/tay, media timestamp, cooldown và reaction.
Wave dùng `wave.open_palm_score_threshold` (mặc định 0.5), độc lập với confidence
gesture tĩnh. `require_open_palm: true` vẫn là mặc định.

## Các lựa chọn

- `signal: palm | wrist`: tâm lòng bàn tay hoặc landmark cổ tay 0.
- `counter: hysteresis | steps`: đếm cực trị đủ biên độ hoặc đổi dấu bước di
  chuyển (tham khảo notebook). Bản steps dùng cùng timestamp, tracker, smoothing
  và confidence gate để phép so sánh chỉ thay cách đếm; không phải sao chép nguyên
  mọi ngưỡng của notebook.
- `min_amplitude`: biên độ theo kích thước lòng bàn tay median trong cửa sổ.
  Hysteresis yêu cầu từng lần đảo chiều đạt ngưỡng; steps yêu cầu tổng span đạt
  ngưỡng và từng bước vượt `min_step` lần kích thước tay.
- `min_motion_fraction`: sàn chống nhiễu theo chiều rộng ảnh, áp dụng cho cả
  ngưỡng biên độ và bước. Tọa độ X và kích thước tay cùng dùng hệ có sửa aspect.
- Smoothing dùng media-time delta, quy đổi `smoothing_alpha` tại mốc 15 FPS.
- Một frame mất tay xóa lịch sử chuyển động của tay đó; giữ cooldown khi track
  còn sống. Track hết hạn hoặc đổi danh tính có detector mới.

Mặc định vẫn chọn `palm/hysteresis`; chỉ đổi sau khi so sánh trên nhiều clip.
Không tự chỉnh deployment đang chạy hoặc timeout inference.

## Capture một lần, so sánh trên cùng landmarks

Chạy từ repository root, bằng Python environment có dependencies gesture:

```bash
python -m workers.gesture.src.wave_replay \
  --video data/gesture_sample/wave_hand.mp4 \
  --config workers/gesture/etc/config.yaml \
  --width 320 --height 240 --fps 15 \
  --capture /tmp/wave-landmarks.jsonl \
  --output /tmp/wave-comparison.json \
  --controls-output /tmp/wave-controls.json
```

Capture dùng VIDEO mode, xử lý lần lượt các frame được chọn từ đầu đến cuối clip,
không qua JPEG/RTP. Đây là baseline offline, không thay thế kiểm chứng LIVE_STREAM
và E2E. Các nhánh dùng chính xác cùng landmarks; kết quả gồm timestamp/track của
event, cấu hình và số quyết định theo lý do loại. So sánh 2 tín hiệu × 2 cách đếm ×
3 confidence policy: Open_Palm >=0.5, >=0.6 và bỏ gate để đo đối chứng.

```bash
python -m workers.gesture.src.wave_replay \
  --replay /tmp/wave-landmarks.jsonl \
  --config workers/gesture/etc/config.yaml \
  --output /tmp/wave-comparison-again.json
```

`--controls-output` dựng các chuỗi âm tính từ một quan sát Open_Palm trong clip:
đứng yên, đi một chiều, rung nhỏ, đổi tay, camera pan và camera dao động ngang.
Đây là **dữ liệu landmarks tổng hợp**, không phải sáu video âm tính được ghi thật.
Mỗi số khác 0 trong control là trường hợp cần kiểm tra nhận nhầm.
Camera dao động có thể cho false positive vì thiếu tham chiếu cảnh/nền/cơ thể.
Cần thu clip âm tính thật trước khi tuyên bố precision/recall hoặc chọn cấu hình
cho production. Video Clap có sẵn có thể dùng làm đối chứng bổ sung, nhưng phải
kiểm tra nội dung trước khi coi toàn clip là no-wave.

## Replay chính xác từ worker

Đặt `logging.level: DEBUG` trong config deployment và restart. Worker ghi
`wave_frame` chứa toàn bộ landmarks, metadata nguồn và các frame không có tay;
`wave_decision` ghi tâm/cổ tay, ngưỡng, span, số đảo chiều và lý do.

```bash
python -m workers.gesture.src.wave_replay \
  --replay run/gesture/logs/stdout.log \
  --config run/gesture/etc/config.yaml \
  --output /tmp/worker-wave-replay.json
```

Parser hỗ trợ log text và JSON. Mỗi session/stream có tracker riêng, không trộn
hai leg. Nên dùng log của một lượt chạy để dễ đối chiếu. DEBUG tăng dung lượng log;
chỉ bật trong giai đoạn chẩn đoán. Không thay contract callback hoặc gửi landmarks
mới sang MF.

## Kiểm chứng

```bash
python -m unittest discover -s workers/gesture/tests -p 'test_wave_replay.py' -v
python -m unittest discover -s workers/gesture/tests -p 'test_*.py' -v
```

Các test phủ positive/negative cùng chuỗi landmarks, FPS 10/15/30, thay kích thước
tay, confidence độc lập với gesture tĩnh, phân biệt tín hiệu, cô lập stream và cấu
hình sai. Test hiện có tiếp tục kiểm tra cooldown, metadata và nhiều tay.

## Kết quả đối chứng ngày 2026-11-02

Model `workers/checkpoint/gesture_recognizer.task`, CPU, VIDEO mode, toàn bộ
`wave_hand.mp4`: 36 frame chọn ở 15 FPS, resize 320×240. Cùng một chuỗi landmarks
được replay qua cả 12 nhánh; không chạy lại inference để ưu ái nhánh nào.

| Nhánh | Open >=0.5 | Open >=0.6 | Không gate |
| --- | ---: | ---: | ---: |
| Palm / hysteresis | 0 | 0 | 0 |
| Wrist / hysteresis | 0 | 0 | 0 |
| Palm / steps | 1 | 0 | 1 |
| Wrist / steps | 1 | 0 | 1 |

Nhánh steps phát sự kiện ở media time 667 ms. Kết quả này hỗ trợ thử steps với
confidence 0.5, chưa cho thấy wrist tốt hơn palm. Mặc định vẫn giữ palm/hysteresis;
muốn thử cấu hình mới, đổi `wave.counter: steps` trong config thực tế, cập nhật
package worker và restart theo hướng dẫn deployment.

Control landmarks dựng từ mẫu Open_Palm trong clip: cả 12 nhánh cho 0 sự kiện ở
giữ tay mở, đi một chiều, rung nhỏ, đổi tay và camera pan. Cả 12 nhánh có 1 false
positive ở camera dao động ngang. Cần thêm tham chiếu nền/cơ thể để giải quyết
trường hợp này; thay ngưỡng riêng bàn tay không bảo đảm phân biệt được.

Artifacts lượt đo: `/tmp/media-ai-wave-landmarks.jsonl`,
`/tmp/media-ai-wave-comparison.json`, `/tmp/media-ai-wave-controls.json`.
Đây là một clip ngắn và controls tổng hợp; chưa phải benchmark precision/recall,
chưa xác nhận LIVE_STREAM/gRPC/MF E2E sau thay đổi.
