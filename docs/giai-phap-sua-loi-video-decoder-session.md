# Giải pháp: sửa 4 lỗi decoder video và vòng đời session (main `9de44a5`)

Trạng thái: **đã triển khai** (chốt và code 2026-11-13, nhánh `fix/video-decoder-session-lifecycle`).
Kết quả kiểm chứng ở §6.

| # | Mức | Lỗi | Phương án đã chốt |
| --- | --- | --- | --- |
| 1 | P1 | Buffer FU-A tăng không giới hạn | Kiểm tra `max_nalu_size` trước mỗi lần ghép mảnh |
| 2 | P1 | Dọn dẹp session cũ phá session mới cùng ID | **A**: session mới chờ session cũ dọn xong (`session.reuse_wait_ms`) |
| 3 | P2 | Timestamp sai frame khi có B-frame | Lấy timestamp theo pts của chính frame |
| 4 | P2 | `max_width`/`max_height` không có hiệu lực | `max_pixels` của libavcodec + kiểm tra từng cạnh; đếm `oversize_frames` riêng |

## 1. Hiện trạng và tái hiện (trước khi sửa)

Tái hiện trên WSL (Go 1.26.4, CGO, libavcodec 62.11) bằng test tạm trên bản copy repo:

| # | Kịch bản | Kết quả trên `9de44a5` |
| --- | --- | --- |
| 1 | FU-A đầu + mảnh giữa, không có mảnh cuối | Giới hạn 64 B → `fu_buffer_` 705 B; giới hạn mặc định 2 MiB, 10 000 mảnh × 1400 B → **13,4 MiB** |
| 2 | 200 lần: đóng session rồi tạo lại ngay cùng ID (video, có callback) | Mỗi lần chạy: callback session mới bị xoá 142–147, AI stream session mới bị đóng 6–10, Start session mới lỗi 5–15; bình thường chỉ 30–41 |
| 3 | H.264 main, `bf=2`, RTP ts = thời điểm hiển thị | 3/9 frame sai (I/P mang ts của P kế tiếp); ts gửi AI không tăng đều: 117000, 99000, 108000, 144000… |
| 4 | Stream 64×64, cấu hình 32×32 | Vẫn decode và xuất frame 64×64 |

Nguyên nhân:

1. `H264RtpParser::HandleFuA` nối mảnh vào `fu_buffer_` trước, `max_nalu_size_` chỉ được kiểm tra
   ở `AppendNAL` khi nhận mảnh cuối.
2. Goroutine dọn dẹp trong `Coordinator.Start`/`StartVideo` chờ `sess.Ctx.Done()` rồi gỡ tài nguyên
   **theo `sess.ID`** (dispatcher sink, audio/video pool, AI stream, jitter buffer).
   `session.Manager.Close` xoá ID ngay nên ID tạo lại được trước khi dọn xong.
3. `VideoStreamDecoder::DrainDecoder` gán timestamp của AU vừa đẩy vào cho mọi frame lấy ra. Decoder
   có thể trả frame của AU đã gửi trước đó (reorder), nên timestamp bị lệch.
4. `max_width`/`max_height` chỉ đi tới `MakeConfig`; decoder và Go không dùng.

## 2. Lỗi 1 — FU-A

`h264_rtp_parser.cpp`: trước **mỗi** lần ghép, cả mảnh đầu (`size-1`) lẫn mảnh giữa, kiểm tra
`fu_buffer_.size() + phần thêm ≤ max_nalu_size_`. Vượt thì bỏ FU-A và AU hiện tại, chờ IDR
(`MarkCurrentAUCorrupted`, giống NAL quá lớn trong `AppendNAL`), trả `ParseStatus::SizeExceeded`
(đã có sẵn, tính vào `corrupted_access_units`). Mảnh còn lại của NAL đó rơi vào nhánh
"mảnh giữa không có mảnh đầu" như trước. Không thêm cấu hình: dùng `max_nalu_size` (mặc định 2 MiB).

## 3. Lỗi 2 — vòng đời session (phương án A)

`coordinator.go`:

- Coordinator giữ `lives[sessionID] → sessionLife{sess, pending, done}` cho instance gần nhất.
- `Start`/`StartVideo` gọi `acquireLife(sess)` **trước khi đăng ký bất kỳ tài nguyên nào**:
  - ID trống hoặc đúng instance này → `pending++`.
  - ID thuộc instance trước đã đóng, chưa dọn xong → chờ `done`, tối đa `SessionReuseWait`; quá hạn thì
    trả lỗi (log WARN), controlplane rollback như các lỗi Start khác.
  - ID thuộc instance khác **còn sống** → lỗi ngay (không chờ).
  - Session đã đóng → lỗi.
- Mỗi goroutine dọn dẹp (và mỗi nhánh rollback) gọi `release()` đúng một lần khi xong;
  `pending` về 0 thì đóng `done` và xoá khỏi `lives`.
- Dọn dẹp của `Start` nay còn: `aiMgr.Close(sess.ID)` (gỡ stream audio khỏi Manager ngay, session mới
  không bị `ErrDuplicateStream`) và chờ các result pump của session thoát (session mới không nhận
  kết quả AI của session cũ).

Cấu hình mới:

