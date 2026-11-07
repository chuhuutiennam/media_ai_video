# Video: contract và kiểm thử end-to-end

Để xem nhiệm vụ các phần đã thay đổi, sơ đồ luồng hiện tại, cách gateway/mock
dùng chung codec và ý nghĩa metrics, đọc
[Tổng hợp thay đổi và luồng video hiện tại](uncommitted-changes-summary.md).

Giữ nguyên hai `Session` phẳng `{callId}-tcore` / `{callId}-taccess`.
Không chuyển sang `CallSession{TCore,TAccess Termination{Audio,Video}}`.

## Contract DCSF

`POST /v1/vonras/call-sessions/{callId}/notify-event`:

```json
{
  "callId": "video-lab",
  "event": "ANSWER",
  "selectedService": "change_background"
}
```

- Mỗi dịch vụ video dùng **đúng một AI service** (danh mục `video.services`, xem
  [giai-phap-video-service-catalog.md](giai-phap-video-service-catalog.md)).
- `change_background`: video-only, H264/90000, `effect_type=bg_replace`, chỉ AI
  segmentation. Mặc định 1 leg: HTTP **201** chỉ trả `tcore_video_rtp_port` /
  `tcore_local_non_dc_video_media`; kết quả của session tcore đi về `callbackUrl` của
  `tAccess` trong ctrl-result. Không cần `bearerCapability: VIDEO`. Cần `video.enabled=true`
  và RTP port pool.
- `video_effects`: chỉ AI gesture (AI task `gesture_recognition`), cũng 1 leg như trên: chỉ trả
  `tcore_video_rtp_port` / `tcore_local_non_dc_video_media`; kết quả gesture đi về `callbackUrl`
  của `tAccess`. `callbackUrls` (override theo task) của dịch vụ 1 leg lấy từ `tCore`.
- `legs` trong `video.services` mặc định `tcore`; `legs: both` mới tạo thêm leg taccess.
- Không có cổng/SDP audio cho dịch vụ video; `codec` rỗng và `sample_rate=0`.
- `speech_to_text` / `realtime_translation` chỉ cấp audio PCMU/8000; **không** mở video
  kể cả khi `bearerCapability=VIDEO`.
- AI của dịch vụ không còn slot, mọi target mất kết nối, hoặc không có worker riêng: **503**
  `no AI worker available for service <tên>`. Với `video.ai.failover: true` (mặc định), target
  có kết nối gRPC đang `TRANSIENT_FAILURE` bị bỏ qua khi chọn, và mở stream lỗi thì thử target
  khác của cùng dịch vụ. Dịch vụ có nhiều target nên đặt `video.ai.max_retry` > 0 để AI lỗi giữa
  cuộc gọi được chọn lại sang target khác.
- Cổng RTP trong ANSWER luôn chẵn (`rtp.even_ports`, mặc định `true`; `port_start` phải chẵn), cổng
  lẻ kế tiếp dành cho RTCP. Gói RTCP gửi vào cổng video bị bỏ (`media_ai_video_ingress_rtcp_dropped_total`).
- `video.ai.stream_timeout_sec` là giới hạn cứng thời gian sống của stream AI video (mặc định 300s):
  cuộc gọi dài hơn sẽ mất AI. Triển khai IMS đặt `0` — xem `config/gateway-video-ims.yaml`.
- Nếu ANSWER có `callbackUrl`, gateway gửi `CALL_CTRL` (`actions=duplicate`) đến URL đó.
  DCSF gửi `ctrl-result` về URL gateway cung cấp, chứa `mediaResources` và
  `callbackUrl` **riêng** của `tCore` / `tAccess`. Đây là URL nhận kết quả,
  khác URL điều khiển trong ANSWER.
- Thiếu video config: 400; thiếu/hết port pool: 503; lỗi bind: 500;
  lỗi khởi tạo video pipeline/AI được báo đồng bộ: 502. Rollback đóng mọi
  session đã tạo, listener tự trả port sau khi đóng socket. Lỗi gRPC xảy ra sau khi
  mở stream được xử lý bởi stream manager, không thể đổi HTTP ACK đã gửi.

