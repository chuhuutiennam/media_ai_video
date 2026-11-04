# MediaPipe Worker — Architecture Analysis

Cập nhật theo code: 2026-09-29 (v1.0.1)

---

## 1. Mô tả Project & Mục tiêu

**MediaPipe Worker** (package `mediapipe_video`) là một **Python microservice** thuộc hệ thống `media_ai_video`. Nó đóng vai trò là AI inference backend chuyên xử lý **video segmentation** theo thời gian thực — tách người (foreground) khỏi nền (background) trong từng video frame.

### Project này dùng để làm gì?

Worker nhận video frame liên tục qua **gRPC bidirectional streaming**, chạy model ML để tạo **segmentation mask**, rồi trả kết quả về cho gateway để áp dụng hiệu ứng:

- `bg_blur` — Làm mờ nền
- `bg_replace` — Thay nền bằng ảnh khác
- `bg_remove` — Xóa nền hoàn toàn (trong suốt)

Worker chỉ tạo mask; việc áp dụng hiệu ứng do Gateway thực hiện. Danh sách `effect_type` hợp lệ cấu hình được (`segmentation.allowed_effect_types`).

### Mục tiêu

1. **Tách biệt AI inference** khỏi gateway chính (Go) để scale độc lập
2. **Low-latency per-frame** — mỗi frame được xử lý và trả về ngay, không batch
3. **Hỗ trợ concurrent streams** — nhiều phiên video song song (`server.max_streams`, mặc định 8)
4. **Pluggable model** — đổi TFLite model mà không sửa code (`--model` / `mediapipe.model`)
5. **GPU/CPU switchable** — `delegate` = `gpu`, `cpu` hoặc `auto` (thử GPU, lỗi thì fallback CPU)
6. **Scale theo GPU** — mỗi process gắn một GPU (`gpu_device_index` → `CUDA_VISIBLE_DEVICES`); chạy N process cho N GPU, cân bằng tải bằng nginx (xem `docs/deployment.md`)
7. **Quan sát được** — Prometheus metrics và log JSON (xem `docs/metrics.md`)

---

## 2. Kiến trúc Tổng thể

MediaPipe Worker là **một node trong pipeline xử lý video** của hệ thống lớn hơn. Các thành phần chính:

| Lớp | Công nghệ | Vai trò |
|---|---|---|
| **Gateway** | Go | Tiếp nhận RTP stream, decode, resize, JPEG-encode, gửi qua gRPC |
| **MediaPipe Worker** | Python | Nhận JPEG frame, chạy inference, trả RLE mask |
| **TFLite Model** | `selfie_segmenter.tflite` | ML model chạy bên trong Worker |
| **Proto Contract** | `video.proto` | Định nghĩa giao tiếp giữa Gateway ↔ Worker |

### Vị trí trong hệ thống

```
Client (WebRTC/RTP)
        ↓
   Gateway (Go)
   ├── FPS sampling
   ├── Decode + Resize + JPEG encode
   └── gRPC stream → MediaPipe Worker (Python)
                         ├── Producer thread: đọc stream + JPEG decode (cv2)
                         ├── ImageSegmenter (LIVE_STREAM)
                         ├── TFLite model (GPU/CPU)
                         └── RLE mask → trả về Gateway
```

### Đặc điểm kiến trúc

- **Process boundary rõ ràng**: Gateway (Go) và Worker (Python) chạy trong 2 process riêng biệt, giao tiếp qua gRPC (mặc định `127.0.0.1:50052`)
- **Stateless per-stream**: Mỗi gRPC stream tạo một `ImageSegmenter`, `SimpleQueue`, `decode_queue` riêng; không chia sẻ state giữa các stream, ngoại trừ semaphore giới hạn GPU segmenter và các metric toàn cục
- **Contract-first**: `video.proto` là nguồn sự thật. Nguồn chỉnh sửa là `internal/ai/proto/video.proto` của gateway; script sinh Go binding đồng bộ snapshot `src/proto/video.proto` cho worker độc lập. Python checkout ưu tiên internal, fallback snapshot/resource đóng gói; không sửa snapshot độc lập
- **Producer–consumer trong từng stream**: một thread đọc + decode JPEG, một thread điều khiển GPU, nhờ đó decode frame kế tiếp chồng lấp với inference frame hiện tại
- **Một response cho mỗi frame, trả ngay**: consumer xử lý tuần tự từng frame (submit → chờ callback → RLE → yield), tối đa một frame trên GPU (LIVE_STREAM sẽ drop frame nếu submit chồng)
- **Async callback model**: MediaPipe LIVE_STREAM trả kết quả qua callback bất đồng bộ; Worker dùng `SimpleQueue` để đồng bộ về thread xử lý stream
- **Cấu hình 3 lớp**: mặc định trong dataclass < `etc/config.yaml` < CLI flag

