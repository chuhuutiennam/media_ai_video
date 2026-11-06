# MF C++ cho kiểm thử video

`mf_cpp` nhận callback HTTP/2 h2c từ RTPGW và ghép frame nguồn, background và emoji.
Runner `scripts/test-video-e2e.sh` dùng MF này cho segmentation, gesture-only hoặc
hai task kết hợp. Cách chạy đầy đủ tại [video testing](../docs/video-testing.md).

Luồng điều khiển/dữ liệu, diagram và mẫu bản tin từng module nằm trong
[tài liệu video realtime RTPGW–AI–MF](../docs/luong-xu-ly-video-realtime-rtpgw-ai-mf.md).

## Build và chạy

Cần C++17, CMake 3.16+, pkg-config, nlohmann-json, nghttp2, FFmpeg libraries
libavcodec/libavutil/libswscale. Không cần Pillow để ghép frame.

```bash
bash scripts/build-mf-cpp.sh
bash scripts/test-video-e2e.sh --ai-mode mock --duration 2 --output-video
bash scripts/test-video-e2e.sh --gesture-only --gesture-mode mock --output-video --duration 3
```

Runner không có `--bin-dir` sẽ tự build vào thư mục run. Nếu dùng binary sẵn,
phải có gateway, sender, MF và mock AI khi profile cần mock.

## Dữ liệu và nguồn frame

```text
Video/PCAP → sender → RTP H264 → RTPGW decode/sample/JPEG → AI
               │                                         │
               └→ RGBA spool + frames.json; PNG debug sau seal                └→ mask / hands
                                                                  │
                        MF C++ ← HTTP/2 callback ← RTPGW ←─────────┘
                           │
                  background + foreground + sticker → PNG/timeline
                           │
                     FFmpeg CLI → output.mp4
```

Callback chứa metadata và source identity, không chứa frame màu gốc. Sender chuẩn bị RGBA/manifest trước khi phát packet đầu tiên; PNG nguồn debug
được MF export sau khi seal. MF khớp theo leg/RTP timestamp;
RLE theo hàng, bắt đầu background 0. Mask resize nearest-neighbor; phần foreground
lấy pixel nguồn. Gesture giữ timeline riêng, ghép sticker RGBA gần từng hand;
`--gesture-only` dùng frame nguồn làm base, không đòi mask/thay nền.

MF lưu kết quả trước ACK, gộp retry hợp lệ, từ chối identity/metadata mâu thuẫn.
Render chạy theo deadline riêng và không sửa PNG đã xuất. `/finalize` seal input;
runner poll `/status` chờ hoàn tất recording trước DELETE. Đây là kiểm chứng compositor trong lab; MP4 không có
audio và không được phát RTP ngược vào cuộc gọi.

## Receiver và artifacts

Ví dụ khởi động receiver với các biến chỉ tới thư mục run đã chuẩn bị:

```bash
./bin/mf_cpp --port 9999 --call-id example   --frames-root "$VIDEO_RUN/output" --background data/video/bg_image.jpg   --emoji-dir ../gesture/assets/emoji   --events "$VIDEO_RUN/mf-events.jsonl"
```

Ảnh emoji lấy từ repo AI gesture (`../gesture/assets/emoji`, script E2E dùng biến
`GESTURE_DIR`). Dịch vụ 1 leg (`change_background`, `video_effects`) chạy thêm `--legs 1`.

Receiver bind localhost. `/results/tCore` và `/results/tAccess` nhận callback đúng
session/stream/resource từng leg. `GET /status` xem trạng thái;
`POST /finalize` seal input; poll `/status` tới finalized để chờ render/recording. Không truyền `--frames-root` thì chỉ nhận/log.
`--gesture-only` chọn compositor trên nguồn, không nhận mask.

`mf-events.jsonl` lưu sự kiện callback/ACK; `callback.log` là stdout/stderr của MF.
`output/{tcore,taccess}` chứa source/mask/output PNG, `frames.json`, `render.json`,
`output.ffconcat` và `output.mp4`; runner kiểm tra pixel/timeline trong `result.json`.

## Test và giới hạn

```bash
cmake -S mf -B /tmp/media-ai-mf-test
cmake --build /tmp/media-ai-mf-test --parallel 2
ctest --test-dir /tmp/media-ai-mf-test --output-on-failure
```

Renderer tests kiểm tra alpha blend, nhiều tay, TTL, retry/conflict, metadata đến
muộn và gesture-only. E2E bổ sung callback HTTP/2 thật, pixel/timeline và cleanup.

MF hiện giữ một call/process với clock/render/recorder riêng từng leg. Lịch sử
kết quả và recording queue có giới hạn; chưa có admission/scheduler nhiều call
production. Các thay đổi cần làm trước khi mở tải nằm trong
[review streaming](../docs/gesture-implementation-summary-and-streaming-review.md).

## Realtime playout và MP4 kiểm tra

MF nhận callback và lưu kết quả trước ACK, không render/ghi PNG trong callback.
Sender chuẩn bị frames.rgba/frames.json (defer-source-png) rồi POST `/start/tCore` hoặc `/start/tAccess`
ngay trước khi phát RTP. Mỗi leg có clock riêng, render worker và recorder hữu hạn.
`--playout-delay-ms` mặc định 300 ms; hai `--mask-max-age-ms` và
`--gesture-max-age-ms` mặc định 500 ms. Hai task không cần cùng FPS.