Gateway trả mask phân vùng; việc ghép nền, encode video đầu ra và phát video
đã thay nền không nằm trong luồng này.

## Contract AI và callback

RPC `/video.VideoStream/Segment`, theo `internal/ai/proto/video.proto`:

1. Message đầu là `VideoRequest.config`: effect, kích thước ảnh inference, FPS, threshold.
2. Các message sau là `VideoRequest.frame`: JPEG đã resize, `frame_id`,
   `rtp_timestamp`, `timestamp_ms` và kích thước gốc. Timestamp ms tăng dần
   theo từng stream; RTP timestamp giữ giá trị 32-bit gốc.
3. Mỗi frame trả `VideoResponse.mask`, echo `frame_id`, `rtp_timestamp`, session/stream ID.
   `is_final=true` xác nhận mask của frame, không đóng stream.
4. Có thể đóng nửa gửi của stream, hoặc gửi `is_eos=true`; EOS không kèm frame được chấp nhận.

`rle_counts` dùng **row-major** (theo từng hàng), nhị phân: background=0,
foreground=1; run đầu luôn là background, có thể bằng 0. Tổng counts phải bằng
`mask_width * mask_height` với `status=ok`. Ví dụ mask `[1,1,0,0]` → `[0,2,2]`.
Kích thước mask là kích thước ảnh inference, không phải video gốc. Đây là quy
ước của contract lab; worker/MF bên ngoài phải thống nhất quy ước này.

HTTP/2 callback có `event_type=video.mask.result`, `kind=video_mask`, `video_mask`,
`session_id`, `stream_id`, `is_final` và `mediaResources` đúng một phía với dịch vụ 2 leg;
dịch vụ 1 leg (`change_background`, `video_effects`) mang cả `tCore` và `tAccess` (ctrl-result single-leg).
Deduplicate video bằng `(session_id, stream_id, video_mask.frame_id)`; không dùng
`seq` của ASR (video hiện có thể trả `seq=0`). `status=error` có `error_message`
và không được tính là inference thành công.

## Chạy mock E2E trên lab