---

## 3. Các Module Chính

Package `src/mediapipe_video/`:

| Module | Vai trò |
|---|---|
| `server.py` | Entry point (`main()`), CLI, gộp config, auto-detect delegate, đặt `CUDA_VISIBLE_DEVICES`, dựng gRPC server |
| `segmenter.py` | `Worker`: vòng đời `ImageSegmenter`, handler `Segment`, producer thread, `_abort()` |
| `codec.py` | `threshold_mask()`, `encode_rle()` |
| `config.py` | Dataclass config + `load_config()` (YAML) |
| `contract.py` | Compile `video.proto` lúc import → `pb` |
| `log.py` | JSON log formatter, rotate file theo ngày |
| `metrics.py` | Prometheus metrics, HTTP `/metrics`, log snapshot định kỳ |

### `server.main()` — Entry Point
Parse CLI, load config, setup logging và metrics, khởi tạo `Worker` (xử lý `delegate=auto`), dựng gRPC server, bind port, block đợi kết nối. Xử lý Ctrl+C bằng graceful stop.

### `contract.load_contract()` — Proto Loader
Compile `video.proto` tại runtime bằng `grpc_tools.protoc` vào thư mục tạm → import `video_pb2`. Kết quả gán vào biến module `pb`, được các module khác import.

### `Worker` class — Core Logic
Là trái tim của worker. Quản lý vòng đời của `ImageSegmenter` và xử lý từng gRPC stream.

| Method | Mô tả |
|---|---|
| `__init__(mp_config, seg_config)` | Đọc model, chọn delegate (`gpu`/`cpu`), tạo semaphore giới hạn segmenter, probe khởi động để vạch lỗi sớm |
| `create_segmenter(callback)` | Tạo `ImageSegmenter` mới với LIVE_STREAM mode và callback cụ thể |
| `foreground_mask(result, threshold)` | Lấy confidence plane từ MediaPipe result, ngưỡng thành binary mask |
| `wait_for_result(queue, ts, ctx)` | Block đợi callback, có timeout, kiểm tra cancel, bỏ callback cũ (đếm frame drop) |
| `Segment(requests, context)` | gRPC handler; bọc metric `ACTIVE_STREAMS` / `STREAMS_TOTAL`, ủy quyền cho `_segment()` |
| `_segment(requests, context)` | **Handler thật** — validate config, lấy semaphore, chạy producer thread, vòng consumer điều khiển GPU và yield response |

### `_abort(context, code, message)`
Tăng `stream_errors_total{reason}` rồi `context.abort()`. Mọi điểm abort đều đi qua hàm này để metric đầy đủ.

### `codec.encode_rle(mask)` — RLE Encoder
Chuyển binary numpy mask (HxW bool) thành danh sách số nguyên row-major RLE. Background = 0, foreground = 1; run đầu tiên luôn là background (có thể bằng 0).

### `codec.threshold_mask(probabilities, threshold)` — Mask Converter
Chuyển confidence float array (HxW hoặc HxWx1) thành boolean mask bằng cách so sánh với ngưỡng.

---

## 4. Luồng Hoạt động: Start → Shutdown

### Startup