Mỗi frame chọn mask/gesture có media time không vượt frame đó và đã được nhận
trước deadline. Mask thiếu/quá cũ dùng source nguyên bản; gesture quá cũ hoặc
reaction hết hạn không vẽ sticker. Callback đến muộn vẫn được đối chiếu/drain,
nhưng không sửa output đã phát. Frame render trễ quá 100 ms lặp hình đã xuất trước
đó, ghi `dropped` trong timeline; không giữ một backlog render vô hạn.

Trong realtime, recorder ghi đúng ảnh đã chọn vào spool RGBA tuần tự. Sau khi
input đã drain/seal, recorder encode các pixel đã capture thành PNG lossless
không nén (không ghép lại ảnh); runner encode các ảnh này theo
`output.ffconcat` thành MP4 sau đó. Đây là bản ghi quyết định realtime, không
composite offline bằng kết quả đến sau. Spool RGBA nguồn là adapter lab; chưa phải MF
nhận RTP và encode/phát RTP output production.

`POST /finalize` chỉ seal input, không chạy nhanh timeline hoặc render lại.
Poll `GET /status` tới `finalized: true` để chờ cả playout lẫn recorder xong.
Trạng thái có frames_rendered/written/dropped, recording queue/bytes và error.
`render.json` có deadline, nguồn thực sự đã dùng, mask ID và gesture response ID.

Mặc định mỗi leg nhận tối đa 4096 kết quả và ngân sách kết quả 64 MiB (ước lượng
JSON DOM); recorder giữ tối đa 32 ảnh/64 MiB đang chờ cộng một ảnh đang ghi.
Quá dung lượng kết quả trả HTTP 503, không ACK rồi bỏ. Recorder không theo kịp
báo lỗi, không âm thầm tăng RAM hoặc dừng clock chờ ghi ổ đĩa. Các giới hạn này
phù hợp lab hữu hạn; không phải admission control cho nhiều call production.

Ví dụ runner realtime kết hợp hai task:

```bash
bash scripts/test-video-external-e2e.sh \
  --ai-target 127.0.0.1:50052 --gesture-target 127.0.0.1:50053 \
  --input data/gesture_sample/wave_hand.mp4 --duration 6 \
  --playout-delay-ms 300 --mask-max-age-ms 500 --gesture-max-age-ms 500 \
  --artifacts-dir /tmp/video-realtime
```

Phải rebuild MF và RTP sender mới khi dùng `--bin-dir`. Tăng playout delay là
thay ngân sách latency thực, không phải tăng drain timeout. Nếu AI trả reaction
sau khi video đã phát, `--require-gesture` có thể FAIL dù inference thành công;
đây là kết quả cần thấy khi đánh giá hệ thống realtime. Chỉ tăng delay sau khi
đối chiếu arrival/deadline và yêu cầu độ trễ cuộc gọi.

Export dùng time base 90 kHz; runner kiểm tra số frame và PTS bằng ffprobe để
tránh mặc định PNG 25 FPS làm rơi frame của nguồn 30 FPS/VFR. MP4 có thêm một
frame lặp cuối làm end marker cho thời lượng frame cuối; đây không phải render
lại bằng dữ liệu AI muộn. `result.json` ghi `mp4_timeline` và `realtime_playout`.

### Kiểm tra hồi quy RTP và tải recorder

Ghi PNG đồng thời RTP/AI trên filesystem Windows có thể gây thêm áp lực I/O và
scheduling. Recorder hiện chỉ ghi RGBA trên file tạm ẩn của hệ thống (`std::tmpfile`, tự xóa khi đóng) tuần tự trong lúc clock chạy;
PNG debug được encode sau seal từ đúng dữ liệu đã capture, không dùng AI đến muộn.
PNG nguồn cũng được tạo sau seal từ RGBA nguồn. Spool tạm được xóa khi đã ghi PNG/timeline thành công. `frames_written` lúc live
là số ảnh đã capture vào raw spool; chỉ `finalized: true` bảo đảm PNG đã đủ.

Gateway bổ sung counter riêng cho video: UDP received/parsed/parse error/session
queue drop, jitter input/received/released và drop theo late/duplicate/output-full.
Không suy ra video không drop từ metric jitter audio bằng 0. Harness báo lỗi ngay
khi counter drop/lost hoặc mask/gesture result drop tăng, giữ nguyên strict packet
accounting thay vì đợi drain timeout chung.


### Gesture render modes và trace

`--gesture-render-mode realtime|arrival|source` chọn chính sách test. `realtime`
là mặc định; `arrival` hiển thị reaction trên frame hiện tại khi nhận callback,
giữ lifetime theo lần nhận event đầu tiên; `source` đợi seal rồi dựng theo
timeline nguồn, có thể sử dụng callback đến muộn cho cả mask và gesture.
Trạng thái `realtime: false` phân biệt source export. Trace theo response/frame
được ghi vào `gesture-trace.jsonl` bên cạnh `render.json`; xem [video testing](../docs/video-testing.md#mp4-test-realtime-arrival-và-source)
để biết công thức latency, giới hạn và lệnh chạy. Queue/callback RTPGW không đổi.
