# Video: contract và kiểm thử end-to-end

Danh mục dịch vụ và luồng AI xem
[thiết kế video service catalog](giai-phap-video-service-catalog.md).
Hướng dẫn gesture → keyword của module hiện tại nằm ở
[phần Gesture → MF keyword](#gesture--mf-keyword-trong-module-hiện-tại) bên dưới.

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
  kể cả khi `bearerCapability=VIDEO`. Trên gateway chỉ video (`audio.enabled: false` — mọi
  config `gateway-video-*.yaml` và config do `test-video-e2e.sh` sinh) hai dịch vụ này trả **400**
  `service <tên> is not supported by this gateway (audio.enabled=false)`, không tạo session.
  `audio.enabled: false` bắt buộc `ai.grpc_target: ''`.
- AI của dịch vụ không còn slot, mọi target mất kết nối, hoặc không có worker riêng: **503**
  `no AI worker available for service <tên>`. Với `video.ai.failover: true` (mặc định), target
  có kết nối gRPC đang `TRANSIENT_FAILURE` bị bỏ qua khi chọn, và mở stream lỗi thì thử target
  khác của cùng dịch vụ. Dịch vụ có nhiều target nên đặt `video.ai.max_retry` > 0 để AI lỗi giữa
  cuộc gọi được chọn lại sang target khác.
- Cổng RTP trong ANSWER luôn chẵn (`rtp.even_ports`, mặc định `true`; `port_start` phải chẵn), cổng
  lẻ kế tiếp dành cho RTCP. Gói RTCP gửi vào cổng video bị bỏ (`media_ai_video_ingress_rtcp_dropped_total`).
- DELETE → 204 (404 nếu không còn session). Cổng vừa trả chờ `rtp.port_reuse_delay_ms` (mặc định 5000)
  mới cấp lại, cổng trả trước cấp trước: cuộc gọi kế tiếp nhận cổng khác nên RTP còn đến của cuộc gọi
  cũ không lọt vào. Cổng đang chờ hiện ở `/v1/connections` `per_session_cooling` và metric
  `media_ai_rtp_ports_cooling`; dải cổng nhỏ trong lab có thể hết cổng (503) khi tạo/xoá liên tục.
- ANSWER chờ mở stream AI tối đa `video.ai.open_timeout_ms` mỗi leg (mặc định 5000, gồm mọi lần thử
  target); AI không phản hồi (SYN không trả lời) → 502 `... timed out after 5s ...` thay vì treo ~20s.
  Số leg × `open_timeout_ms` phải nhỏ hơn `server.write_timeout_sec` (mặc định 10), nếu không gateway
  không khởi động.
- `video.ai.stream_timeout_sec` mặc định `0`: stream AI video sống theo cuộc gọi.
  Đặt giá trị dương sẽ giới hạn cứng thời gian sống của stream, nên cuộc gọi dài hơn
  giới hạn đó sẽ mất AI. Profile IMS cũng đặt `0` — xem `config/gateway-video-ims.yaml`.
- `callbackUrl` của ANSWER phải là `http`/`https` tuyệt đối có host, sai → 400. CALL_CTRL dùng h2c cho
  `http://`, TLS + HTTP/2 cho `https://`; DELETE huỷ CALL_CTRL đang chờ, và CALL_CTRL lỗi chỉ đóng đúng
  cuộc gọi của nó (không đóng cuộc gọi mới dùng lại `callId`).
- Nếu ANSWER có `callbackUrl`, gateway gửi `CALL_CTRL` (`actions=duplicate`) đến URL đó.
  DCSF gửi `ctrl-result` về URL gateway cung cấp, chứa `mediaResources` và
  `callbackUrl` **riêng** của `tCore` / `tAccess`. Đây là URL nhận kết quả,
  khác URL điều khiển trong ANSWER. URL trong ctrl-result (`callbackUrl`, `callbackUrls`) phải là
  `http`/`https` tuyệt đối có host, không `user:password`; sai → 400, không áp dụng phần nào.
  Kết quả sinh ra trước ctrl-result đi về `callback.url` và không có `mediaResources`.
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


## Gesture → MF keyword trong module hiện tại

RTPGW hỗ trợ phần chuẩn hóa ở mục 6.2–6.3 của
[thiết kế keyword](thiet-ke-e2e-gesture-segmentation-keyword.md). Module hiện tại
vẫn dùng một AI cho mỗi dịch vụ: `change_background` trả mask segmentation,
`video_effects` trả gesture. Cập nhật này không bật hai task trong cùng session.
Các kết quả E2E hai task/hai leg trong tài liệu nguồn không phải kết quả kiểm
chứng của module này.

Dùng `config/gateway-video-ims.yaml` làm nguồn cấu hình. Profile này đã chọn
`keyword`; mặc định trong Go vẫn là `legacy`. Với profile khác, bổ sung phần
dưới đây. CLI chỉ ghi đè cho lần test tương ứng.

```yaml
callback:
  gesture_keywords:
    mode: keyword
    mapping:
      Wave: {topic_label: EMOTION, topic_info: congrat}
      Thumb_Up: {topic_label: EMOTION, topic_info: approve}
```

Mapping có sẵn đủ tám gesture; các mục trong YAML chỉ override nhãn tương ứng.
Mỗi mục override phải có đủ `topic_label` và `topic_info`; các nhãn khác giữ
mapping mặc định. Đây là cấu hình nghiệp vụ, không thay đổi model AI.

| Gesture | `topic_label` | `topic_info` | Effect MF lab ghi nhận |
| --- | --- | --- | --- |
| Closed_Fist | EMOTION | determination | sparks |
| Open_Palm | EMOTION | hello | greeting |
| Pointing_Up | EMOTION | attention | highlight |
| Thumb_Down | EMOTION | disapprove | thumbs_down |
| Thumb_Up | EMOTION | approve | thumbs_up |
| Victory | EMOTION | celebrate | confetti |
| ILoveYou | EMOTION | love | hearts |
| Wave | EMOTION | congrat | fireworks |

Các tham số dưới `callback.gesture_keywords`:

| Field | Mặc định Go | Ý nghĩa / giới hạn khi nạp YAML |
| --- | --- | --- |
| `mode` | `legacy` | `legacy` hoặc `keyword` |
| `base_url` | Rỗng | Origin HTTP(S) MF tùy chọn; không userinfo/query/fragment, path chỉ rỗng hoặc `/` |
| `cache_size` | 4096 | 8..65536 key đã ACK cho mỗi sink |
| `open_palm_max_gap_ms` | 500 | 1..60000 ms giữa hai mẫu Open Palm |
| `open_palm_hold_ms` | 200 | 1..60000 ms giữ Open Palm; vẫn cần ít nhất 3 mẫu |
| `open_palm_min_score` | 0.6 | Số hữu hạn trong khoảng (0, 1] |
| `mapping` | Bảng 8 nhãn trên | Key không rỗng/`None`; mỗi topic/info không toàn khoảng trắng, tối đa 128 byte |

Bỏ field để lấy mặc định; đặt `0` cho các giới hạn dương nêu trên bị từ chối
khi nạp config. `base_url` rỗng dùng origin callback do ctrl-result cung cấp.

Gesture dùng reaction còn hiệu lực, coalesce các tay cùng keyword và chỉ ghi
nhận event đã gửi sau ACK 2xx. `Open_Palm` không có reaction được xác nhận bằng
ít nhất ba snapshot, giữ 200 ms và score ≥ 0.6; mất tay/đổi nhãn/gap > 500 ms
reset quá trình xác nhận. Gateway không map trực tiếp nhãn thô `None`; nếu tay
vẫn có `active_reaction` hợp lệ thì reaction đó tiếp tục được mapping, kể cả
`raw_label` đang là `None` hoặc `stable_label` rỗng. Không có reaction/fallback
được xác nhận, reaction chưa có mapping hoặc snapshot không có tay thì không
phát keyword. Gesture dùng FIFO theo session và HTTP/2 pool riêng với mask.

Body HTTP có đúng hai trường:

```json
{"topic_label":"EMOTION","topic_info":"approve"}
```

RTPGW gửi body tới
`/nmf-mrm/v2/contexts/{contextId}/termination/{terminationId}/keyword`, kèm
`Idempotency-Key` SHA-256 giữ nguyên khi retry. ID resource lấy từ `ctrl-result`,
không nằm trong body. Trong service một leg, session tCore giữ cả hai resource
nhưng keyword đi tới **termination tAccess**, giống callback kết quả hiện tại.
Trong service `legs: both`, mỗi session dùng resource của leg đó.

Theo API hiện tại, override task của service một leg vẫn đặt trong
`mediaResources.tCore.callbackUrls.gesture_recognition`; URL keyword đầy đủ ở
đây phải chứa context/termination **tAccess**. Với hai leg, đặt override ở từng
resource tương ứng. URL đầy đủ được ưu tiên và kiểm tra khớp resource; nếu chỉ
có callback cũ, gateway dựng path từ `base_url` hoặc origin callback.
`base_url` chỉ được chứa scheme/host/port; có thể bỏ để dùng origin callback.

Ví dụ ctrl-result cho dịch vụ `video_effects` một leg (cổng MF là minh họa,
runner tự điền cổng thật):

```http
POST /v1/vonras/call-sessions/call-demo/ctrl-result
Content-Type: application/json
```

```json
{
  "callId": "call-demo",
  "mediaResources": {
    "tCore": {
      "contextId": "ctx-tCore",
      "termination": {"terminationId": "term-tCore"},
      "callbackUrl": "http://127.0.0.1:9999/results/tCore",
      "callbackUrls": {
        "gesture_recognition": "http://127.0.0.1:9999/nmf-mrm/v2/contexts/ctx-tAccess/termination/term-tAccess/keyword"
      }
    },
    "tAccess": {
      "contextId": "ctx-tAccess",
      "termination": {"terminationId": "term-tAccess"},
      "callbackUrl": "http://127.0.0.1:9999/results/tAccess"
    }
  }
}
```

MF nhận POST HTTP/2 với `Content-Type: application/json`, `Idempotency-Key`
và body đúng hai field như trên; không có frame ID, geometry, TTL hay resource
trong body. Schema endpoint ở [OpenAPI callback](openapi-callback.yaml).

| Phản hồi / sự kiện | Xử lý |
| --- | --- |
| HTTP 2xx | Đánh dấu các event trong request đã ACK; không gửi lại trong các snapshot tiếp theo khi key còn trong cache |
| HTTP 4xx | Không retry request đó; ghi lỗi; MF lab trả 409 nếu cùng resource/key nhưng body khác |
| HTTP 5xx, lỗi mạng hoặc timeout | Retry theo `callback.max_retry`, `callback.retry_backoff_ms`, cùng body và key |
| Hết retry / hết deadline dispatcher | Trả lỗi cho dispatcher; event chưa ACK có thể được thử lại từ snapshot tiếp theo |

`callback.timeout_ms` giới hạn mỗi HTTP attempt. `result.send_timeout_ms`
giới hạn toàn bộ lần dispatcher gửi snapshot, gồm mọi keyword và retry; deadline
này có thể kết thúc trước khi đủ số retry cấu hình. Trong profile IMS hiện tại
cả hai đều 2000 ms. `callback.max_retry: 3` nghĩa là tối đa 1 lần đầu + 3 retry
khi deadline còn cho phép; đây không phải timeout gRPC bên worker gesture.

Cache gateway có giới hạn theo sink, không lưu bền qua đóng session/thay sink
hoặc restart. MF lab chống trùng theo context/termination/key, tối đa 4096 mục,
TTL 5 phút; cùng key/body trả 200 với `duplicate=true` và không kích hoạt lại.
Không xem cơ chế này là đảm bảo exactly-once vô hạn; MF thật cần chính sách
idempotency của môi trường triển khai.

`test-video-external-e2e.sh` hỗ trợ `--gesture-callback-mode keyword`. Runner
kiểm tra số snapshot AI (kể cả snapshot không phát keyword), coverage RTP,
request mapping, ACK HTTP/2 2xx và receipt MF cùng idempotency key/body/resource.
MF C++ nhận keyword, chống retry trùng và log effect với `rendered=false`; keyword
chưa tạo hiệu ứng trên MP4. `--require-gesture` chỉ dùng với callback legacy để
kiểm tra sticker; keyword mode tự yêu cầu ít nhất một keyword nhận tại MF.

Khi chạy `--gesture-callback-mode keyword`, terminal chỉ in tóm tắt khi gesture
thay đổi, theo từng session và track tay:

```text
[GESTURE] START session=call-tcore track=9 gesture=Thumb_Up frame=129 keyword=EMOTION/approve
[GESTURE] END session=call-tcore track=9 gesture=Thumb_Up frames=129..209 keyword=EMOTION/approve
```

`START` là frame đầu có gesture đã mapping. `END` ghi frame đầu/cuối còn thấy
gesture đó trong các snapshot đã mapping; số frame có thể cách nhau do sampling.
Giữ nguyên gesture không in lặp, kể cả event ID đổi. Khi gesture đổi, biến mất
hoặc hết dữ liệu, runner đóng khoảng trước; nếu có gesture mới thì mở khoảng
mới. Hai tay được theo dõi độc lập. Runner không in JSON từng frame lên terminal.
START/END là tóm tắt từ log mapping, không phải ACK MF và không phải bản tin
được gửi tới MF. Frame kết thúc là mẫu cuối còn có reaction đã mapping, không
phải kết luận về thời điểm bàn tay đổi tư thế giữa hai mẫu.

File `gateway.log` lưu đầy đủ kết quả và các thông tin chẩn đoán khác:

- `keyword: after mapping`: session/response/frame, track, raw label/score,
  stable label, gesture, event ID và `topic_label`/`topic_info`.
- `keyword: skipped`: thông tin nguồn liên quan và `reason` giải thích vì sao
  không tạo hoặc không gửi keyword cho kết quả đó.
- `keyword: delivered` / `keyword: delivery failed`: trạng thái gửi MF.

Các lý do bỏ qua thường gặp:

| `reason` | Ý nghĩa |
| --- | --- |
| `unmapped_gesture` | Reaction đã có nhưng gesture chưa có trong bảng mapping |
| `no_confirmed_reaction` | AI chưa xác nhận reaction có hiệu lực |
| `no_hands` | Snapshot không có tay |
| `open_palm_pending` | Open Palm chưa đủ số mẫu hoặc thời gian giữ |
| `open_palm_low_score` | Điểm Open Palm thấp hơn ngưỡng |
| `duplicate_event` | Event đã được MF ACK; bỏ qua gửi trùng |
| `late_snapshot` | Snapshot cũ hơn kết quả đã xử lý |

Mapping thành công chưa đồng nghĩa MF đã nhận; đối chiếu dòng `delivered`.
Một event trùng vẫn có log mapping thành công và thêm log `duplicate_event`.
Lỗi cấu hình/snapshot/địa chỉ đích được log `WARN` với reason tương ứng.
Legacy không bật luồng log keyword này. Nếu dùng `--bin-dir`, cần rebuild
gateway để có các log mapping mới.

`raw_score` là điểm của `raw_label` do AI trả về. Ví dụ `raw_label: "None"`
với điểm 0.98 vẫn nghĩa là AI chọn nhãn None ở frame đó, không xác nhận một
gesture khác. `stable_label` rỗng nghĩa là chưa có nhãn ổn định; `gesture` rỗng
nghĩa là gateway chưa chọn được reaction để mapping.

Trên `/mnt/c` của WSL, đọc file đang ghi có thể tạm lỗi `ENODATA` / `No data
available`. Bộ hiển thị log tự thử lại từ vị trí đã đọc, giữ dòng chưa ghi xong,
không phát lại các dòng cũ. Cảnh báo đọc log này thuộc bài test; đối chiếu
`gateway.log` và `mf-events.jsonl` để kiểm tra kết quả AI/MF.

### Mặc định xử lý từ IMS, kết nối lab trên local

`test-video-external-e2e.sh` và `test-video-e2e.sh --ai-mode external` mặc định
đọc `config/gateway-video-ims.yaml`. Runner lấy các thông số xử lý từ file này
rồi tạo cấu hình runtime cho lab. **Không cần chỉnh sửa file IMS để test local.**

Thông số xử lý ưu tiên **CLI được truyền rõ → profile → mặc định Go cho field
còn thiếu**. Wrapper không tự chèn timeout callback khác với profile.

| Tham số CLI | Field cấu hình | Giá trị IMS hiện tại |
| --- | --- | --- |
| `--gesture-fps` | `video.services.video_effects.gesture.infer_fps` | 30 FPS |
| `--gesture-callback-mode` | `callback.gesture_keywords.mode` | keyword |
| `--infer-size` | `video.services.change_background.segmentation.infer_width`, `infer_height` | 256 × 256 |
| `--callback-timeout` | `callback.timeout_ms` | 2000 ms = 2 giây |
| `--callback-workers` | `result.dispatcher_workers` | 32 |
| `--callback-retries` | `callback.max_retry` | 3 |

Các thông số worker pool, queue, jitter, AI timeout/retry, mapping/dedup và
HTTP callback khác tiếp tục lấy từ profile. Địa chỉ kết nối trong lab được đặt
riêng trong cấu hình runtime:

| Thành phần | Kết nối lab |
| --- | --- |
| Gateway HTTP và metrics | `127.0.0.1`, port trống; `--gw-port` để chọn port |
| TLS gateway | Tắt trong lab; không cần `/etc/tls/gateway.crt` hay key |
| RTP | `127.0.0.1`, dải UDP trống cho lần test |
| Segmentation AI | `127.0.0.1:50052`; đổi bằng `--ai-target` |
| Gesture AI | `127.0.0.1:50053`; đổi bằng `--gesture-target` |
| MF callback | Receiver local do runner khởi động, URL gửi qua ctrl-result |
| DCSF / PCAP | Không preconnect host IMS; không bật capture hệ thống |

Timeout idle của session được kéo dài tối thiểu tới hết thời gian phát, drain
và cleanup của bài test. Những thay đổi môi trường lab này chỉ áp dụng trong
runtime. Không sử dụng IP, cert, DNS AI/MF/DCSF mẫu của IMS cho kết nối lab.

Hai AI workers phải chạy sẵn. Mỗi lần test chọn một dịch vụ, một AI. Ví dụ:

```bash
# Gesture keyword, worker đang chạy tại 127.0.0.1:50053.
bash scripts/test-video-external-e2e.sh \
  --gesture-only --gesture-callback-mode keyword \
  --input data/gesture_sample/static_gesture.mp4 \
  --duration 30 --artifacts-dir tmp/video-external

# Gesture legacy, worker đang chạy tại 127.0.0.1:50053.
bash scripts/test-video-external-e2e.sh \
  --gesture-only --gesture-callback-mode legacy \
  --input data/gesture_sample/static_gesture.mp4

# Segmentation, worker đang chạy tại 127.0.0.1:50052.
bash scripts/test-video-external-e2e.sh --input /path/to/video.mp4
```

Bỏ `--bin-dir` để build gateway, RTP sender và MF từ source. Để test keyword,
đổi `--gesture-callback-mode legacy` thành `--gesture-callback-mode keyword`
hoặc bỏ option này khi profile đang chọn keyword. CLI chỉ thay đổi lần chạy;
`--gateway-config FILE` chọn nguồn thông số xử lý khác và vẫn dùng kết nối lab.

Artifacts để kiểm tra nguồn và cấu hình thực chạy:

- `gateway-source.yaml`: nguyên byte file nguồn, gồm comment.
- `gateway.yaml`: cấu hình xử lý từ nguồn, áp dụng kết nối lab và CLI.
- `gateway-config-overrides.json`: field trước/sau; `option: "lab"` là thay đổi
  môi trường test, các option bắt đầu `--` là tham số CLI.
- `options.json`: đường dẫn và SHA-256 nguồn/runtime, các giá trị test hiệu lực.
- `gateway.log`: nguồn snapshot, mapping, skip reason, request/body/key và HTTP ACK.
- `mf-events.jsonl`: receipt thực tế của MF, cùng key/body/resource.
- `metrics-{before,active,after,cleanup}.prom`: counters và cleanup của lần chạy.
- `progress.json`, `result.json`: số snapshot/keyword, PASS/FAIL, video đầu ra.

PASS của keyword yêu cầu đủ snapshot và độ phủ RTP, đối chiếu các bước source
→ mapping → HTTP request → ACK HTTP/2 2xx → receipt MF. Số snapshot và số keyword
khác nhau do các frame không có reaction và dedup; không yêu cầu một POST/frame.
Runner kiểm tra lỗi RTP/decode/AI/callback, drain và trả tài nguyên khi DELETE.
START/END trên terminal không tham gia quyết định PASS.

Đọc YAML cần PyYAML trong Python được chọn bằng `PYTHON`. Các tham số riêng
của bài test như input, duration, kích thước video nguồn, timeout chờ, artifacts
và MF playout tiếp tục dùng mặc định runner hoặc CLI. Chế độ mock/mediapipe
trong runner chung tiếp tục tạo config lab khi không chọn profile.

Luồng test là RTP → gateway → AI external → MF test. Runner mô phỏng DCSF qua
ANSWER/ctrl-result/DELETE; chưa tích hợp DCSF/MF/H.248 production.

### Đối chiếu bằng code và kết quả lab

Kiểm tra mapping, resource, Open Palm, dedup/retry, thứ tự, legacy và log:

```bash
go test -mod=readonly ./internal/result -run 'TestGesture(Keyword|Legacy)|TestOrderedGesture' -count=1
go test -mod=readonly ./internal/config ./internal/coordinator -run TestGestureKeyword -count=1
python3 -m unittest discover -s scripts/tests -p 'test_*.py' -v
bash scripts/build-mf-cpp.sh
ctest --test-dir build/mf -R keyword_receiver --output-on-failure
```

Go build/test video cần CGO và FFmpeg development libraries; build MF cần
CMake, compiler C++, FFmpeg và nghttp2 development libraries như runner lab.
Các lệnh trên cần chạy từ root repository. Nếu đặt `BUILD_DIR` cho script build
MF thì dùng cùng đường dẫn đó cho `ctest --test-dir`.

Run thực đã đối chiếu: `tmp/video-external/video-e2e-qkqx82n3` ngày 2026-11-10,
input `data/gesture_sample/static_gesture.mp4`, 30 giây, 15 FPS inference:

| Chỉ tiêu | Kết quả trong artifacts của run |
| --- | --- |
| Kết quả | PASS |
| Packet RTP | 1760, không mất ở các stage RTP đã đo |
| Snapshot gesture nhận tại gateway | 450 |
| Keyword MF nhận | 6: 3 approve, 1 disapprove, 2 celebrate |
| Gesture tạo keyword | Thumb_Up, Thumb_Down, Victory |
| MP4 | 900 frame nguồn/record, cộng một frame đánh dấu cuối khi encode |

Đây là run một dịch vụ/AI. Replay log qua bộ tóm tắt hiện tại cho 6 khoảng
gesture và 12 dòng START/END. Kết quả không xác nhận nhận diện đủ cả 8 gesture,
capacity nhiều cuộc gọi, render effect keyword hoặc tích hợp MF/DCSF thật.
Artifacts nằm trong `tmp/`, có thể bị dọn và không thay thế bộ test tự động.

Các vị trí triển khai:
[normalizer](../internal/result/gesture_keyword.go),
[HTTP callback/retry](../internal/result/http_callback.go),
[dispatcher FIFO](../internal/result/dispatcher.go),
[cấu hình](../internal/config/config.go),
[MF keyword receiver](../mf/src/keyword_receiver.cpp),
[runner](../scripts/test-video-e2e.sh),
[bộ tóm tắt START/END](../scripts/stream-keyword-log.py).

### Chẩn đoán `timeout after 30s: gateway ready`

Các bản runner trước từng chép cả TLS/IP IMS vào runtime, gây lỗi thiếu cert
hoặc truy cập địa chỉ mẫu. Runner hiện dùng HTTP local và tự chọn port nên
không cần sửa IP/TLS trong `gateway-video-ims.yaml`. Chạy lại script mới để
tạo runtime mới; không dùng lại `gateway.yaml` của lần chạy lỗi cũ.

Nếu gateway vẫn không khởi động, đọc `gateway.log`. Gateway tự dừng khi HTTP
listener lỗi; nếu probe HTTP thất bại, timeout kèm URL và lỗi curl cuối cùng,
lưu trong `gateway-health-error.txt`. Xem `options.json` để xác nhận endpoint
AI và mode thực chạy. Legacy cần `--gesture-callback-mode legacy` nếu profile
đang chọn keyword.

### Chẩn đoán `ai_recv_errors_total` khi test gesture

Counter `media_ai_video_ai_recv_errors_total` tăng nghĩa là luồng nhận kết quả
AI gặp lỗi; đọc trường `error` của log `ai: video recv error`/`ai: video stream
stopped` trong `gateway.log`. Runner đưa lỗi gRPC này vào thông báo FAIL và lưu
`ai_failure` trong `result.json`; các kiểm tra đầy đủ/mất dữ liệu vẫn áp dụng.

Nếu worker trả `DeadlineExceeded: MediaPipe gesture callback timed out`, timeout
nằm trong worker gesture lúc chờ MediaPipe LIVE_STREAM trả callback. Các tham
số `--callback-timeout` (HTTP tới MF) và `--drain-timeout` không đổi deadline này.
Đối chiếu log worker tại `../gesture/run/logs/mediapipe-gesture.log` và cấu hình
runtime `../gesture/run/etc/config.yaml`, đặc biệt `gesture.delegate` và
`gesture.result_timeout_secs`.

Runner external lấy FPS từ profile IMS (**15 FPS** hiện tại). Có thể ghi đè
riêng cho lần test để giảm tải worker CPU và ghi artifact trên filesystem Linux của WSL:

```bash
bash scripts/test-video-external-e2e.sh \
  --gesture-only --gesture-target 127.0.0.1:50053 \
  --gesture-callback-mode keyword --gesture-fps 10 \
  --input data/gesture_sample/static_gesture.mp4 \
  --duration 6 --artifacts-dir /tmp/video-external
```

Nếu không truyền `--gesture-fps`, FPS lấy từ `video.services.video_effects.gesture.infer_fps` trong
profile. Có thể đổi giá trị mặc định trong YAML hoặc truyền `--gesture-fps 10`
để chỉ thay đổi lần test hiện tại. Số kết quả tối thiểu tự tính theo FPS hiệu lực.

Với `gesture.static_gap_ms: 150` hiện tại của worker, 10 FPS vẫn giữ khoảng
cách mẫu khoảng 100 ms. Nếu hạ xuống 5 FPS (200 ms/mẫu), phải điều chỉnh
`static_gap_ms` của worker cho phù hợp; nếu không nhãn tĩnh có thể liên tục
bị reset và không phát reaction/keyword.

Nếu worker CPU vẫn timeout với tải thấp, có thể tăng
`gesture.result_timeout_secs` từ 2 lên 10 trong **config runtime của worker**,
rồi restart worker để nạp lại. Điều này tăng thời gian chờ callback, không chữa
được graph MediaPipe bị lỗi hoặc callback mất vĩnh viễn; cần đọc log native nếu
vẫn tái diễn. Hạ FPS/giảm I/O là bước chẩn đoán tải, không phải bằng chứng đã sửa
mọi nguyên nhân timeout. Một lần chạy có packet loss hoặc frame coverage không
đủ vẫn phải FAIL dù hết lỗi gRPC.