1. `main()` parse CLI args (`--config`, `--addr`, `--model`, `--foreground-index`, `--gpu-device-index`, `--max-streams`, `--delegate`, `--result-timeout`, `--log-level`)
2. Load YAML (`--config` hoặc `etc/config.yaml` nếu tồn tại), CLI flag ghi đè giá trị YAML
3. `setup_logging()`; start Prometheus HTTP server (nếu `metrics.enabled`) và periodic metrics logger (nếu `log_interval_secs > 0`)
4. Validate: có `model` và file tồn tại, `max_streams > 0`, `result_timeout_secs` hữu hạn và dương
5. Nếu có `gpu_device_index` → `CUDA_VISIBLE_DEVICES` (trước khi tạo `Worker`)
6. `contract.py` đã compile `video.proto` lúc import module → `pb`
7. Tạo `Worker`; probe bằng cách tạo và đóng ngay 1 `ImageSegmenter`
   - `delegate=auto`: thử `gpu`, gặp `ValueError`/`RuntimeError` thì fallback `cpu`; cpu cũng lỗi thì thoát
   - `delegate` cố định mà lỗi thì thoát với thông báo
8. Cảnh báo nếu `delegate=gpu` mà `max_concurrent_segmenters=0`
9. `grpc.server(ThreadPoolExecutor(max_workers=max_streams), maximum_concurrent_rpcs=max_streams)`
10. Register handler `/video.VideoStream/Segment` → `worker.Segment`, `add_insecure_port(addr)`, `server.start()`
11. Log `server ready addr=… model=… mediapipe=… delegate=…` (JSON, trường `message`)
12. `server.wait_for_termination()` — block mãi

### Per-Stream (mỗi gRPC Segment call)

1. `Segment()` tăng `ACTIVE_STREAMS`, chuyển sang `_segment()`
2. Đọc message đầu tiên — phải là `config` có `session_id`, `stream_id`, không phải EOS; validate `effect_type`, `threshold` (0 → default), dimensions
3. Nếu bật giới hạn: lấy semaphore GPU (timeout `result_timeout`), hết chỗ → `RESOURCE_EXHAUSTED`
4. Tạo `results` (`SimpleQueue`), `decode_queue` (`Queue(maxsize=2)`), `stop_event`
5. Khởi động **producer thread**: đọc `requests`, validate từng message, decode JPEG bằng cv2 → RGB → `mp.Image`, đưa vào `decode_queue`; lỗi từng frame (encoding, timestamp, JPEG) được gắn vào item, không dừng stream
6. `create_segmenter(on_result)` — tạo `ImageSegmenter` dành riêng cho stream này
7. **Vòng consumer** (thread gRPC), mỗi lượt lấy một item từ `decode_queue` và xử lý trọn vẹn:
   - Decode OK: `segment_async()` → `wait_for_result()` → `encode_rle()`; decode lỗi hoặc callback lỗi: `mask.status="error"`
   - Cập nhật metric/log → `yield VideoResponse` ngay (đúng một response cho mỗi frame, không chờ frame kế tiếp)
8. Sentinel `None` (hết stream, EOS hoặc half-close) hoặc frame mang `is_eos=true` → thoát vòng lặp; không còn response nào chờ gửi
9. `finally`: `stop_event.set()`, xả `decode_queue`, `join` producer, thoát `with` (`ImageSegmenter.__exit__()`), release semaphore, `STREAMS_TOTAL++`, `ACTIVE_STREAMS--`

### Shutdown

- `KeyboardInterrupt` (Ctrl+C) → `server.stop(shutdown_grace_secs).wait()` (mặc định 2 giây cho stream hiện tại kết thúc). Không có handler riêng cho SIGTERM.

---

## 5. Thư viện Quan trọng & Dependency

### Thư viện chính