| Khoá | Mặc định | Giới hạn | Ý nghĩa |
| --- | --- | --- | --- |
| `session.reuse_wait_ms` | 2000 | 1..60000 (ngoài khoảng → gateway không khởi động) | Session dùng lại ID vừa đóng chờ session cũ giải phóng xong tối đa N ms |

Đã thêm vào `config/gateway.yaml`, `config/gateway-ims.yaml`, `config/gateway-video-ims.yaml`,
`deploy/k8s/configmap.yaml`; các file config khác dùng mặc định code. Không có biến môi trường.

Còn lại (chấp nhận): kết quả của session cũ **đã nằm trong hàng đợi dispatcher** trước khi session cũ
đóng vẫn có thể được giao cho sink của session mới cùng ID. Dispatcher chỉ biết `SessionID` của kết
quả; muốn chặn hẳn cần gắn thế hệ session vào `RecognitionResult`.

## 4. Lỗi 3 — timestamp theo frame

- `h264_decoder.cpp`: `av_pts` = `frame->pts` (libavcodec giữ pts của packet qua reorder), thiếu thì
  `best_effort_timestamp`, thiếu nữa thì ts AU vừa gửi. Bỏ điều kiện `pts <= 0` (0 là ts hợp lệ).
- `video_stream_decoder.cpp`: `DrainDecoder()` lấy `rtp_timestamp`, `extended_rtp_timestamp`,
  `timestamp_ms` từ `av_pts` của từng frame.
- `rtp_timestamp_tracker.cpp`: `ToMilliseconds` trả 0 nếu ts nhỏ hơn mốc (không để trừ unsigned quay vòng).

Frame ra theo thứ tự hiển thị nên timestamp gửi AI tăng dần.

## 5. Lỗi 4 — giới hạn độ phân giải

- So theo **khung, không phân biệt ngang/dọc**: cạnh dài ≤ max(`max_width`,`max_height`) và cạnh ngắn ≤
  min(...). Lý do: điện thoại có thể gửi video dọc (720×1280); so cứng từng chiều với 1920×1080 sẽ bỏ
  nhầm stream dọc hợp lệ.
- Lớp 1, `H264Decoder::Open`: `codec_ctx_->max_pixels = FFALIGN(cạnh dài, 64) × FFALIGN(cạnh ngắn, 64)`.
  libavcodec từ chối cấp phát/decode frame có diện tích lớn hơn, nên không tốn CPU/RAM decode. FFmpeg
  so với kích thước đã căn lề (coded size bội 16/32, stride bội tới 64), nên giới hạn được nới lên bội 64
  để stream hợp lệ không bị chặn nhầm. `send_packet` lỗi mà kích thước SPS (`codec_ctx_->width/height`,
  vẫn giữ sau khi bị từ chối, đã kiểm chứng với libavcodec 62.11) vượt giới hạn → `DecodeStatus::Oversize`.
- Lớp 2: frame decode xong nhưng vượt một cạnh (lọt lớp 1 do nới) → bỏ trước khi copy.
- Đếm riêng: `VideoDecoderStats.oversize_frames` → `video.Stats.OversizeFrames` →
  `VideoStats.OversizeFrames` → metric `media_ai_video_decoder_oversize_frames_total`. Không tính vào
  `decode_errors`. Lần đầu mỗi session: log WARN `pipeline: video: độ phân giải vượt
  video.max_width/max_height, bỏ frame` (`session_id`, `max_width`, `max_height`).
- Không thêm cấu hình: dùng `video.max_width`/`max_height` có sẵn (mặc định 1920×1080; 0 = mặc định C++
  3840×2160). **Thay đổi hành vi:** stream lớn hơn giới hạn nay bị bỏ (trước vẫn decode).

## 6. Kiểm chứng (sau khi sửa)

Chạy lại **đúng** kịch bản tái hiện ở §1:

| # | Trước | Sau |
| --- | --- | --- |
| 1 | `fu_buffer_` 705 B / 13,4 MiB | 0 B (bỏ ngay ở mảnh vượt giới hạn) |
| 2 | bình thường 30–41/200 | bình thường **200/200** (3 lần chạy) |
| 3 | 3/9 frame sai | **0/9** sai |
| 4 | xuất frame 64×64 | 0 frame, đếm `oversize_frames` |

Regression test (giữ trong repo):

- `internal/codec/video/decoder_limits_test.go`: `TestDecoder_FuALimitAppliedPerFragment`,
  `TestDecoder_MaxDimensions` (libavcodec chặn; lọt diện tích nhưng vượt cạnh; đúng bằng giới hạn;
  stream dọc), `TestDecoder_BFrameTimestampsFollowFrames` (cần `ffmpeg` + `ffprobe`, thiếu thì skip).
- `internal/coordinator/session_reuse_test.go`: dùng lại ID 100 lần, quá hạn chờ, ID của session còn sống.
- `internal/config/session_reuse_wait_test.go`: mặc định, ghi đè, giới hạn, các file profile.

Toàn bộ: `go vet ./...` sạch; `go test ./...` pass; `-race` pass cho codec/video, coordinator, pipeline,
config, controlplane, result; Python `scripts/tests` 32/32. E2E video lab (mock AI, MF C++):
`change_background` + xuất MP4 PASS; `video_effects` keyword PASS (11 keyword MF xác nhận).