Hướng dẫn duy nhất cho build, copy binary, chạy mock, metrics, tiêu chí PASS và
troubleshooting nằm trong [scripts/lab-test-setup.md — Test video với mock AI](../scripts/lab-test-setup.md#video-mock-lab).

```bash
bash scripts/test-video-e2e.sh --ai-mode mock
# Lab có binary đã build:
bash scripts/test-video-e2e.sh --ai-mode mock --bin-dir ./bin --duration 60
```

`mock-callback-server` chỉ nhận callback và ghi JSON ra stdout như bản audio,
bổ sung mask video, protocol và path. Script tự gửi ANSWER (không có `callbackUrl`)
rồi gửi `ctrl-result` để cấu hình URL mỗi phía; đọc `callback.log` để kiểm tra mask.
Bài lab không kiểm tra CALL_CTRL và không dựng mock DCSF hoặc `/state`. Các POST
báo hiệu dùng HTTP/1.1; callback kết quả vẫn được kiểm tra HTTP/2.
Mock AI đọc pixel JPEG để tạo mask, không cần model/GPU.
Bash điều phối tiến trình, HTTP và vòng chờ; đoạn Python nhúng chỉ xử lý JSON,
mask/metrics và dò cổng. Trên lab chỉ cần copy `scripts/test-video-e2e.sh` cùng
các binary, không cần một script Python điều phối riêng.

## Chạy MediaPipe thật

Worker Python dùng MediaPipe Tasks ImageSegmenter, `LIVE_STREAM`, mặc định
GPU delegate. Mỗi stream có segmenter và callback riêng. Nó đọc JPEG, đổi RGB,
gọi `segment_async`, nhận confidence mask qua `result_callback`, threshold và
encode RLE; xử lý cả dạng mask `H×W` lẫn `H×W×1`.
Theo [hướng dẫn MediaPipe chính thức](https://ai.google.dev/edge/mediapipe/solutions/vision/image_segmenter/python).

MediaPipe có thể bỏ frame trong LIVE_STREAM khi bận. Worker giới hạn một frame
đang inference trên mỗi stream, chờ callback trước khi nhận frame tiếp theo;
các stream vẫn chạy đồng thời. Callback timestamp phải khớp frame đang chờ,
giữ nguyên `frame_id`/`rtp_timestamp`. EOS được xử lý sau mask cuối. Mặc định
hết 10 giây chưa có callback thì RPC kết thúc bằng `DEADLINE_EXCEEDED`;
có thể đổi bằng `--result-timeout` khi chạy worker riêng.

Trên server Ubuntu, cần NVIDIA driver hỗ trợ EGL/OpenGL ES 3.1 trở lên và
bản MediaPipe có GPU support. Đường GPU này không sử dụng CUDA trực tiếp;
có CUDA 12 chưa xác nhận được EGL hoặc GPU delegate hoạt động.
Xem [MediaPipe GPU support](https://developers.google.com/edge/mediapipe/framework/getting_started/gpu_support).
AI segmentation của `change_background` là repo riêng
[mediapipe](https://github.com/chuhuutiennam/mediapipe), clone cạnh repo này
(`../mediapipe`, hoặc đặt biến `MEDIAPIPE_DIR`); model nằm ở `../mediapipe/checkpoint/`.
Cài thư viện hệ thống và môi trường Python riêng (yêu cầu ≥ 3.10, xem README của repo đó):

```bash
sudo apt-get update
sudo apt-get install -y libegl1 libgles2 python3-venv
python3 -m venv ../mediapipe/.venv-linux
../mediapipe/.venv-linux/bin/python -m pip install -e ../mediapipe
../mediapipe/.venv-linux/bin/python -m pip check

MEDIAPIPE_PYTHON=../mediapipe/.venv-linux/bin/python \
bash scripts/test-video-e2e.sh --ai-mode mediapipe \
  --mediapipe-delegate gpu \
  --model ../mediapipe/checkpoint/selfie_segmenter.tflite
```

Script chạy `python -m mediapipe_video` từ `$MEDIAPIPE_DIR/src` bằng `MEDIAPIPE_PYTHON`
(mặc định `PYTHON`).

Gateway và sender vẫn cần FFmpeg/CGO theo phần build lab; nếu đã có binary,
thêm `--bin-dir ./bin` để bỏ bước build.

Nếu chạy trong Docker, cần NVIDIA Container Toolkit trên host; thêm
`--gpus all -e NVIDIA_DRIVER_CAPABILITIES=compute,utility,graphics` vào lệnh
`docker run` của image worker. `graphics` cấp thư viện OpenGL/EGL; mặc định
chỉ có `compute,utility` là chưa đủ cho đường này.
Theo [tài liệu NVIDIA](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/docker-specialized.html#driver-capabilities).
Biến môi trường này dành cho NVIDIA container runtime, không cài driver trên host.

Worker kiểm tra model và GPU trước khi mở cổng gRPC, không tự chuyển sang CPU.
Log `ready` ghi `running_mode=LIVE_STREAM`, `delegate=gpu`. Khi chạy trên server,
kiểm tra log MediaPipe có renderer GPU NVIDIA và theo dõi GPU trong lúc inference;
`llvmpipe`/`softpipe` là renderer phần mềm, không xác nhận GPU NVIDIA.
Nếu gặp lỗi `GPU emulation detected`, lỗi EGL hoặc GPU support bị tắt trong bản
MediaPipe, cần sửa driver/container hoặc dùng bản MediaPipe có GPU support.

Mặc định dùng mẫu testsrc2 để xác nhận model thực sự chạy. Để đánh giá trên ảnh
người/video cuộc gọi, truyền dữ liệu của bạn:

```bash
MEDIAPIPE_PYTHON=../mediapipe/.venv-linux/bin/python \
bash scripts/test-video-e2e.sh --ai-mode mediapipe \
  --mediapipe-delegate gpu \
  --model ../mediapipe/checkpoint/selfie_segmenter.tflite \
  --input /path/to/person.mp4 --duration 5
```

Worker hiện phục vụ SelfieSegmenter (person/background). Model đa lớp cần chạy
worker riêng với `--foreground-index` đúng class của model; không tự giả định
mọi class đều là người. Lệnh tải dùng alias `latest`; muốn tái lập chính xác hãy
lưu model cùng SHA-256. Dependency Python được pin trong `pyproject.toml` của repo mediapipe.

Máy không có GPU có thể kiểm thử cùng luồng LIVE_STREAM bằng cách chọn CPU
rõ ràng (kết quả này không xác nhận khả năng chạy GPU):

```bash
MEDIAPIPE_PYTHON=../mediapipe/.venv-linux/bin/python \
bash scripts/test-video-e2e.sh --ai-mode mediapipe --mediapipe-delegate cpu \
  --model ../mediapipe/checkpoint/selfie_segmenter.tflite
```

AI service đã chạy sẵn (repo mediapipe cho `change_background`, repo
[gesture](https://github.com/chuhuutiennam/gesture) cho `video_effects`):

```bash
bash scripts/test-video-external-e2e.sh --ai-target 127.0.0.1:50052
bash scripts/test-video-external-e2e.sh --gesture-only --gesture-target 127.0.0.1:50053
```

Chạy gesture cần ảnh emoji từ repo gesture (`../gesture/assets/emoji`, biến `GESTURE_DIR`).

Chế độ external không khởi động hay dừng worker đó. Worker phải implement contract
protobuf trên; harness không sửa kết nối hay triển khai của bạn.

## Test hồi quy

```bash
go build ./...
go vet ./...
go test ./...
go test -race ./internal/controlplane/... ./internal/coordinator/... \
  ./internal/ai/... ./internal/ingress/rawrtp/... ./internal/pipeline/... \
  ./cmd/mock-ai-worker/... ./cmd/mock-video-rtp-sender/... ./internal/config/...
go test -race ./cmd/mock-callback-server
python3 -m unittest discover -s scripts/tests -p test_video_e2e.py -v
```

Test của hai AI service chạy trong repo của chúng (`../mediapipe`, `../gesture`).

Các test này cần quyền mở socket loopback. Có test decoder cấu hình một phần
(chỉ width/height), RTP có padding/extensions và datagram lớn, queue đầy,
port exhaustion/bind failure/AI startup failure, và config truyền đúng video queues.
Các test coordinator cũ bơm packet vào queue và mock client trong bộ nhớ;
script E2E bổ sung HTTP, UDP, gRPC và inference qua các tiến trình thật.

Lỗi đã sửa trong đợt này: mock worker thiếu implementation video; cấp port audio
bị mất; rollback trả port hai lần; video startup lỗi vẫn ACK; queue video không
được truyền từ config; decoder coi các giới hạn bỏ trống là 0; RTP padding lẫn
vào payload; script thiếu sender/config và có thể báo PASS mà thiếu một phía. Bài lab hiện
kiểm tra thêm packet/frame/callback đầy đủ, metrics video và RTP port sau DELETE.

## Môi trường Studio đã dùng để kiểm thử

Môi trường Studio hiện tại không có GPU NVIDIA khả dụng. Kiểm thử LIVE_STREAM
chọn `--mediapipe-delegate cpu` với model thật; test unit kiểm tra callback,
hai stream đồng thời, EOS, timeout, cancellation và lỗi khởi tạo GPU.
Kiểm thử GPU phải chạy trên server có GPU vật lý theo hướng dẫn ở trên.
Model đã thử có SHA-256:
`191ac9529ae506ee0beefa6b2c945a172dab9d07d1e802a290a4e4038226658b`.