| Thư viện | Phần dùng | Vai trò |
|---|---|---|
| `mediapipe==0.10.35` | `mp.tasks.vision.ImageSegmenter`, `mp.Image` | ML inference engine, quản lý model TFLite |
| `grpcio==1.81.1` | `grpc.server`, `grpc.stream_stream_rpc_method_handler` | gRPC server runtime |
| `grpcio-tools==1.81.1` | `grpc_tools.protoc` | Compile `.proto` at runtime |
| `protobuf==6.33.6` | `video_pb2` (generated) | Serialize/deserialize protobuf messages |
| `numpy==2.4.6` | `np.frombuffer`, `np.flatnonzero`, `np.diff` | Buffer view, mask manipulation, RLE |
| `opencv-contrib-python>=4.5` | `cv2.imdecode`, `cv2.cvtColor` | JPEG decode, BGR → RGB |
| `PyYAML>=6.0` | `yaml.safe_load` | Đọc `config.yaml` |
| `prometheus-client>=0.20` | `Counter`, `Gauge`, `Histogram`, `start_http_server` | Metrics |
| `ruamel.yaml>=0.19` | (khai báo dependency, code chưa import) | — |

### Dependency giữa các module

```
server.main()
  ├── config.load_config()
  ├── log.setup_logging()
  ├── metrics (server + periodic logger)
  ├── contract.pb                → grpcio-tools, protobuf
  └── segmenter.Worker
       ├── create_segmenter()    → mediapipe
       ├── foreground_mask()     → codec.threshold_mask → numpy
       ├── wait_for_result()     → stdlib (queue, time), grpc, metrics
       └── Segment() / _segment()
            ├── producer thread  → opencv (cv2), numpy, mediapipe (mp.Image)
            ├── segment_async    → mediapipe
            ├── encode_rle()     → numpy
            └── metrics          → prometheus-client
```

### External dependencies (ngoài process)

- **`video.proto`** — nguồn chính `internal/ai/proto/video.proto`; `src/proto/video.proto` là snapshot phân phối. Build wheel/sdist ưu tiên internal nếu có, fallback snapshot cho worker độc lập
- **TFLite model file** — `--model` hoặc `mediapipe.model` (ví dụ: `checkpoint/selfie_segmenter.tflite`)
- **GPU driver** — cần EGL/OpenGL ES nếu chạy với `delegate=gpu` (hoặc `auto` muốn dùng GPU); trên RHEL 8 headless cần thêm thư viện X11 (xem `docs/deployment.md`)
- **File config** — `etc/config.yaml` (tùy chọn)

---

## 6. Architecture Diagram

```mermaid
graph TB
    subgraph Client["Client Layer"]
        C[WebRTC / RTP Client]
    end

    subgraph Gateway["Gateway Layer (Go)"]
        GW[media-ai-gateway]
        GW --> |FPS sample| FPS[FPS Sampler]
        GW --> |decode+resize| ENC[JPEG Encoder]
    end

    subgraph Worker["AI Worker Layer (Python: mediapipe_video)"]
        SRV[server.py\ngRPC server]
        WK[segmenter.py\nWorker]
        PROD[Producer thread\ncv2 JPEG decode]
        SEG[MediaPipe ImageSegmenter]
        TFL[(selfie_segmenter.tflite)]
        SRV --> WK
        WK --> PROD
        WK --> SEG
        SEG --> TFL
    end

    subgraph Obs["Observability"]
        PROM[Prometheus\n:9101 /metrics]
        LOG[JSON log\nstdout + logs/*.log]
    end

    subgraph Proto["Contract"]
        PB[video.proto]
    end

    C -->|RTP stream| GW
    ENC -->|gRPC VideoRequest\nstream VideoFrame JPEG| SRV
    SRV -->|gRPC VideoResponse\nVideoMask RLE| GW
    GW -->|apply effect\nbg_blur / bg_replace / bg_remove| C
    PB -.->|defines| GW
    PB -.->|compiled at import| WK
    WK -.->|metrics| PROM
    WK -.->|log| LOG

    classDef go fill:#00ADD8,color:#fff
    classDef python fill:#3572A5,color:#fff
    classDef model fill:#F7931E,color:#fff
    classDef proto fill:#6B7280,color:#fff
    classDef obs fill:#10B981,color:#fff
    class GW,FPS,ENC go
    class SRV,WK,PROD,SEG python
    class TFL model
    class PB proto
    class PROM,LOG obs
```

### Triển khai multi-GPU

