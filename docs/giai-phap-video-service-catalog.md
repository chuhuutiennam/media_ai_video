# Giải pháp: danh mục dịch vụ video — mỗi dịch vụ một AI service

Trạng thái: **đã triển khai** (duyệt 2026-11-05, code 2026-11-06). Kết quả kiểm chứng ở §9.

## 1. Bối cảnh và mục tiêu

Mỗi cuộc gọi đăng ký một dịch vụ người dùng (`selectedService` trong NOTIFY ANSWER).
Mục tiêu: mỗi dịch vụ video gắn với **đúng một AI service riêng** (địa chỉ worker và
giới hạn stream riêng), cấu hình bằng YAML, dịch vụ này không dùng worker của dịch
vụ khác. Thêm một dịch vụ mới dùng giao thức AI đã có (VideoStream/Segment) chỉ cần
sửa config, không build lại.

## 2. Quyết định đã chốt

| # | Quyết định |
| --- | --- |
| 1 | Dùng danh mục dịch vụ trong config (phương án 1). |
| 2 | Bắt buộc **1 dịch vụ ↔ 1 AI**: `change_background` chỉ dùng AI segmentation, `video_effects` (trước đây tên `gesture_recognition`) chỉ dùng AI gesture — hai AI service khác nhau. |
| 3 | Chỉ áp dụng cho **video**. Audio nhiều AI (STT, dịch) thuộc project `media_ai`; `speech_to_text`, `realtime_translation` trong repo này giữ nguyên. |
| 4 | Chưa thêm `keyword`, `voice_controller_emoji`. |
| 5 | Quy tắc 1:1 áp dụng cho **cả config cũ** (không có danh mục): `change_background` không còn mở gesture. |
| 6 | Sửa luôn script E2E và docs của video theo quy tắc 1:1 trong phạm vi việc này. |
| 7 | AI Engine (`video.ai.engine_target`) giữ là lựa chọn chung, tuân theo 1:1 (mỗi source một task); **chưa** đưa vào danh mục ở P1. |
| 8 | (2026-11-06) AI service là repo riêng: `change_background` → [mediapipe](https://github.com/chuhuutiennam/mediapipe) (`../mediapipe`, :50052), dịch vụ gesture đổi tên thành **`video_effects`** → [gesture](https://github.com/chuhuutiennam/gesture) (`../gesture`, :50053). Thư mục `workers/` bỏ khỏi repo này; mock Go chuyển sang `internal/mockai`. AI task vẫn là `gesture_recognition`. |
| 9 | (2026-11-06) `video_effects` **chỉ tạo leg tcore**, giống `change_background`. `legs` mặc định đổi thành `tcore` (cả `video.services` lẫn config cũ); `legs: both` vẫn giữ cho dịch vụ khai báo thêm. ctrl-result 1 leg giữ cách cũ: session tcore lấy `callbackUrl` từ tAccess, `callbackUrls` (override theo task) từ tCore; `callbackUrls` trong tAccess bị bỏ qua — ghi rõ trong docs cho DCSF. |
| 10 | (2026-11-06) **Chọn AI theo kết nối.** Trước đây registry không biết kết nối gRPC: target chết có 0 stream nên luôn được chọn. Chạy thật với 1 target sống và 1 target chết: 7/8 ANSWER trả 502. Nay: (A) bỏ qua target đang lỗi kết nối khi kiểm tra slot lúc ANSWER và khi chọn worker; (B) mở stream lỗi thì thử target khác của cùng dịch vụ; (C) `/v1/connections` hiện target AI video kèm dịch vụ và task; `/health/ready` có tùy chọn báo dịch vụ mất AI. Bật/tắt bằng `video.ai.failover` (mặc định `true`) và `video.ai.readiness_requires_ai` (mặc định `false`). (D) Dịch vụ có nhiều target nên đặt `video.ai.max_retry` > 0: AI lỗi giữa cuộc gọi thì chọn lại target khác. |
| 11 | (2026-11-07) **Triển khai IMS cho video.** Đối chiếu `../media_ai/config/gateway-ims.yaml`. Tạo [config/gateway-video-ims.yaml](../config/gateway-video-ims.yaml): phần hạ tầng IMS của media_ai + `video.services`; `ai.grpc_target: ''`; `video.ai.stream_timeout_sec: 0`; hàng đợi video 1024; `keepalive_time_sec: 0` (AI Python). Cổng RTP: **chỉ cấp cổng chẵn** như media_ai (`rtp.even_ports`, mặc định `true`, `port_start` phải chẵn → dải cũ chỉ còn một nửa số cổng) **và** bộ nhận video bỏ gói RTCP (metric `media_ai_video_ingress_rtcp_dropped_total`). Giữ quyết định 9: video 1 leg lấy `tAccess.callbackUrl` (media_ai lấy `tCore`) — ghi trong file config. |
| 12 | (2026-11-07) **ANSWER không được treo khi AI không phản hồi.** Chạy thật với target `192.0.2.1` (SYN không trả lời): kết nối gRPC ở CONNECTING, mở stream chờ ~20s (MinConnectTimeout), HTTP server bỏ response vì `WriteTimeout` 10s → DCSF không nhận được gì (HTTP 000 sau 19,8s). Sửa: (A) `video.ai.open_timeout_ms` (đã có, mặc định 5000, trước chỉ dùng cho Engine) làm hạn chờ chung cho bước mở stream ở chế độ registry, gồm mọi lần thử target; chỉ giới hạn bước mở, stream đã mở sống theo session; `0` = không giới hạn. (B) `server.write_timeout_sec` (mặc định 10, trước cố định). `Load` kiểm tra số leg video × `open_timeout_ms` < `write_timeout_sec`. Vấn đề "gateway video vẫn nhận dịch vụ audio khi không có AI audio" chưa xử lý, chờ quyết định. |
| 13 | (2026-11-07) **Gateway chỉ video từ chối dịch vụ audio bằng lỗi.** Trước: `speech_to_text` trả 201, cấp 2 cổng, không bao giờ có kết quả. Nay: khoá `audio.enabled` (mặc định `true`, giữ hành vi cũ cho gateway audio và `gateway-local.yaml` chạy audio không AI); `false` → `speech_to_text`/`realtime_translation` trả **400** `service <tên> is not supported by this gateway (audio.enabled=false)`, không tạo session, log Warn từng cuộc gọi. `audio.enabled: false` bắt buộc `ai.grpc_target: ''` (mặc định trong code là `ai-router:50051`). Mọi `config/gateway-video-*.yaml` và config do `test-video-e2e.sh` sinh đặt `false`. Không tự suy ra từ `ai.grpc_target` rỗng vì sẽ làm hỏng `gateway-local.yaml`. Không dùng 503 (DCSF có thể thử lại mãi). |
| 14 | (2026-11-07) **ctrl-result an toàn hơn.** Rà lại bằng chạy thật (gateway `-race`): (A) ctrl-result ghi `sess.MediaResources` trong khi result pump đọc không khoá → race detector báo `WARNING: DATA RACE` (manager.go Update ↔ coordinator.pumpResults) khi log level info; nay `Session.MediaResources()` / `SetMediaResources()` dùng `atomic.Pointer`. (B) URL trong ctrl-result không được kiểm tra: `ftp://…` vẫn 200 rồi mọi kết quả gửi lỗi; nay `callbackUrl` và giá trị `callbackUrls` phải là http(s) tuyệt đối có host, không `user:password`, ≤ 2048 ký tự (`validCallbackURL` theo media_ai) — sai → 400 `invalid <field>…`, không áp dụng phần nào, log Warn không kèm URL; rỗng giữ nghĩa cũ. Chỉ áp dụng ctrl-result (ANSWER `callbackUrl` chưa kiểm tra). (C) Lỗi gửi kết quả chỉ log Debug → ở level info không thấy; nay Warn `result: callback delivery failing` một lần khi một callback sink bắt đầu lỗi, Info `…recovered` khi gửi lại được. (D) Kết quả trước ctrl-result vẫn đi `callback.url`, không có `mediaResources` — giữ nguyên, ghi docs. Kèm gofmt `session.go`, `http_callback.go` (lệch từ trước). |
| 15 | (2026-11-07) **DELETE: không cấp lại ngay cổng vừa trả.** Rà lại bằng chạy thật (gateway `-race`): DELETE trả 204 trong ~1ms, session/stream AI/decoder/cổng/slot AI về 0 ngay, không mất kết quả, không race, không rò goroutine theo cuộc gọi; 404 cho callId lạ hoặc DELETE lần 2. Vấn đề: cổng trống là stack (LIFO) nên cuộc gọi kế tiếp nhận đúng cổng vừa trả; nguồn RTP cũ (socket không "connected" như MF) vẫn gửi → cuộc gọi mới nhận 100 gói của cuộc gọi cũ trong 2s, đưa vào decoder (171 lỗi giải mã). Sửa: `PortAllocator` thành hàng đợi FIFO + `rtp.port_reuse_delay_ms` (mặc định 5000; 0 = tắt): cổng vừa trả chờ 5s mới cấp lại; cổng đang chờ không tính là trống (`Available`), có `InUse`/`Cooling`, metric `media_ai_rtp_ports_cooling`, `/v1/connections` `per_session_cooling`. Đánh đổi: dải nhỏ + nhiều cuộc gọi ngắn có thể 503 sớm hơn. Chưa làm (bàn, không khuyến nghị lúc này): khoá SSRC/nguồn, drain như media_ai, dọn dẹp theo đối tượng session thay vì ID. Kèm gofmt `monitor.go` (lệch từ trước). |
| 16 | (2026-11-07) **CALL_CTRL gắn với cuộc gọi, dùng lại code media_ai.** Rà lại bằng chạy thật (DCSF giả h2c + TLS): (A) goroutine CALL_CTRL đóng session theo chuỗi callId → DELETE rồi ANSWER lại cùng callId, CALL_CTRL cũ hết hạn đóng nhầm cuộc gọi mới; CALL_CTRL không dừng khi DELETE. Nay ctx CALL_CTRL kế thừa `sess.Ctx` (DELETE huỷ ngay, log Debug) và lỗi chỉ đóng đúng session của nó (`Manager.CloseSession`, so đối tượng). (B) theo media_ai: `https://` dùng TLS + h2 (trước đây h2c qua TCP thường → `unexpected EOF`), tối đa 64 client động; callId escape trong URL ctrl-result (`url.PathEscape`); `callbackUrl` của ANSWER sai → 400 `invalid callbackUrl` (trước đây 201 rồi cuộc gọi bị đóng). Không thử lại CALL_CTRL. |

## 3. Hiện trạng

| Phần | Code | Hạn chế |
| --- | --- | --- |
| Danh sách dịch vụ | `serviceProfiles` cứng, [handler.go](../internal/controlplane/handler.go) | Thêm dịch vụ phải sửa code |
| Dịch vụ dùng AI task nào | Cứng trong [coordinator.go](../internal/coordinator/coordinator.go) `StartVideo`: `gestureOnly := Task == "gesture_recognition"`; `change_background` mở thêm gesture khi `video.gesture.enabled` | Ghép 2 AI vào 1 dịch vụ; cờ gesture áp dụng cho mọi dịch vụ |
| Chọn worker | `WorkerRegistry.SelectVideoTask(task, ...)` theo **AI task** ([worker_registry.go](../internal/ai/worker_registry.go)) | Hai dịch vụ cùng task dùng chung worker |
| Số leg | `SingleLeg: true` cứng cho `change_background` | Mâu thuẫn với test controlplane và E2E đang kỳ vọng 2 leg |
| Admission | Chỉ kiểm tra "có ít nhất 1 worker" | Không biết AI của dịch vụ được yêu cầu còn chỗ hay không |

## 4. Thiết kế

### 4.1 Cấu hình `video.services`

Đặt dưới `video:` (không phải top-level) vì chỉ áp dụng cho dịch vụ video; dịch vụ
audio không khai báo ở đây.

```yaml
video:
  enabled: true
  services:
    change_background:
      enabled: true
      legs: tcore                  # tcore (mặc định) | both
      effect_type: bg_replace
      ai:
        task: video_segment        # video_segment | gesture_recognition
        targets: ['10.0.0.11:50052', '10.0.0.12:50052']
        max_streams: 8             # mỗi target; 0 = video.ai.max_active_streams
    video_effects:
      enabled: true
      legs: tcore
      ai:
        task: gesture_recognition
        targets: ['10.0.0.21:50053']
        max_streams: 4
```

| Field | Bắt buộc | Mặc định | Ý nghĩa |
| --- | --- | --- | --- |
| `enabled` | không | `true` | `false`: ANSWER trả 200 `{}`, không tạo session (như dịch vụ chưa hỗ trợ) |
| `legs` | không | `tcore` | `tcore`: chỉ tạo leg tcore, kết quả về `callbackUrl` tAccess của ctrl-result; `both`: tcore + taccess |
| `effect_type` | không | `video.default_effect_type` | Chỉ dùng khi `ai.task = video_segment` |
| `ai` | có | — | **Một object**, không phải list → không khai báo được 2 AI cho 1 dịch vụ |
| `ai.task` | có | — | `video_segment` hoặc `gesture_recognition` (giao thức VideoStream/Segment) |
| `ai.targets` | có | — | ≥ 1 địa chỉ `host:port`; nhiều target = nhiều instance của **cùng** AI service, chọn theo tải |
| `ai.max_streams` | không | `0` | Giới hạn stream đồng thời mỗi target; `0` kế thừa `video.ai.max_active_streams` |

Thông số model vẫn lấy từ cấu hình chung như hiện tại: `video.infer_*`, `video.jpeg_quality`
cho segmentation; `video.gesture.infer_fps/max_dimension/max_hands/frame_queue_size` cho gesture.

Validate khi `Load` (sai thì gateway không khởi động, báo lỗi rõ):

- Có `video.services` ⇒ `video.enabled = true`.
- Không kết hợp với `video.ai.workers`, `video.ai.grpc_target`, `video.ai.engine_target`,
  `video.gesture.enabled: true` — một nguồn cấu hình duy nhất.
- Tên dịch vụ chữ thường `[a-z0-9_]`, không trùng dịch vụ audio (`speech_to_text`, `realtime_translation`).
- `legs ∈ {tcore, both}`; `ai.task` hợp lệ; `effect_type` chỉ đi với `video_segment`.
- `ai.targets` ≥ 1, không trùng trong cùng một dịch vụ; `ai.max_streams ≥ 0`.
- Có dịch vụ dùng `gesture_recognition` ⇒ validate các giới hạn `video.gesture.*` như hiện tại.

Bật/tắt: có `video.services` → chế độ danh mục; không có → chế độ config cũ (§4.2).
Từng dịch vụ bật/tắt bằng `enabled`. Nguồn config như hiện tại: YAML qua `--config`
hoặc env `GATEWAY_CONFIG`, k8s `deploy/k8s/configmap.yaml` (mount `/etc/gateway`).

### 4.2 Chế độ config cũ (không có `video.services`)

Gateway tự sinh danh mục tương đương, theo quy tắc 1:1:

| Dịch vụ | Sinh từ | legs | AI |
| --- | --- | --- | --- |
| `change_background` | luôn có khi `video.enabled` | `tcore` (giữ hành vi hiện tại) | `video_segment`, worker từ `video.ai.workers` / `grpc_target` / Engine |
| `video_effects` | chỉ khi `video.gesture.enabled` | `tcore` | `gesture_recognition`, như trên |

Khác biệt duy nhất so với hiện tại: `change_background` **không mở gesture** nữa, kể cả
khi `video.gesture.enabled: true`. Worker cấu hình theo kiểu cũ đăng ký không gắn
dịch vụ (dùng được cho mọi dịch vụ có cùng task), như hiện tại.

### 4.3 Luồng xử lý

```mermaid
flowchart LR
  A[NOTIFY ANSWER<br/>selectedService] --> B{Danh mục video<br/>có dịch vụ?}
  B -- không / enabled=false --> Z[200 {} không tạo session]
  B -- có --> C{Admission:<br/>AI của dịch vụ còn chỗ?}
  C -- không --> Y[503 no AI worker<br/>for service]
  C -- có --> D[Tạo session theo legs<br/>VideoAITask = ai.task]
  D --> E[coordinator.StartVideo<br/>chỉ mở 1 stream theo VideoAITask]
  E --> F[VideoRoutingDialer<br/>SelectVideoTask service, task]
  F --> G[Worker của đúng dịch vụ]
```

- `session.SessionConfig` thêm `VideoAITask` (controlplane gán từ danh mục). Coordinator
  quyết định mở stream nào dựa trên `VideoAITask`, không dựa vào tên dịch vụ hay cờ
  gesture chung:
  - `video_segment`: chỉ segmentation (effect từ dịch vụ), pipeline không sinh frame gesture.
  - `gesture_recognition`: chỉ gesture (`DisableSegmentation`), như `gestureOnly` hiện tại.
- Engine (chế độ config cũ): source chỉ chứa đúng task của dịch vụ (`Segmentation` hoặc `Gesture`).
- `VideoStreamConfig.SelectedService` (đã có) được truyền vào registry để chọn worker.

### 4.4 Registry và chọn worker

- `WorkerInfo` thêm `Services []string`. Worker từ danh mục: `Services = [<dịch vụ>]`,
  ID `video-service:<dịch vụ>:<target>`. Worker từ config cũ: `Services` rỗng.
- `SelectVideoTask(service, task, format, protocol)`: worker hợp lệ khi `Services` rỗng
  hoặc chứa `service`, cộng các điều kiện hiện có (task, media type, format, protocol,
  còn slot); chọn tải thấp nhất. Ở chế độ danh mục mọi worker đều gắn đúng một dịch
  vụ nên hai dịch vụ không bao giờ dùng chung slot.
- Hai dịch vụ trỏ cùng một địa chỉ (ví dụ một `mock-ai-worker` trong lab): **cho phép**;
  dùng chung kết nối gRPC (pool theo địa chỉ) nhưng giới hạn stream tách riêng.
- Watch reconnect và `ai.reconnect_max_backoff_ms` tự áp dụng cho mọi target.
- Chọn theo kết nối (quyết định 10), chỉ khi chọn AI qua registry (`video.services`,
  `video.ai.workers`, `video.ai.grpc_target`), không áp dụng Engine:

  | Khoá | Mặc định | Ý nghĩa |
  | --- | --- | --- |
  | `video.ai.failover` | `true` | Bỏ qua target có kết nối gRPC đang `TRANSIENT_FAILURE`/`SHUTDOWN` (`SharedConnPool.Reachable`, gắn vào registry bằng `SetReachable`); target `IDLE`/`CONNECTING`/`READY` vẫn được chọn. Mở stream lỗi thì trả slot và thử target khác của cùng dịch vụ (`VideoRoutingDialer.Failover`), mỗi target tối đa 1 lần; ctx bị huỷ thì dừng. `false` = hành vi cũ |
  | `video.ai.readiness_requires_ai` | `false` | `/health/ready` trả 503 `{"reason":"video_service_unavailable","services":[...]}` khi một dịch vụ đang bật không còn target kết nối được. Để tắt mặc định vì not_ready làm k8s ngừng chuyển traffic vào gateway, kéo theo cả dịch vụ còn sống. Bật mà không có target qua registry (Engine, không có target) → `Load` báo lỗi |

  Target chết sống lại: gRPC tự nối lại theo backoff (giới hạn bằng `ai.reconnect_max_backoff_ms`;
  `0` = mặc định gRPC, tối đa 120s), khi `READY` thì target được chọn lại. Giữa cuộc gọi:
  stream lỗi và `video.ai.max_retry` > 0 → chọn lại qua cùng cơ chế (bỏ qua target chết).

### 4.5 Admission theo dịch vụ

`WorkerRegistry.HasVideoCapacity(service, task)` (chỉ đọc, không giữ slot). Trong
`handleAnswer`, sau khi tra danh mục: không có worker phù hợp còn slot và kết nối dùng được
(quyết định 10) → **503**
`{"error":"no AI worker available for service <tên>","retry_after_ms":3000}`. Slot vẫn được giữ lúc dial như hiện tại; nếu
bị tranh mất giữa kiểm tra và dial, ANSWER rollback 502 như hành vi hiện có.
Triển khai: kiểm tra nằm trong `handleAnswer`, bật bằng `Server.SetVideoWorkerRegistry`
(không đi qua `AdmissionController`, nên readiness `no_ai_worker` của audio không đổi).
`main.go` chỉ gắn khi video dial qua registry (`video.services` hoặc `video.ai.workers` /
`grpc_target`); chế độ Engine và `NullVideoDialer` không kiểm tra.

### 4.6 Không đổi

- Giao thức worker (proto, VideoStream/Segment, VideoAIService/Process).
- Callback: event `video.mask.result` / `video.gesture.result`, lane dispatcher theo task.
- ctrl-result `callbackUrls`: vẫn nhận key `video_segment`, `gesture_recognition`; key
  không thuộc AI của dịch vụ được bỏ qua. Dịch vụ 1 leg: session tcore lấy `callbackUrl`
  từ tAccess nhưng `callbackUrls` từ tCore (quyết định 9).
- Dịch vụ audio và `serviceProfiles` audio.

## 5. Thay đổi hành vi

| Tình huống | Hiện tại | Sau thay đổi |
| --- | --- | --- |
| `change_background`, `video.gesture.enabled: true` | Mở segmentation + gesture | Chỉ segmentation |
| Dịch vụ gesture | `gesture_recognition`, chỉ gesture | Đổi tên thành `video_effects`; `gesture_recognition` → ACK 200 `{}` (trừ khi khai báo trong `video.services`) |
| Dịch vụ video không có trong danh mục / `enabled: false` | Dịch vụ lạ: 200 `{}` | 200 `{}` |
| AI của dịch vụ hết slot | Lỗi lúc dial → 502 | 503 trước khi tạo session |
| Thêm dịch vụ video mới cùng giao thức | Sửa code | Chỉ sửa `video.services` |
| `speech_to_text` + `bearerCapability: VIDEO` | Mở thêm video segmentation | **Cần duyệt** (§8) |

## 6. File thay đổi

| File | Nội dung |
| --- | --- |
| `internal/config/video_services.go` (mới) | `VideoServiceSection`, validate, `ToVideoServices()`, sinh danh mục từ config cũ, worker theo dịch vụ |
| `internal/config/config.go` | `VideoSection.Services`, gọi validate trong `Load`, truyền danh mục sang `ServerConfig` |
| `internal/ai/worker_info.go`, `worker_registry.go`, `video_grpc_dialer.go` | `Services`, `SelectVideoTask(service, …)`, `HasVideoCapacity` |
| `internal/session` | `SessionConfig.VideoAITask` |
| `internal/controlplane/handler.go`, `server.go` | Tra danh mục cho dịch vụ video, `legs`, `VideoAITask`, admission theo dịch vụ |
| `internal/coordinator/coordinator.go` | Mở stream theo `VideoAITask` (direct và Engine) |
| `cmd/media-ai-gateway/main.go` | Đăng ký worker theo dịch vụ, watch, gắn registry vào admission khi có video |
| `internal/config/video_workers.go` | `ToVideoWorkers()` trả worker theo dịch vụ khi có `video.services` |
| `internal/mockai/` (chuyển từ `workers/mockai/`) | Mock AI Go cho `cmd/mock-ai-worker` và test; `workers/` (AI Python) bỏ khỏi repo, AI thật ở repo `mediapipe`, `gesture` |
| `scripts/generate-video-proto.sh`, `.gitignore`, `mf/README.md` | Chép proto sang repo AI nếu có (`MEDIAPIPE_DIR`, `GESTURE_DIR`); bỏ đường dẫn `workers/` |
| `config/gateway-video-services.yaml` (mới) | Mẫu danh mục 2 dịch vụ |
| `scripts/test-video-e2e.sh`, `scripts/test-video-engine-e2e.sh`, `scripts/test-video-external-e2e.sh` | Bỏ chế độ ghép segmentation + gesture trong 1 cuộc gọi (`both`); gesture chỉ đi với `--gesture-only`; direct-worker sinh `video.services`; làm việc theo danh sách leg của dịch vụ (`change_background`: gateway tcore → MF tAccess) |
| `mf/include/renderer.hpp`, `mf/src/main.cpp`, `mf/src/callback_server.cpp` | Option `mf_cpp --legs 1\|2` (mặc định 2): số leg phải render trước `/finalize`; với `--legs 1` MF chấp nhận session `<call>-tcore` trên leg nhận kết quả (ctrl-result single-leg gửi kết quả tcore về callback tAccess) |
| `scripts/validate-gesture-output.py` | (quyết định 9) Kiểm tra theo các leg MF có output (`result.output_videos`), không cố định tCore + tAccess |
| Docs | `luong-xu-ly-video-realtime-rtpgw-ai-mf.md` (§5.1, §16 và ví dụ ghép 2 task), `video-testing.md`, `dcas-dcsf-signaling.md` (bảng `selectedService` video, ctrl-result 1 leg) |
| Test | config, ai, controlplane (gồm sửa 2 test đang fail về số leg), coordinator |

## 7. Kế hoạch test (không giả lập, đọc log)

| Mức | Nội dung | Chạy ở |
| --- | --- | --- |
| Unit config | Validate từng luật §4.1; config cũ sinh đúng danh mục §4.2 | Windows |
| Unit ai | 2 gRPC server thật trên loopback, mỗi server là AI của một dịch vụ: dial dịch vụ A luôn tới server A; hết slot → `HasVideoCapacity` false | Windows |
| Unit controlplane (coordinator giả) | Số leg theo `legs`; `VideoAITask` đúng; `enabled: false` → 200 `{}`; hết slot → 503 | Windows |
| Unit coordinator | `change_background` chỉ mở segmentation; `video_effects` chỉ gesture; Engine source đúng 1 task | Linux/WSL (cần decoder H.264 thật) |
| Chạy thật | Gateway + 2 `mock-ai-worker` (mỗi dịch vụ một port) + `mock-video-rtp-sender`; đọc log `ai: dialing video worker` (`worker_addr`) và callback từng dịch vụ | Linux/WSL |
| E2E | `test-video-e2e.sh` segmentation-only, `--gesture-only`; `test-video-engine-e2e.sh` segmentation, gesture | Linux/WSL (Go + CGO + FFmpeg + CMake + nghttp2) |

Trên máy dev Windows: decoder chỉ có bản CGO, ANSWER video tạo decoder ngay
(`RegisterSession`), nên mọi bước có session video thật phải chạy trên Linux/WSL.
WSL Ubuntu đã có FFmpeg dev và g++, **chưa có Go**.

## 8. Điểm cần duyệt

1. **Dịch vụ audio có `bearerCapability: VIDEO`** (test `TestNotifyEvent_Answer_VideoOnly_BearerCapabilityStillWorks`):
   hiện mở thêm video segmentation, tức một dịch vụ dùng 2 AI. Đề xuất: theo quy tắc 1:1,
   chỉ dịch vụ có trong danh mục video mới mở video; dịch vụ audio không mở video nữa.
2. **Môi trường test Linux:** cài Go vào WSL Ubuntu để chạy test coordinator, chạy thật và E2E
   (hoặc chạy trên lab Linux có sẵn).
3. Các mặc định đã chọn, đổi được nếu cần: khoá `video.services` (không top-level);
   ~~`legs` mặc định `both`, riêng `change_background` ở config cũ là `tcore`~~ — đổi theo
   quyết định 9: `legs` mặc định `tcore` cho mọi dịch vụ; cho phép hai dịch vụ trỏ cùng một địa chỉ.

## 9. Kết quả kiểm chứng (2026-11-06)

| Mức | Kết quả |
| --- | --- |
| Windows (`go vet` + test, decoder stub) | `go vet ./...` sạch; ai, config, controlplane, session, result, jitter, rawrtp, mockai PASS. Hai test controlplane fail từ trước (số leg của `change_background`) nay PASS |
| WSL (Go 1.26.4, CGO + FFmpeg thật, `go test ./...`) | Mọi package PASS, gồm coordinator (test 1:1 mới). Chỉ `cmd/mock-video-rtp-sender` fail và 3 test coordinator skip vì thiếu chương trình `ffmpeg` (môi trường, giống trước khi sửa) |
| Chạy thật trên WSL (gateway CGO + 2 `mock-ai-worker`, `gateway-video-services.yaml`, chưa có RTP) | `change_background` → 201, chỉ `tcore_video_rtp_port`, dial `127.0.0.1:50052`; `gesture_recognition` → 201, 2 leg, dial `127.0.0.1:50053`; cuộc gesture thứ 3 (hết 4 slot) → 503 `no AI worker available for service gesture_recognition`; dịch vụ tắt và dịch vụ lạ → 200 `{}`; `speech_to_text` + `bearerCapability: VIDEO` → chỉ port audio. Log `video service configured`, `video AI worker registered` (có `services`), `ai: dialing video worker` (có `service`, `worker_addr`) đúng từng dịch vụ |
| Cú pháp script E2E | `bash -n` cho 3 script, `py_compile` cho đoạn Python nhúng và `validate-gesture-output.py`: OK |
| WSL sau khi cài `ffmpeg` 8.0.1, `libnghttp2-dev`, `nlohmann-json3-dev` | `go test ./...` PASS toàn bộ (gồm `mock-video-rtp-sender`; 3 test video coordinator chạy thật, không skip). MF C++ build OK, CTest `gesture_renderer` PASS, `--legs 1\|2` có trong help, giá trị sai bị từ chối |
| E2E direct-worker (`video.services` sinh tự động) | `change_background` mock, 1 leg, `--output-video`: **PASS** (381 packet, 25 mask, MP4 tAccess). `--gesture-only --require-gesture` mock, 2 leg: **PASS** (456 packet, 45 gesture/leg, pixel gesture OK). Gesture trên `wave_hand.mp4` với `ffmpeg` 8.0.1: mọi bước gateway/AI/callback/cleanup đạt, chỉ fail kiểm tra số frame MP4 (90 thay vì 91, thiếu frame end-marker); code `HEAD` 296d188 fail y hệt → do `ffmpeg` 8.0.1, không do thay đổi này. Với **`ffmpeg` 7.1.1** (CLI build từ nguồn chính thức, thư viện CGO vẫn FFmpeg 8): **PASS** (304 packet, 45 gesture/leg, MP4 91 frame/leg, pixel gesture OK); chạy lại seg 1 leg, Engine seg, Engine gesture và test Go dùng `ffmpeg`: PASS |
| E2E qua AI Engine (config cũ) | Segmentation (`change_background`, 1 leg): **PASS** (228 packet, 15 mask). `--gesture-only --require-gesture` (2 leg): **PASS** (456 packet, 45 gesture/leg) |
| Từ chối chế độ ghép | `--gesture-mode mock` không kèm `--gesture-only` → FAIL ngay ở preflight với thông báo 1 dịch vụ ↔ 1 AI |
| Sau khi tách AI repo + đổi tên `video_effects` (2026-11-06) | Windows vet/test PASS; WSL `go vet`/`go test ./...` PASS (gồm `internal/mockai`); MF CTest PASS; 6 E2E mock PASS (service in kết quả là `video_effects`, emoji lấy từ `../gesture/assets/emoji`) |
| E2E với **AI thật** (CPU, WSL, `mediapipe` 0.10.35) | `change_background` ↔ repo mediapipe :50052: **PASS** (305 packet, 30 mask; log mediapipe `inference session=…-tcore … latency≈5–50ms`). `video_effects` ↔ repo gesture :50053, `--require-gesture`, `wave_hand.mp4` 10s: **PASS** (1018 packet, 150 gesture/leg; log gesture `stream_open` cho tcore+taccess, `gesture_event … gesture=Wave`) |
| Quyết định 10 — chạy thật trước khi sửa (commit 463927b, `change_background` 2 target: 52052 sống, 52054 không có tiến trình) | 8 ANSWER: 1×201 (52052), **7×502** (52054, `connection refused`) dù log gateway đã ghi `grpc connect failed` cho 52054; `video_effects` chỉ 1 target chết → 502 thay vì 503; `/health/ready` 200; `/v1/connections` `ai_workers: null` |
| Quyết định 10 — sau khi sửa | Windows vet/test PASS. WSL: gofmt 11 file sửa sạch, `go vet`/`go test ./...` PASS, `-race` ai/controlplane/config PASS (test mới chạy lặp 5 lần), 2 E2E mock PASS. Chạy thật 4 pha: (1) 52054 và 52053 chết → 6/6 ANSWER `change_background` 201 vào 52052, `video_effects` 503 `retry_after_ms: 3000`, `/v1/connections` hiện 3 target kèm dịch vụ/task/trạng thái; (2) bật 52054 → log `connection restored down_ms=4512`, 4/4 ANSWER mới vào 52054 (tải thấp hơn); (3) `kill -9` 52054 rồi ANSWER ngay → 2/2 vào 52052 (gateway đã thấy lỗi kết nối trước khi ANSWER tới, nên nhánh thử target khác không chạy; nhánh này được kiểm bằng unit test gRPC thật `TestVideoRoutingSkipsDeadTargetRealServers`); (4) `readiness_requires_ai: true` → 503 `video_service_unavailable` `["video_effects"]`, bật 52053 → 200 và ANSWER `video_effects` 201 |
| Quyết định 16 (2026-11-07) | Trước khi sửa (main bd50956): DCSF 200/500+cause/không trả lời xử lý đúng; DELETE + ANSWER lại cùng callId → cuộc gọi mới 404 khi CALL_CTRL cũ hết hạn; `ftp://` → 201 rồi đóng; callId IMS gửi DCSF không escape; `https` → `unexpected EOF`, DCSF TLS nhận 0 request. Sau khi sửa: Windows vet/test PASS; WSL gofmt 5 file, `go vet`/`go test ./...`, `-race` PASS, 2 E2E mock PASS. Chạy thật (`-race`): cuộc gọi mới cùng callId vẫn 200, không Warn; DCSF thấy CALL_CTRL treo bị huỷ 0,5s sau khi nhận (đúng lúc DELETE, hạn chờ 3s); `ftp://` → 400; URL ctrl-result `…/p2uc31@%5BFC00:0DB8::%5D/ctrl-result`, ctrl-result về URL đó 200; `https` (cert tin qua `SSL_CERT_FILE`) → DCSF nhận `HTTP/2.0`, `tls:true`, cuộc gọi 200; race 0 |
| Quyết định 15 (2026-11-07) | Trước khi sửa (main 3369520): cuộc gọi mới nhận lại cổng 49118 sau DELETE 0,3s và nhận 100 gói RTP của cuộc gọi cũ. Sau khi sửa: Windows vet/test PASS (FIFO, thời gian chờ với đồng hồ giả, DELETE→ANSWER nhận cổng khác); WSL gofmt 8 file, `go vet`/`go test ./...`, `-race` PASS, 2 E2E mock PASS. Chạy thật (`-race`, dải 3 cổng chẵn, nguồn RTP cũ gửi tiếp 50 gói/s): DELETE → `open=0 cooling=1`; cuộc gọi mới nhận 49202 (cũ 49200), 0 gói lạ trong 2s; khi 2 cổng còn lại đã dùng → ANSWER thứ 4 503 `service at capacity` (`retry_after_ms: 10000`), `open=2 cooling=1`; sau 5s `cooling=0` và ANSWER nhận lại 49200; race 0 |
| Quyết định 14 (2026-11-07) | Trước khi sửa (main 42667c6, gateway `-race`): `ftp://` → 200 rồi 15–16 kết quả gửi lỗi `http2: unsupported scheme`, log level info không có dòng nào; race detector 1 cảnh báo `MediaResources`. Sau khi sửa: Windows vet/test PASS; WSL gofmt 10 file, `go vet`/`go test ./...`, `-race` session/result/coordinator/controlplane/ai PASS, 2 E2E mock PASS. Chạy thật (`-race`, log info, RTP 18s): `ftp://`, URL có `user:password`, URL tương đối → 400 đúng field, kết quả vẫn đi URL cũ, `send_errors=0`; tắt MF giả ~2s → 9 lỗi gửi, log đúng 1 Warn `callback delivery failing` + 1 Info `recovered`, 0 dòng `sink send error`; race detector 0 cảnh báo |
| Quyết định 13 (2026-11-07) | Windows vet/test PASS (gồm test 5 config video đều tắt audio và nạp được, `gateway-local.yaml` giữ audio). WSL gofmt 22 file, `go vet`/`go test ./...`, `-race` PASS, 2 E2E mock PASS (config E2E có `audio.enabled: false`). Chạy thật: log khởi động `audio services disabled (audio.enabled=false)…`; `speech_to_text` và `realtime_translation` (kể cả `bearerCapability: VIDEO`) → 400 đúng thông báo trong < 1ms, GET → 404, số cổng mở không đổi, log Warn `audio service rejected` cho từng callId; các nhánh khác (BEGIN, JSON sai, event lạ, `fun_calling`, video 201, 409, AI chết 503, CALL_CTRL thành công/lỗi, DELETE) giữ nguyên kết quả |
| Quyết định 12 (2026-11-07) | Rà lại ANSWER bằng chạy thật trước khi sửa: BEGIN 200, JSON sai 400, event lạ 400, `fun_calling` 200 `{}`, `change_background` 201 (cổng chẵn), trùng callId 409, `video_effects` AI chết 503, `speech_to_text` 201 dù không có AI audio, callId lấy từ URL, CALL_CTRL tới DCSF giả qua HTTP/2 đúng path, CALL_CTRL lỗi → đóng session và trả cổng, DELETE → 204 và trả hết cổng. Sau khi sửa: Windows vet/test PASS; WSL gofmt 20 file, `go vet`/`go test ./...`, `-race` PASS, 2 E2E mock PASS; test gRPC thật (máy nhận TCP nhưng im lặng) cắt ở 300ms (`waiting for connections to become ready`). Chạy thật `192.0.2.1`: `open_timeout_ms` 5000 → **502 sau 5,0s** (`timed out after 5s`), không còn session, cổng đã trả; `0` → như cũ (HTTP 000 sau 19,8s) |
| Quyết định 11 (2026-11-07) | Windows vet/test PASS. WSL: gofmt 17 file sửa sạch, `go vet`/`go test ./...` PASS, `-race` controlplane/rawrtp/config/ai PASS, 2 E2E mock PASS (cổng ANSWER chẵn: 36714, 39416). Chạy thật: `gateway-video-ims.yaml` nạp được (2 dịch vụ, 3 target, `failover: true`, không nối AI audio); `port_start` lẻ → không khởi động, báo `rtp.port_start must be even…`; 6 ANSWER → cổng 45018…45008 đều chẵn; 3 gói RTCP (SR/RR/BYE) vào cổng video → `ingress_rtcp_dropped_total=3`, `parse_errors` giữ 0. **`stream_timeout_sec`**: đặt 4 → AI video dừng ở giây thứ 4 (`ai_streams_active` 1→0, `frames_sent` đứng ở 19) trong khi RTP vẫn tới (`rtp_parsed` 134→480); đặt 0 → AI chạy hết 14s (`frames_sent` 70). Mặc định 300 nghĩa là cuộc gọi > 5 phút mất AI video |
| Sau quyết định 9 (`video_effects` chỉ leg tcore, `legs` mặc định `tcore`) | Windows vet/test PASS; WSL `go vet`/`go test ./...` PASS, gồm `TestVideoEffects_CtrlResultSingleLeg` (tcore nhận `callbackUrl` tAccess, resource cả 2 leg, `callbackUrls` lấy từ tCore). 6 E2E mock PASS: `video_effects` direct (`--require-gesture`, kể cả `wave_hand.mp4`) và qua Engine đều chỉ có `tcore_video_rtp_port`, MF `--legs 1`, 45 gesture ở tAccess. AI thật: `change_background` PASS (305 packet, 30 mask); `video_effects` ↔ repo gesture PASS (509 packet, 150 gesture ở tAccess; log gesture đúng 1 `stream_open` cho `…-tcore`, `gesture=Wave`) |

## 10. Ngoài phạm vi (giai đoạn sau)

- Engine trong danh mục (`protocol: video_source`, mỗi dịch vụ một Engine).
- Thông số model riêng theo dịch vụ (FPS, kích thước, `max_hands`).
- `event_type` callback và metric có nhãn `service`.
- ~~Bỏ qua target đang mất kết nối khi chọn worker (dùng trạng thái watch).~~ Đã làm (quyết định 10).
- Dịch vụ cần giao thức AI mới, dịch vụ audio.