Mỗi process Worker gắn một GPU; nginx (stream proxy, `least_conn`) đứng trước để chia stream:

```
Gateway → nginx :50051 ──┬─► worker-0  CUDA_VISIBLE_DEVICES=0  :50052  metrics :9101
                         └─► worker-1  CUDA_VISIBLE_DEVICES=1  :50053  metrics :9102
```

Chi tiết cấu hình và lệnh chạy xem `docs/deployment.md`.

---

## 7. Module Diagram

```mermaid
graph LR
    subgraph pkg["package mediapipe_video"]
        subgraph server_py["server.py"]
            main["main()\nCLI + bootstrap"]
            H["_VideoStreamHandler"]
        end
        subgraph config_py["config.py"]
            lcfg["load_config()\nAppConfig"]
        end
        subgraph log_py["log.py"]
            sl["setup_logging()"]
        end
        subgraph metrics_py["metrics.py"]
            mt["metrics\n(Prometheus)"]
            pl["start_periodic_logger()"]
        end
        subgraph contract_py["contract.py"]
            lc["load_contract()\n→ pb"]
        end
        subgraph segmenter_py["segmenter.py"]
            W["Worker"]
            cs["create_segmenter()"]
            fm["foreground_mask()"]
            wr["wait_for_result()"]
            Seg["Segment()\n→ _segment()"]
            prod["_producer thread"]
            ab["_abort()"]
        end
        subgraph codec_py["codec.py"]
            rle["encode_rle()"]
            tm["threshold_mask()"]
        end
    end

    subgraph ext["External"]
        pb["video_pb2\n(from video.proto)"]
        mp_seg["mp.tasks.vision\n.ImageSegmenter"]
        cv["cv2"]
        grpc_srv["grpc.Server\n(ThreadPoolExecutor)"]
    end

    main --> lcfg
    main --> sl
    main --> mt
    main --> pl
    main --> W
    main --> grpc_srv
    main --> H
    H --> Seg
    grpc_srv --> H
    lc --> pb

    W --> cs
    W --> fm
    W --> wr
    W --> Seg
    Seg --> prod
    Seg --> cs
    Seg --> wr
    Seg --> rle
    Seg --> ab
    Seg --> pb
    Seg --> mt
    wr --> ab
    prod --> cv
    prod --> ab
    ab --> mt

    cs --> mp_seg
    fm --> tm
    fm --> mp_seg

    classDef core fill:#3572A5,color:#fff
    classDef util fill:#6366F1,color:#fff
    classDef ext fill:#6B7280,color:#fff
    class main,W,Seg core
    class H,lcfg,sl,mt,pl,lc,cs,fm,wr,rle,tm,prod,ab util
    class pb,mp_seg,cv,grpc_srv ext
```

---

## 8. Call Flow Diagram

```mermaid
flowchart TD
    A([mediapipe-video --model ...]) --> A1[load config + CLI\nsetup logging + metrics]
    A1 --> A2{delegate}
    A2 -->|auto| A3[thử gpu\nlỗi thì fallback cpu]
    A2 -->|gpu / cpu| A4[Worker.__init__\nBaseOptions + probe]
    A3 --> A4
    A4 -->|probe OK| D[grpc.server start\nbind addr]
    A4 -->|probe FAIL| ERR([exit 1])
    D --> E([Waiting for connections])

    E --> F[gRPC Segment call\nfrom Gateway]
    F --> G{First message\n= config + ids?}
    G -->|No| AB1([abort INVALID_ARGUMENT])
    G -->|Yes| H[Validate config\neffect_type, threshold, dims]
    H -->|invalid| AB2([abort INVALID_ARGUMENT])
    H -->|valid| S{Semaphore GPU\ntrong result_timeout?}
    S -->|hết chỗ| AB6([abort RESOURCE_EXHAUSTED])
    S -->|OK| I[start producer thread\ncreate_segmenter on_result]

    subgraph Producer["Producer thread"]
        P1{Next request}
        P1 -->|ids đổi / payload sai| PAB([abort INVALID_ARGUMENT])
        P1 -->|EOS only / hết stream| PS[put sentinel None]
        P1 -->|frame| P2[validate encoding + timestamp\ncv2.imdecode → RGB → mp.Image]
        P2 -->|lỗi| P3[item mang error_msg]
        P2 -->|OK| P4[item mang mp_image]
        P3 --> PQ[decode_queue.put\nmaxsize 2]
        P4 --> PQ
        PQ --> P1
    end

    I --> P1
    I --> J

    subgraph Consumer["Consumer - gRPC thread"]
        J[decode_queue.get]
        J -->|sentinel| CL([finally: stop_event, join producer,\nrelease semaphore, close segmenter])
        J -->|item| M{item decode OK?}
        M -->|lỗi| M2[mask.status = error]
        M -->|OK| M1[segment_async]
        M1 --> N[wait_for_result\nSimpleQueue.get]
        N -->|timeout| AB3([abort DEADLINE_EXCEEDED])
        N -->|ts callback lớn hơn| AB5([abort INTERNAL])
        N -->|ts callback cũ| DROP[FRAME_DROPS++\nbỏ qua, chờ tiếp]
        DROP --> N
        N -->|client cancel| Z1([return - không abort])
        N -->|OK| O[encode_rle\nmask.status = ok]
        N -->|callback lỗi| O2[mask.status = error]
        O --> R[metrics + log\nyield VideoResponse]
        O2 --> R
        M2 --> R
        R -->|is_eos| CL
        R --> J
    end

    PS -.-> J
    PQ -.-> J

    subgraph MediaPipe Thread
        M3[segment_async triggers\nasync inference]
        M3 --> CB[on_result callback\nforeground_mask\nthreshold_mask]
        CB --> Q[results.put\ntimestamp, mask, error]
    end
    M1 -.-> M3
    Q -.-> N
```

---

## 9. Sequence Diagram

```mermaid
sequenceDiagram
    autonumber
    participant GW as Gateway (Go)
    participant PR as Producer thread
    participant DQ as decode_queue
    participant W as Consumer (gRPC thread)
    participant MP as MediaPipe ImageSegmenter
    participant CB as on_result callback thread
    participant Q as results (SimpleQueue)

    Note over W: Startup: config + logging + metrics, Worker.__init__() probe, grpc.server.start()

    GW->>+W: stream VideoRequest { config }\nsession_id, stream_id, effect_type, threshold, dims
    W->>W: validate config
    W->>W: acquire semaphore GPU (nếu bật)
    W->>PR: start producer thread
    W->>MP: create_segmenter(on_result)\nLIVE_STREAM mode

    GW->>PR: VideoRequest { frame 1 }
    PR->>PR: cv2.imdecode → RGB → mp.Image
    PR->>DQ: put(frame 1)
    DQ-->>W: get → frame 1
    W->>MP: segment_async(frame 1)
    Note right of MP: inference frame 1\ntrên GPU/CPU

    GW->>PR: VideoRequest { frame 2 }
    PR->>PR: decode frame 2 (chồng lấp với inference frame 1)
    PR->>DQ: put(frame 2)

    MP--)CB: on_result(frame 1)
    CB->>CB: foreground_mask() + threshold_mask()
    CB->>Q: put(ts1, mask, None)
    W->>Q: wait_for_result(ts1)
    Q-->>W: (ts1, foreground, None)
    W->>W: encode_rle(frame 1)
    W-->>GW: VideoResponse { frame 1: rle_counts, latency_ms, status="ok" }

    DQ-->>W: get → frame 2
    W->>MP: segment_async(frame 2)

    Note over PR,W: Các frame sau lặp lại như frame 2:\nmỗi frame được trả response ngay khi xử lý xong,\ntrước khi consumer lấy frame kế tiếp

    GW->>PR: VideoRequest { is_eos=true }
    PR->>DQ: put(sentinel None)
    DQ-->>W: get → None
    W->>PR: stop_event.set(), join
    W->>MP: __exit__() — close segmenter
    W-->>-GW: stream closed

    Note over GW,W: Gateway áp dụng mask lên frame gốc\n(bg_blur / bg_replace / bg_remove)
```
