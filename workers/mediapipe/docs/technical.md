# Technical Documentation — MediaPipe Worker

> **Phiên bản**: 1.0.1 (theo `pyproject.toml`)  
> **Ngôn ngữ**: Python ≥ 3.10 (deploy: 3.12)  
> **Cập nhật**: 2026-09-29

---

## Mục lục

1. [Folder Structure](#1-folder-structure)
2. [Chức năng từng Folder](#2-chức-năng-từng-folder)
3. [Chức năng từng Package](#3-chức-năng-từng-package)
4. [Chức năng từng Module](#4-chức-năng-từng-module)
5. [Các Interface](#5-các-interface)
6. [Các Class](#6-các-class)
7. [Các Struct (Protobuf Messages)](#7-các-struct-protobuf-messages)
8. [API](#8-api)
9. [Thread Model](#9-thread-model)
10. [Memory Model](#10-memory-model)
11. [Event Model](#11-event-model)

---

## 1. Folder Structure

```
mediapipe/
├── .gitignore                    # Loại trừ __pycache__/, .venv/, build/, *.egg-info/, checkpoint/*.tflite
├── Makefile                      # install, run, run-cpu, test, cov, lint, typecheck, clean
├── pyproject.toml                # Metadata, dependency, entry point, cấu hình pytest/ruff/mypy/coverage
├── README.md
│
├── src/
│   ├── mediapipe_video/          # Python package (src layout)
│   │   ├── __init__.py           # Export Worker
│   │   ├── __main__.py           # python -m mediapipe_video → server.main()
│   │   ├── server.py             # gRPC bootstrap, CLI, auto-detect delegate
│   │   ├── segmenter.py          # Worker: vòng đời ImageSegmenter + handler Segment
│   │   ├── codec.py              # threshold_mask(), encode_rle()
│   │   ├── config.py             # Dataclass config + YAML loader
│   │   ├── contract.py           # Compile video.proto lúc import → pb
│   │   ├── log.py                # JSON log formatter + file rotation
│   │   ├── metrics.py            # Prometheus metrics + periodic snapshot log
│   │   └── video.proto           # Resource do build_py tạo trong wheel, không duy trì bản source thứ hai
│   ├── proto/
│   │   └── video.proto           # Snapshot từ internal/ai/proto/video.proto; không sửa độc lập
│   └── mediapipe_video.egg-info/ # Sinh bởi pip install -e (không commit)
│
├── etc/
│   └── config.yaml               # Config mặc định (YAML)
│
├── checkpoint/
│   └── selfie_segmenter.tflite   # TFLite model (~249 KB, không commit)
│
├── scripts/                      # install.sh, upgrade.sh, check.sh, start.sh, stop.sh
├── tests/                        # conftest.py, test_codec.py, test_segmenter.py
├── docs/                         # architecture.md, deployment.md, metrics.md, technical.md
├── logs/                         # File log (mediapipe-video.log, rotate theo ngày)
├── wheels/                       # Wheel cho cài offline (pip download)
├── build/                        # Artifact của setuptools (không commit)
└── .venv/, .idea/, .pytest_cache/   # Môi trường/IDE/cache cục bộ (không commit)
```

**Contract proto**: nguồn chính là [`internal/ai/proto/video.proto`](../../../internal/ai/proto/video.proto). Chạy `bash scripts/generate-video-proto.sh` từ root để sinh Go binding và đồng bộ snapshot `src/proto/video.proto`. Python checkout ưu tiên nguồn internal; worker độc lập dùng snapshot. Build wheel/sdist ưu tiên internal nếu có, fallback snapshot khi tách worker; wheel dùng resource `mediapipe_video/video.proto`. Không sửa snapshot bằng tay.

---

## 2. Chức năng từng Folder

| Folder | Chức năng |
|---|---|
| `/` (root) | `pyproject.toml`, `Makefile`, `README.md`, `.gitignore` |
| `src/mediapipe_video/` | Package chính của service |
| `src/proto/` | Contract gRPC gốc dùng chung với Gateway |
| `etc/` | Config runtime (`config.yaml`). Đường dẫn mặc định `etc/config.yaml` tính theo working directory |
| `checkpoint/` | TFLite model. Đường dẫn truyền qua `--model` hoặc `mediapipe.model` trong YAML, không hardcode |
| `scripts/` | Cài đặt / nâng cấp / kiểm tra / start / stop trên server (xem `docs/deployment.md`) |
| `tests/` | Test suite (pytest) |
| `docs/` | Tài liệu dự án |
| `logs/` | Thư mục log mặc định, tự tạo khi `logging.file` được cấu hình |
| `wheels/` | Wheel tải sẵn để cài offline |
| `.venv/` | Virtual environment cục bộ trên máy dev. Trên server dùng venv dùng chung `/opt/nemotron-venv` |

---

## 3. Chức năng từng Package

Project có một Python package nội bộ: **`mediapipe_video`** (`src/mediapipe_video/`), cài qua `pip install` từ source, entry point `mediapipe-video = mediapipe_video.server:main`.

**Dependency runtime** (theo `pyproject.toml`):

| Package | Phiên bản | Chức năng trong project |
|---|---|---|
| `mediapipe` | 0.10.35 | ML inference engine. Cung cấp `ImageSegmenter` (LIVE_STREAM), TFLite runtime, GPU/CPU delegate |
| `grpcio` | 1.81.1 | gRPC server runtime |
| `grpcio-tools` | 1.81.1 | `protoc` binding — compile `video.proto` lúc import (`contract.py`) |
| `protobuf` | 6.33.6 | Protobuf runtime, message classes trong `video_pb2` |
| `numpy` | 2.4.6 | Mảng pixel, threshold mask, RLE |
| `opencv-contrib-python` | ≥ 4.5 | Decode JPEG (`cv2.imdecode`) và đổi BGR → RGB (`cv2.cvtColor`) |
| `PyYAML` | ≥ 6.0 | Đọc `config.yaml` (`yaml.safe_load`) |
| `ruamel.yaml` | ≥ 0.19 | Khai báo trong dependency (code hiện tại không import) |
| `prometheus-client` | ≥ 0.20 | Metrics và HTTP endpoint `/metrics` |

**Dev** (`[project.optional-dependencies].dev`): `pytest`, `pytest-cov`, `ruff`, `mypy`. Lưu ý: `tests/conftest.py` import `PIL` (Pillow) để tạo JPEG mẫu, nhưng Pillow không được khai báo trong dependency nào — cần cài riêng khi chạy test.

**Standard library** (không cần install):

| Module | Dùng ở đâu | Chức năng |
|---|---|---|
| `argparse` | `server.main()` | Parse CLI |
| `concurrent.futures.ThreadPoolExecutor` | `server.main()` | Thread pool cho gRPC server |
| `threading` | `segmenter`, `metrics` | Producer thread, `Event`, `Semaphore`, thread `metrics-logger` |
| `queue` (`Queue`, `SimpleQueue`, `Empty`, `Full`) | `segmenter` | `decode_queue`, hàng đợi kết quả callback |
| `importlib`, `tempfile`, `sys` | `contract.load_contract()` | Import `video_pb2` từ thư mục tạm |
| `logging`, `logging.handlers`, `json`, `datetime` | `log.py` | JSON log, `TimedRotatingFileHandler` |
| `math` | `server`, `segmenter`, `metrics` | `isfinite`, tính quantile |
| `os` | `server.main()` | Đặt `CUDA_VISIBLE_DEVICES` |
| `time` | `segmenter`, `metrics` | `perf_counter()` đo latency, `monotonic()` tính deadline |
| `dataclasses`, `pathlib` | `config.py` | Config và đường dẫn |

---

## 4. Chức năng từng Module

### `server.py`

| Symbol | Loại | Chức năng |
|---|---|---|
| `_VideoStreamHandler` | class | `grpc.GenericRpcHandler` — map `/video.VideoStream/Segment` tới `worker.Segment` |
| `main()` | function | Parse CLI, load config, setup logging/metrics, khởi tạo `Worker` (auto-detect delegate), start gRPC server, chờ termination |

### `segmenter.py`

| Symbol | Loại | Chức năng |
|---|---|---|
| `_abort(context, code, message)` | function | Tăng `STREAM_ERRORS_TOTAL{reason=<code.name.lower()>}` rồi `context.abort()`. Mọi điểm abort đều đi qua đây |
| `Worker` | class | Quản lý cấu hình MediaPipe, semaphore GPU, và handler gRPC `Segment` |

### `codec.py`

| Symbol | Chức năng |
|---|---|
| `threshold_mask(probabilities, threshold)` | Mảng float (H×W hoặc H×W×1) → mảng bool `probabilities > threshold`. Raise `ValueError` nếu shape khác |
| `encode_rle(mask)` | Mask bool → list RLE row-major, run đầu là background (có thể bằng 0). Raise `ValueError` nếu mask rỗng |

### `config.py`

Dataclass `ServerConfig`, `MediaPipeConfig`, `SegmentationConfig`, `LogConfig`, `MetricsConfig`, `AppConfig` và `load_config(path)`. Key lạ trong YAML bị bỏ qua; section thiếu dùng giá trị mặc định.

### `contract.py`

`load_contract()` compile nguồn internal trong checkout đầy đủ, fallback snapshot `src/proto/video.proto` của worker độc lập hoặc resource trong wheel bằng `grpc_tools.protoc` vào thư mục tạm, import `video_pb2`, trả về module. Biến `pb` được tạo ngay lúc import module.

### `log.py`

`_JsonFormatter` (mỗi dòng log là một JSON object) và `setup_logging(cfg)`: handler stdout, cộng thêm `TimedRotatingFileHandler` (rotate 00:00 UTC, giữ `backup_days` file) nếu `logging.file` khác null.

### `metrics.py`

Định nghĩa metric Prometheus, `snapshot()`, `start_periodic_logger(interval)` và `start_metrics_server(port)`. Chi tiết từng metric xem [`docs/metrics.md`](metrics.md).

### `__main__.py` / `__init__.py`

`python -m mediapipe_video` gọi `server.main()`. `__init__` chỉ export `Worker`.

### Test (`tests/`)

| File | Nội dung |
|---|---|
| `conftest.py` | `Aborted`, `MockContext`, `CallbackSegmenter`, helper `make_jpeg` / `deliver_result` / `make_requests`, fixture `context` và `live_stream` |
| `test_codec.py` | `TestThresholdMask`, `TestEncodeRle`, `TestProtoContract` (oneof, `status`) |
| `test_segmenter.py` | `TestSegmentStream`: 3 kiểu EOS, stream đồng thời, lỗi callback, timestamp lặp, timeout, cancel, sai timestamp callback, thiếu config, lỗi khởi tạo GPU |

---

## 5. Các Interface

### 5.1 gRPC Service Interface

Định nghĩa trong `video.proto`, package `video`:

```protobuf
service VideoStream {
  rpc Segment(stream VideoRequest) returns (stream VideoResponse);
}
```

`server.py` đăng ký qua `_VideoStreamHandler`:

```python
grpc.stream_stream_rpc_method_handler(
    worker.Segment,
    request_deserializer=pb.VideoRequest.FromString,
    response_serializer=pb.VideoResponse.SerializeToString,
)
```

### 5.2 gRPC Handler Signature

```python
def Segment(
    self,
    requests: Iterator[pb.VideoRequest],
    context: grpc.ServicerContext,
) -> Iterator[pb.VideoResponse]:
    ...
```

- `requests`: iterator lazy — mỗi `next()` block tới khi client gửi message. Được đọc bởi **producer thread**, không phải thread handler
- `context`: dùng `is_active()` và `abort(code, details)`
- Return: generator — mỗi `yield` gửi một `VideoResponse` ngay

### 5.3 MediaPipe Callback Interface

Callback truyền vào `ImageSegmenter.create_from_options()` (hàm `on_result` bên trong `_segment`):

```python
def on_result(result, image, timestamp_ms) -> None
```

- Chạy trên **thread nội bộ của MediaPipe**
- `result.confidence_masks[i].numpy_view()` **mượn** bộ nhớ MediaPipe — phải threshold xong **trước khi callback return**
- Không được raise: mọi exception được bắt và đưa vào queue dưới dạng `(timestamp_ms, None, str(exc))`; thành công là `(timestamp_ms, foreground, None)`

### 5.4 ImageSegmenter Context Manager

```python
with worker.create_segmenter(callback) as segmenter:
    segmenter.segment_async(mp_image, timestamp_ms)   # non-blocking
```

- `__exit__` dọn resource MediaPipe
- Mỗi gRPC stream có một `ImageSegmenter` riêng

---

## 6. Các Class

### 6.1 `Worker` (`segmenter.py`)

```
Worker
├── Attributes
│   ├── base_options: mp.tasks.BaseOptions   # model_asset_buffer + delegate
│   ├── foreground_index: int | None         # None = tự detect
│   ├── result_timeout: float                # mediapipe.result_timeout_secs (mặc định 10.0)
│   ├── seg_config: SegmentationConfig
│   └── _segmenter_sem: threading.Semaphore | None   # None nếu max_concurrent_segmenters = 0
│
└── Methods
    ├── __init__(mp_config, seg_config=None)
    ├── create_segmenter(callback) → ImageSegmenter
    ├── foreground_mask(result, threshold) → np.ndarray[bool]
    ├── wait_for_result(results, timestamp_ms, context) → np.ndarray[bool] | None
    ├── Segment(requests, context) → Generator[VideoResponse]
    └── _segment(requests, context) → Generator[VideoResponse]
```

#### `__init__(mp_config, seg_config=None)`

| Tham số | Kiểu | Mô tả |
|---|---|---|
| `mp_config` | `MediaPipeConfig` | `model`, `delegate` (`gpu`/`cpu`), `foreground_index`, `result_timeout_secs`. Model được đọc bằng `read_bytes()` và truyền qua `model_asset_buffer` |
| `seg_config` | `SegmentationConfig \| None` | `None` → dùng mặc định |

`Worker` chỉ nhận `delegate` là `gpu` hoặc `cpu` (dùng `Delegate[delegate.upper()]`). Giá trị `auto` được `server.main()` xử lý trước khi tạo `Worker`.

**Side effect**: tạo rồi đóng ngay một `ImageSegmenter` với callback rỗng (startup probe). Model lỗi hoặc GPU không khả dụng sẽ raise ngay tại init, trước khi nhận stream đầu tiên.

#### `create_segmenter(callback)`

Tạo `ImageSegmenter` với `running_mode=LIVE_STREAM`, `output_confidence_masks=True`, `output_category_mask=False`, `result_callback=callback`.

#### `foreground_mask(result, threshold)`

1. Lấy `result.confidence_masks`
2. Chọn index: `foreground_index` nếu có; nếu `None` thì `0` khi có 1 plane, `1` khi có 2 planes, ngoài ra `ValueError` (model multiclass cần chỉ định index)
3. `ValueError` nếu index ngoài phạm vi
4. `numpy_view()` rồi `threshold_mask()` → mảng bool mới (an toàn để giữ)

#### `wait_for_result(results, timestamp_ms, context)`

Polling loop, mỗi lượt `results.get(timeout=min(poll_interval_secs, remaining))`:

```
deadline = now + result_timeout
loop:
    if context không active          → return None   (status đã được set bởi nơi khác)
    if quá deadline                  → abort(DEADLINE_EXCEEDED); return None
    get (timestamp, foreground, error)   (Empty → tiếp tục)
    if timestamp <  timestamp_ms     → FRAME_DROPS_TOTAL++, bỏ qua, chờ tiếp
    if timestamp != timestamp_ms     → abort(INTERNAL); return None
    if error                         → raise ValueError(error)
    return foreground
```

Trả `None` nghĩa là stream đã bị hủy/abort; `_segment` thoát ngay.

#### `Segment(requests, context)`

Wrapper mỏng: `ACTIVE_STREAMS.inc()`, `yield from self._segment(...)`, trong `finally` tăng `STREAMS_TOTAL` và `ACTIVE_STREAMS.dec()`.

#### `_segment(requests, context)`

Handler thật. Chi tiết validation ở [mục 8](#8-api), luồng thread ở [mục 9](#9-thread-model).

---

### 6.2 `AppConfig` và các dataclass (`config.py`)

| Dataclass | Field (mặc định) |
|---|---|
| `ServerConfig` | `addr="127.0.0.1:50052"`, `max_streams=8`, `max_receive_message_bytes=8<<20`, `shutdown_grace_secs=2.0` |
| `MediaPipeConfig` | `model=None`, `delegate="auto"`, `foreground_index=None`, `result_timeout_secs=10.0`, `gpu_device_index=None` |
| `SegmentationConfig` | `default_threshold=0.5`, `max_inference_dim=4096`, `poll_interval_secs=0.1`, `allowed_effect_types=["bg_blur","bg_replace","bg_remove"]`, `max_concurrent_segmenters=0` |
| `LogConfig` | `level="INFO"`, `file=logs/mediapipe-video.log`, `backup_days=30` |
| `MetricsConfig` | `enabled=True`, `port=9101`, `log_interval_secs=60` (`0` = tắt periodic log) |

---

### 6.3 Test doubles (`tests/conftest.py` — chỉ dùng cho test)

- **`Aborted(Exception)`**: có `code` (`grpc.StatusCode`), message là `details`
- **`MockContext`**: `active=True`, `is_active()`, `abort(code, details)` raise `Aborted`
- **`CallbackSegmenter`**: thay `ImageSegmenter`, chạy callback trên `ThreadPoolExecutor(max_workers=1)` để mô phỏng async mà không cần GPU. Có `options`, `process`, `closed`; `segment_async()` submit `process(result_callback, image, timestamp_ms)`
- **Fixture `live_stream`**: patch `ImageSegmenter.create_from_options`, tạo `Worker` với `result_timeout_secs=0.2`, trả `(worker, segmenters, process_ref)`. Thay `process_ref[0]` để đổi cách mock trả (hoặc giữ lại) kết quả

---

## 7. Các Struct (Protobuf Messages)

Định nghĩa trong `video.proto`, sinh thành `video_pb2` lúc runtime (`contract.py`).

### 7.1 `VideoConfig`

Gửi **một lần** trong message đầu tiên.

| Field | Type | Mô tả |
|---|---|---|
| `effect_type` | `string` | Bắt buộc, phải thuộc `segmentation.allowed_effect_types` (mặc định `bg_blur`, `bg_replace`, `bg_remove`) |
| `inference_width` | `uint32` | Chiều rộng JPEG. Phạm vi `1..max_inference_dim` (mặc định 4096) |
| `inference_height` | `uint32` | Chiều cao JPEG. Phạm vi như trên |
| `inference_fps` | `uint32` | FPS mục tiêu sau khi Gateway sample. Chỉ tham khảo, Worker không dùng |
| `threshold` | `float` | Ngưỡng confidence `[0.0, 1.0]`. `0` = dùng `segmentation.default_threshold` |

### 7.2 `VideoFrame`

| Field | Type | Mô tả |
|---|---|---|
| `frame_id` | `int64` | ID tăng dần trong session, Worker echo vào `VideoMask` |
| `rtp_timestamp` | `int64` | RTP timestamp gốc, chỉ echo |
| `original_width` / `original_height` | `int32` | Độ phân giải gốc trước resize (Worker không dùng) |
| `encoding` | `string` | Phải là `"jpeg"` |
| `image_data` | `bytes` | JPEG bytes, kích thước phải khớp config |
| `timestamp_ms` | `int64` | **Phải tăng dần** trong stream. Worker dùng để khớp callback bất đồng bộ |

### 7.3 `VideoRequest`

| Field | Type | Mô tả |
|---|---|---|
| `session_id` | `string` | Không được đổi trong stream |
| `stream_id` | `string` | Không được đổi trong stream |
| `is_eos` | `bool` | `true` = frame cuối, hoặc message thuần EOS (không payload) |
| `payload` | `oneof` | `config` (message đầu) hoặc `frame` (các message sau) |

**Ràng buộc thứ tự**:
1. Message đầu: `payload = config`, `session_id` và `stream_id` không rỗng, `is_eos = false`
2. Mọi message sau: `payload = frame`
3. EOS: `is_eos = true`, kèm frame hoặc là message riêng không payload

### 7.4 `VideoMask`

| Field | Type | Mô tả |
|---|---|---|
| `frame_id` | `int64` | Echo `VideoFrame.frame_id` |
| `rtp_timestamp` | `int64` | Echo `VideoFrame.rtp_timestamp` |
| `mask_width` / `mask_height` | `int32` | Kích thước mask (= kích thước inference). Chỉ được set khi `status="ok"`, lỗi mềm để `0` |
| `rle_counts` | `repeated uint32` | RLE. Tổng = `mask_width × mask_height` khi `status="ok"` |
| `latency_ms` | `float` | Xem ghi chú bên dưới |
| `status` | `string` | `"ok"` hoặc `"error"` |
| `error_message` | `string` | Chi tiết khi `status="error"` |

**Đo `latency_ms`**: `started = time.perf_counter()` đặt trong producer ngay khi nhận frame (trước validate + decode); kết thúc sau khi nhận callback và RLE encode xong. Khoảng này gồm: validate + decode JPEG, thời gian frame nằm trong `decode_queue` (khi consumer còn bận frame trước), inference GPU và encode RLE. Response được yield ngay sau khi frame xử lý xong nên không cộng thêm khoảng chờ frame kế tiếp. Cùng giá trị này được ghi vào histogram `mediapipe_video_inference_latency_seconds`.

**RLE format**: row-major, nhị phân, run đầu **luôn là background** (có thể bằng `0` nếu pixel đầu là foreground). Ví dụ (mask 2×2):
- `[4]` = toàn background
- `[0, 4]` = toàn foreground
- `[0, 1, 2, 1]` = fg tại pixel 0, bg tại pixel 1–2, fg tại pixel 3

**Kích thước RLE**

Lý thuyết (`N = mask_width × mask_height`): tối thiểu 1 phần tử (toàn background), 2 phần tử (toàn foreground, `[0, N]`), tối đa `N + 1` (các pixel xen kẽ). Phạm vi `inference_width/height` là `1..max_inference_dim` (mặc định 4096), nên trường hợp tệ nhất về lý thuyết ở 4096×4096 là trên 16 triệu phần tử.

Đo thực tế (2026-09-29): chạy qua `Worker.Segment` với `checkpoint/selfie_segmenter.tflite`, delegate CPU, JPEG quality 80. Mỗi độ phân giải dùng 5 biến thể của một ảnh chân dung (nguyên bản, lật, hai kiểu crop giữa, crop lệch); tất cả `status="ok"`.

| Độ phân giải | Kích thước mask | `rle_counts` (min / median / max) | Response protobuf | So với tệ nhất (`N+1`) |
|---|---|---|---|---|
| 256×256 | 256×256 | 366 / 537 / 601 | 0,6–0,9 KB | 0,6–0,9% (tệ nhất 65.537) |
| 320×180 | 320×180 | 260 / 357 / 392 | 0,4–0,65 KB | 0,7% |
| 640×360 | 640×360 | 516 / 705 / 819 | 1,0–1,5 KB | 0,36% |
| 1280×720 | 1280×720 | 1056 / 1553 / 1636 | 2,0–2,9 KB | 0,18% |

- Kích thước mask luôn bằng độ phân giải inference (MediaPipe trả mask cùng kích thước ảnh đầu vào).
- Số phần tử RLE xấp xỉ 2–2,3 điểm chuyển trên mỗi hàng ảnh, nên tăng gần tuyến tính theo **chiều cao** ảnh (gấp đôi độ phân giải thì gấp đôi số phần tử, không gấp bốn). Mỗi phần tử tốn khoảng 1,7–1,8 byte trong protobuf (packed varint).
- JPEG đầu vào tương ứng khoảng 13 KB, 44 KB và 130 KB, lớn hơn response hai bậc độ lớn.

Giới hạn của phép đo: chỉ dùng một ảnh một người (ảnh gốc 512×600, bị resize sang 16:9); chưa có cảnh nhiều người, tóc rối hay nền nhiễu (RLE sẽ dài hơn); chưa dùng frame thật từ Gateway.

**Thông lượng (FPS) đo thực tế**

FPS thực tế do Gateway quyết định: Worker không giới hạn FPS và không dùng `inference_fps`. Bảng dưới là khả năng xử lý tối đa của Worker, đo ngày 2026-09-29 trên CPU Intel i5-13420H (12 luồng), delegate CPU, cùng ảnh chân dung, 200 frame mỗi stream, gọi trực tiếp `Worker.Segment` trong cùng process và đẩy frame hết tốc lực (không có mạng, không pacing):

| Độ phân giải | Số stream | FPS mỗi stream | FPS tổng | `latency_ms` P50 / P95 |
|---|---|---|---|---|
| 256×256 | 1 | 206,2 | 206,2 | 15,9 / 18,3 |
| 256×256 | 4 | 127,5 | 510,2 | 24,8 / 28,7 |
| 640×360 | 1 | 121,7 | 121,7 | 20,7 / 97,7 |
| 640×360 | 4 | 62,1 | 248,5 | 39,8 / 89,3 |
| 1280×720 | 1 | 46,0 | 46,0 | 73,2 / 119,4 |
| 1280×720 | 4 | 26,5 | 106,1 | 130,3 / 173,8 |

- Trần mỗi stream bằng nghịch đảo thời gian xử lý một frame của consumer, khoảng 8 ms ở 640×360 và 22 ms ở 1280×720. Đây **không** phải `1000 / latency_ms`: khi frame đến nhanh hơn khả năng xử lý, `latency_ms` gồm cả thời gian chờ trong `decode_queue` (tối đa khoảng hai đến ba frame), nên lớn hơn thời gian xử lý thực.
- Vì cùng lý do, các số latency 16–32 ms trong bảng đo RLE ở trên cũng gồm thời gian chờ queue; chúng không phải thời gian inference thuần.
- Thời gian xử lý tăng theo độ phân giải (JPEG decode, resize, RLE), nhận xét "latency gần như không đổi theo độ phân giải" từ 5 mẫu ban đầu không đúng khi đo dài hơn.
- 4 stream đồng thời chỉ cho khoảng 2× tổng FPS của 1 stream (không 4×), vì cùng tranh CPU.
- Chưa đo GPU, chưa đo qua mạng gRPC thật, và chưa đo với pacing như Gateway. Giá trị Gateway gửi thực tế (nếu thấp hơn các số trên, ví dụ 15–30 fps) thì Worker chưa bị quá tải; nếu cao hơn trần thì `decode_queue` đầy, producer bị chặn và flow control gRPC làm chậm Gateway.

Worker không đặt giới hạn kích thước message gửi đi. Giới hạn nhận của Gateway (mặc định gRPC-Go là 4 MiB) chưa được xác nhận, nhưng với mask người thật ở các độ phân giải đã đo, response dưới 3 KB.

### 7.5 `VideoResponse`

| Field | Type | Mô tả |
|---|---|---|
| `session_id` | `string` | Echo |
| `stream_id` | `string` | Echo |
| `mask` | `VideoMask` | Kết quả segmentation |
| `is_final` | `bool` | Luôn `true` (1 mask / frame) |

---

## 8. API

### 8.1 gRPC Service

```
Service  : video.VideoStream
Transport: gRPC bidirectional streaming (HTTP/2)
Address  : server.addr (mặc định 127.0.0.1:50052)
Security : insecure (không TLS) — thiết kế cho localhost / mạng nội bộ
Max receive message: server.max_receive_message_bytes (mặc định 8 MiB)
Max concurrent RPCs: server.max_streams (mặc định 8) — cũng là max_workers của ThreadPoolExecutor
```

RPC vượt `max_streams` bị chính gRPC từ chối (không xếp hàng).

### 8.2 RPC: `Segment`

```
rpc Segment(stream VideoRequest) returns (stream VideoResponse)
```

**Giao thức trong một stream**:

```
Gateway → Worker: VideoRequest { session_id, stream_id, config { effect_type, dims, threshold } }
Worker  → Gateway: (không gửi gì cho message config)

loop:
  Gateway → Worker: VideoRequest { session_id, stream_id, frame { frame_id, timestamp_ms, jpeg } }
  Worker  → Gateway: VideoResponse { session_id, stream_id, mask { frame_id, rle_counts, status, latency_ms } }
                     (đúng một response cho mỗi frame, gửi ngay khi frame đó xử lý xong,
                      không chờ frame kế tiếp — xem mục 9)

Gateway → Worker: VideoRequest { session_id, stream_id, is_eos=true }
Worker  → Gateway: (stream kết thúc, không còn response nào chờ gửi)
```

**Lỗi cứng — abort stream** (đều qua `_abort()`, tăng `mediapipe_video_stream_errors_total{reason}`):

| Điều kiện | gRPC Status | Nơi kiểm tra |
|---|---|---|
| Message đầu không phải config, thiếu `session_id` hoặc `stream_id`, hoặc `is_eos=true` | `INVALID_ARGUMENT` | `_segment` |
| `effect_type` không nằm trong `allowed_effect_types` | `INVALID_ARGUMENT` | `_segment` |
| `threshold` (sau khi thay `0` bằng default) không finite hoặc ngoài `[0,1]` | `INVALID_ARGUMENT` | `_segment` |
| `inference_width`/`inference_height` ngoài `1..max_inference_dim` | `INVALID_ARGUMENT` | `_segment` |
| Không lấy được semaphore GPU trong `result_timeout` giây | `RESOURCE_EXHAUSTED` | `_segment` (đồng thời `segmenter_exhausted_total++`) |
| `session_id`/`stream_id` đổi giữa stream | `INVALID_ARGUMENT` | producer thread |
| Message sau config không phải `frame` (và không phải EOS thuần) | `INVALID_ARGUMENT` | producer thread |
| Callback MediaPipe không đến trong `result_timeout` | `DEADLINE_EXCEEDED` | `wait_for_result` |
| Timestamp callback lớn hơn timestamp frame đang chờ | `INTERNAL` | `wait_for_result` |

**Client disconnect / stream bị hủy**: `context.is_active()` trả `False`; producer thoát, `wait_for_result` trả `None`, `_segment` return. Không gọi `_abort`, nên không tăng `stream_errors_total`.

**Lỗi mềm** (`mask.status="error"`, stream **không đóng**, frame sau vẫn xử lý):

| Điều kiện | Nơi kiểm tra |
|---|---|
| `encoding != "jpeg"` | producer |
| `timestamp_ms` không lớn hơn timestamp frame trước | producer |
| JPEG decode lỗi hoặc kích thước không khớp config | producer |
| Lỗi trong `on_result` (ví dụ shape mask lạ, index foreground sai) | `wait_for_result` raise `ValueError` |

Lỗi mềm vẫn tăng `frames_total{status="error"}` và trả `VideoResponse` với `mask_width`/`mask_height`/`rle_counts` rỗng.

### 8.3 CLI

```
mediapipe-video [OPTIONS]        (hoặc: python -m mediapipe_video)

  --config PATH             File YAML config (mặc định: etc/config.yaml nếu tồn tại)
  --addr ADDR               gRPC bind address
  --model PATH              File TFLite model [bắt buộc, qua CLI hoặc mediapipe.model]
  --foreground-index INT    Index plane foreground (mặc định: auto-detect)
  --gpu-device-index INT    Chọn GPU; đặt CUDA_VISIBLE_DEVICES (mặc định: system default)
  --max-streams INT         Số stream đồng thời tối đa
  --delegate {gpu,cpu,auto} Backend tính toán (mặc định: auto)
  --result-timeout FLOAT    Giây chờ callback mỗi frame
  --log-level {DEBUG,INFO,WARNING,ERROR}
```

**Thứ tự ưu tiên**: CLI flag (nếu truyền) > YAML > mặc định trong dataclass. Kiểm tra sau khi gộp config: có `model` và là file tồn tại; `max_streams > 0`; `result_timeout_secs` hữu hạn và dương.

**Khởi động (`main`)**:
1. Load config, `setup_logging`, start metrics server (nếu `metrics.enabled`) và periodic logger
2. Nếu có `gpu_device_index` → `os.environ["CUDA_VISIBLE_DEVICES"]` (phải đặt trước khi tạo `Worker` để EGL chọn đúng GPU)
3. `delegate=auto`: thử `gpu`; gặp `ValueError`/`RuntimeError` thì fallback `cpu`; cpu cũng lỗi thì thoát. Delegate cố định lỗi thì thoát với thông báo (kèm gợi ý GPU/EGL nếu là gpu)
4. Cảnh báo khi `delegate=gpu` mà `max_concurrent_segmenters=0` (khuyến nghị 4–6 để tránh cạn EGL context)
5. Tạo gRPC server, bind `addr`, start, log `server ready`, chờ termination

### 8.4 Cấu hình (YAML)

`etc/config.yaml` có 5 section: `logging`, `server`, `mediapipe`, `segmentation`, `metrics`. Ý nghĩa và giá trị mặc định từng key xem ở [mục 6.2](#62-appconfig-và-các-dataclass-configpy) và ngay trong file `etc/config.yaml` (đã có chú thích từng key).

### 8.5 Logging

Log ra stdout (và file nếu cấu hình) dưới dạng JSON lines với 5 field cố định; nội dung sự kiện nằm trong `message` dạng text `key=value`:

```json
{"timestamp": "2026-09-29T03:12:45.123456+00:00", "level": "INFO", "logger": "mediapipe_video.server", "source": "server.py:132", "message": "server ready  addr=127.0.0.1:50052  model=checkpoint/selfie_segmenter.tflite  mediapipe=0.10.35  delegate=gpu"}
```

Các message chính:

| Message | Level | Khi nào |
|---|---|---|
| `server ready  addr=… model=… mediapipe=… delegate=…` | INFO | Server sẵn sàng |
| `auto-detect: GPU available → delegate=gpu` / `auto-detect: GPU unavailable (…) → delegate=cpu` | INFO / WARNING | Kết quả auto-detect |
| `inference  session=… frame=… jpeg=…B fg=…/…px latency=…ms` | INFO | Mỗi frame thành công |
| `frame error  session=… frame=… <lỗi> latency=…ms` | ERROR | Mỗi frame lỗi mềm |
| `metrics_snapshot …` | INFO | Định kỳ `metrics.log_interval_secs` giây |
| `shutting down  grace=…s` | INFO | Nhận Ctrl+C |

### 8.6 Metrics

Endpoint Prometheus `http://<host>:<metrics.port>/metrics` (mặc định 9101), prefix `mediapipe_video_`. Danh sách metric, label, ví dụ PromQL, ngưỡng cảnh báo và dashboard Grafana xem [`docs/metrics.md`](metrics.md).

---

## 9. Thread Model

### 9.1 Tổng quan

```
┌────────────────────────────────────────────────────────────────────┐
│ Process: mediapipe-video                                           │
│                                                                    │
│  Main thread                                                       │
│  ├── load config, setup logging, start metrics                     │
│  ├── Worker.__init__() [startup probe]                             │
│  ├── grpc.server.start()                                           │
│  └── server.wait_for_termination()  ← blocks                       │
│                                                                    │
│  metrics-logger (daemon, 1 thread, nếu log_interval_secs > 0)      │
│  Prometheus HTTP server thread (nếu metrics.enabled)               │
│                                                                    │
│  Với MỖI stream đang hoạt động:                                    │
│                                                                    │
│  gRPC worker thread (ThreadPoolExecutor, max_workers=max_streams)  │
│  └── Worker._segment()  = CONSUMER                                 │
│       ├── decode_queue.get()                                       │
│       ├── segment_async()             [non-blocking, GPU]          │
│       ├── wait_for_result()           [poll SimpleQueue]           │
│       └── encode_rle() + yield VideoResponse                       │
│                                                                    │
│  mp-producer-<session[:8]> (daemon)  = PRODUCER                    │
│  └── đọc requests iterator, validate, JPEG decode (cv2)            │
│       └── decode_queue.put((req, frame, mp_image, err, started))   │
│                                                                    │
│  MediaPipe internal thread (1 / ImageSegmenter)                    │
│  └── TFLite inference → on_result → results.put(...)               │
└────────────────────────────────────────────────────────────────────┘
```

Vậy mỗi stream dùng **2 thread của Worker** (consumer + producer) cộng thread nội bộ của MediaPipe.

### 9.2 Producer–Consumer

Mục đích: consumer không bao giờ chờ network I/O hay JPEG decode; producer đọc và decode frame tiếp theo trong lúc GPU đang xử lý frame trước.

- `decode_queue = Queue(maxsize=2)`, phần tử: `(req, frame, mp_image, error_msg, started)`
- Sentinel `None` → producer báo hết stream (luôn được đặt trong `finally` của producer, `put` timeout 2 s)
- `stop_event` → consumer báo producer thoát sớm khi stream kết thúc/abort. Producer dùng `put(timeout=0.1)` lặp để có thể bị ngắt khi queue đầy

**Vòng lặp consumer** (mỗi vòng xử lý trọn một frame):
1. `decode_queue.get(timeout=result_timeout + 1)`. Nếu `Empty` mà producer đã chết không có sentinel → thoát; nếu producer còn sống → chờ tiếp. Nhận `None` → thoát vòng lặp
2. Nếu frame decode lỗi (`error_msg`): đánh dấu `mask.status="error"`, bỏ qua GPU
3. Nếu decode OK: `segment_async` → `wait_for_result` → encode RLE → `mask.status="ok"` (lỗi callback/RLE → `status="error"`)
4. `_frame_response`: chốt `latency_ms`, cập nhật metric/log → `yield VideoResponse` ngay. Nếu frame mang `is_eos=true` thì kết thúc stream

Vì mỗi frame được chờ xong và yield trước khi lấy frame kế tiếp, GPU chỉ có **tối đa một frame in-flight** và mỗi frame nhận đúng một response, không bị trễ một nhịp. Producer vẫn chồng lấp *decode/đọc mạng* của frame kế tiếp với *inference* của frame hiện tại (qua `decode_queue`).

### 9.3 Giới hạn GPU segmenter

`_segmenter_sem` (`threading.Semaphore(max_concurrent_segmenters)`, tạo khi giá trị > 0):
- Acquire (blocking, timeout `result_timeout`) trước khi tạo producer/segmenter; timeout → `RESOURCE_EXHAUSTED`
- Release trong `finally` cuối của `_segment`
- Mục đích: giới hạn số EGL context đồng thời khi dùng GPU

### 9.4 Synchronization

| Cơ chế | Giữa | Dùng để |
|---|---|---|
| `decode_queue` (`Queue`) | producer → consumer | Truyền frame đã decode, backpressure (maxsize 2) |
| `results` (`SimpleQueue`) | MediaPipe thread → consumer | Truyền kết quả callback |
| `stop_event` | consumer → producer | Dừng producer |
| `_segmenter_sem` | các stream | Giới hạn segmenter đồng thời |

`_abort()` có thể được gọi từ producer thread (lỗi `session_id/stream_id` đổi, payload sai) chứ không chỉ từ thread handler.

### 9.5 Shared State

`Worker` **có** state dùng chung giữa các stream: `_segmenter_sem` (khi bật) và các metric toàn cục (thread-safe theo `prometheus_client`). `base_options`, `foreground_index`, `result_timeout`, `seg_config` chỉ đọc sau init. Mỗi stream có `ImageSegmenter`, `results`, `decode_queue`, `stop_event` và `last_ts` riêng.

### 9.6 Startup Probe

`Worker.__init__()` tạo và đóng ngay một `ImageSegmenter` với callback rỗng. Chạy trên Main thread trước khi server start → không có race condition. Với `delegate=auto`, probe này chính là bước dò GPU.

### 9.7 Shutdown

`KeyboardInterrupt` (Ctrl+C / SIGINT) → `server.stop(shutdown_grace_secs).wait()`. Code không cài handler riêng cho SIGTERM.

---

## 10. Memory Model

### 10.1 Lifetime của dữ liệu theo flow

```
1. JPEG bytes (frame.image_data)
   └── gRPC deserialize → nằm trong VideoRequest
       └── np.frombuffer(image_data, uint8)          ← view, không copy
           └── cv2.imdecode(..., IMREAD_COLOR)       → BGR (H×W×3, uint8) — mảng mới
               └── cv2.cvtColor(BGR2RGB)             → RGB (H×W×3, C-contiguous) — mảng mới
                   └── mp.Image(SRGB, data=rgb)
                       └── decode_queue (tối đa 2 phần tử)
                           └── consumer: segment_async()
                               └── [inference...]
                                   └── on_result callback:
                                       confidence_masks[i].numpy_view()
                                       → BORROWED float32 (H×W)
                                       → threshold_mask() → bool (H×W) MỚI  ← an toàn để giữ
                                       → BORROWED array vô hiệu sau khi callback return

2. Foreground mask (bool, H×W)
   └── results.put((ts, foreground, None)) → SimpleQueue
       └── wait_for_result() trả về
           └── encode_rle() → list[int]
               └── mask.rle_counts.extend() → copy vào protobuf
                   └── yield VideoResponse → serialize → gửi qua gRPC
```

### 10.2 Borrowed References (Critical)

`numpy_view()` trả **view** vào bộ nhớ MediaPipe, không phải copy:

```python
# Trong on_result → foreground_mask():
plane = probabilities[index].numpy_view()   # mượn bộ nhớ MediaPipe
mask = plane > threshold                    # mảng bool mới, an toàn
```

Sau khi `on_result` return, MediaPipe có thể tái sử dụng vùng nhớ. Chỉ `mask` được đưa vào `results`, không bao giờ đưa `plane`. Fixture `deliver_result` trong `tests/conftest.py` kiểm chứng bằng cách `plane.fill(0)` ngay sau callback.

### 10.3 Per-Frame Allocations

Ví dụ 640×360:

| Object | Kích thước | Lifetime |
|---|---|---|
| JPEG bytes trong `VideoRequest` | ~20–100 KB | Đến khi request/frame được giải phóng (giữ trong tuple ở `decode_queue` và biến cục bộ của consumer) |
| `np.frombuffer` view | O(1) | Ngắn |
| BGR array | 640×360×3 ≈ 691 KB | Đến khi vòng producer kế tiếp gán lại biến `bgr` |
| RGB array | ≈ 691 KB | Đến khi `mp.Image`/MediaPipe dùng xong |
| `mp.Image` | wrapper | Trong `decode_queue` rồi đến khi inference xong |
| Confidence mask (MediaPipe) | float32 H×W ≈ 922 KB | Mượn, chỉ hợp lệ trong callback |
| Bool mask | 640×360 = 230 KB | Đến khi `encode_rle()` xong |
| RLE list | N × ~8 B (list Python) | Đến khi copy vào protobuf |
| `VideoResponse` | ≈ N × 4 B + overhead | Đến khi gRPC gửi xong |

Ước lượng số frame decode đang sống cùng lúc trên **một** stream: tối đa 2 trong `decode_queue` + 1 producer đang giữ khi chờ `put` + 1 frame đang ở GPU, tức khoảng 4. Tổng bộ nhớ ảnh xấp xỉ `max_streams × ~4 × kích thước RGB`.

### 10.4 Model Weights

`Worker.__init__` đọc toàn bộ file `.tflite` (~249 KB) bằng `read_bytes()` một lần và giữ trong `base_options` (`model_asset_buffer`); mọi `ImageSegmenter` tạo cho các stream đều dùng lại `base_options` này.

### 10.5 Giới hạn Message Size

```python
options=(("grpc.max_receive_message_length", cfg.server.max_receive_message_bytes),)  # mặc định 8 MiB
```

Thực tế JPEG 640×360 ≈ 20–100 KB.

---

## 11. Event Model

### 11.1 Các Event trong Lifecycle

```
STARTUP EVENTS
──────────────
[E1] process_start        Parse CLI, load YAML config, setup logging
[E2] metrics_started      Start /metrics server (nếu enabled) và periodic logger (nếu interval > 0)
[E3] contract_loaded      contract.py compile video.proto → pb  (xảy ra lúc import module)
[E4] gpu_device_set       CUDA_VISIBLE_DEVICES được đặt (nếu có gpu_device_index)
[E5] worker_ready         Worker.__init__() probe thành công
                          (delegate=auto: thử gpu, lỗi thì fallback cpu — log auto-detect)
[E6] server_listening     gRPC server bind + start → log "server ready ..."

STREAM EVENTS (lặp cho mỗi gRPC Segment call)
──────────────────────────────────────────────
[E7]  stream_open         Segment() được dispatch, ACTIVE_STREAMS++
[E8]  config_received     Message đầu (VideoConfig) nhận được
[E9]  config_validated    Validate pass
[E10] semaphore_acquired  Lấy slot GPU (nếu max_concurrent_segmenters > 0)
[E11] producer_started    Tạo thread mp-producer-*
[E12] segmenter_created   ImageSegmenter được tạo, ACTIVE_SEGMENTERS++

  Producer, mỗi frame:
  [E13] frame_received    Đọc VideoRequest{frame} từ iterator
  [E14] frame_validated   Kiểm tra encoding + timestamp tăng dần
  [E15] jpeg_decoded      cv2.imdecode + cvtColor + mp.Image (hoặc ghi error_msg)
  [E16] frame_queued      decode_queue.put()

  Consumer, mỗi frame:
  [E17] frame_dequeued    decode_queue.get()
  [E18] inference_submitted  segment_async() → MediaPipe nhận frame (frame decode lỗi thì bỏ qua E18–E20)

  MediaPipe thread:
  [E19] inference_complete   on_result được gọi
  [E20] mask_queued          results.put((ts, foreground, None)) — threshold xong trong callback

  Consumer, ngay sau khi nhận kết quả:
  [E21] response_sent     wait_for_result → encode_rle → yield VideoResponse
        → log "inference ..." (ok) hoặc "frame error ..." (error)
        → FRAMES_TOTAL, INFERENCE_LATENCY, FRAME_BYTES, FOREGROUND_RATIO cập nhật

[E22] eos_received        is_eos=true, hoặc iterator kết thúc (half-close)
[E23] producer_sentinel   Producer đặt None vào decode_queue
[E24] stream_closed       stop_event.set(), drain decode_queue, join producer, release semaphore,
                          ACTIVE_SEGMENTERS--, STREAMS_TOTAL++, ACTIVE_STREAMS--

EXCEPTION EVENTS
────────────────
[Ex1] frame_error         encoding sai / timestamp không tăng / JPEG lỗi hoặc sai kích thước
      → mask.status="error", stream tiếp tục
[Ex2] callback_error      foreground_mask() lỗi (shape lạ, index sai)
      → (ts, None, error_str) vào queue → wait_for_result raise ValueError
      → mask.status="error", stream tiếp tục
[Ex3] stale_callback      Callback có timestamp nhỏ hơn timestamp đang chờ
      → FRAME_DROPS_TOTAL++, bỏ qua, tiếp tục chờ
[Ex4] timeout             Quá result_timeout không có callback
      → abort(DEADLINE_EXCEEDED)
[Ex5] cancel              context.is_active() = False (client disconnect)
      → producer thoát, wait_for_result trả None, _segment return (không abort, không tăng stream_errors)
[Ex6] timestamp_mismatch  Callback có timestamp lớn hơn timestamp đang chờ
      → abort(INTERNAL)
[Ex7] config_invalid      Validate config thất bại tại E9
      → abort(INVALID_ARGUMENT), segmenter/producer chưa được tạo
[Ex8] segmenter_exhausted Không lấy được semaphore trong result_timeout
      → SEGMENTER_EXHAUSTED_TOTAL++, abort(RESOURCE_EXHAUSTED)
[Ex9] stream_protocol_error  session/stream id đổi hoặc payload không phải frame (sau config)
      → abort(INVALID_ARGUMENT) từ producer thread

SHUTDOWN EVENTS
───────────────
[E25] sigint_received     KeyboardInterrupt (Ctrl+C)
[E26] graceful_stop       server.stop(shutdown_grace_secs).wait() — mặc định 2 giây
[E27] process_exit
```

### 11.2 Event Flow Diagram

```
[E1] → [E2] → [E3] → [E4] → [E5] → [E6]
                                    ↓
   ┌── [E7] stream open ─────────────────────────────────────────────────┐
   │   [E8] config → [E9] validated → [E10] semaphore → [E11] producer   │
   │                                        → [E12] segmenter created    │
   │                                                                     │
   │   PRODUCER thread                      CONSUMER (gRPC thread)       │
   │   [E13] frame received                                              │
   │   [E14] validated                                                   │
   │   [E15] jpeg decoded                                                │
   │   [E16] queued ──────────────────────► [E17] dequeued               │
   │        ▲                               [E18] submit frame này ──┐   │
   │        │ (decode frame kế tiếp                 ↓ (MediaPipe)    │   │
   │        │  trong lúc GPU chạy)          [E19] inference_complete │   │
   │        │                               [E20] mask_queued ───────┘   │
   │        │                               [E21] response_sent          │
   │   [E22] EOS / half-close                                            │
   │   [E23] sentinel ────────────────────► [E24] stream_closed          │
   └─────────────────────────────────────────────────────────────────────┘
                     ↓ (lặp cho stream tiếp theo)
                [E25] → [E26] → [E27]
```

Response của frame N được yield ngay ở E21 của chính frame N, trước khi consumer lấy frame N+1 khỏi queue.

### 11.3 EOS Handling — Ba trường hợp

| Cách | Mô tả | Code path |
|---|---|---|
| **Separate EOS** | Message riêng `{session_id, stream_id, is_eos=true}`, không payload | Producer: `req.is_eos and payload is None` → return → sentinel |
| **Frame+EOS** | Frame cuối kèm `is_eos=true` | Producer xử lý và `put` frame bình thường, sau đó `if req.is_eos: return` → sentinel; consumer xử lý frame này, yield response rồi kết thúc |
| **Half-close** | Gateway đóng write-side, không gửi EOS message | Iterator cạn → vòng `for` của producer kết thúc → `finally` đặt sentinel |

---

## Appendix: Cấu trúc proto đầy đủ

```protobuf
syntax = "proto3";
package video;
option go_package = "media-ai-gateway/internal/ai/proto";

service VideoStream {
  rpc Segment(stream VideoRequest) returns (stream VideoResponse);
}

message VideoConfig {
  string effect_type      = 1;
  uint32 inference_width  = 2;
  uint32 inference_height = 3;
  uint32 inference_fps    = 4;
  float  threshold        = 5;
}

message VideoFrame {
  int64  frame_id        = 1;
  int64  rtp_timestamp   = 2;
  int32  original_width  = 3;
  int32  original_height = 4;
  string encoding        = 5;
  bytes  image_data      = 6;
  int64  timestamp_ms    = 7;
}

message VideoRequest {
  string session_id = 1;
  bool   is_eos     = 2;
  oneof payload {
    VideoConfig config = 3;
    VideoFrame  frame  = 4;
  }
  string stream_id  = 5;
}

message VideoMask {
  int64           frame_id      = 1;
  int64           rtp_timestamp = 2;
  int32           mask_width    = 3;
  int32           mask_height   = 4;
  repeated uint32 rle_counts    = 5;
  float           latency_ms    = 6;
  string          status        = 7;
  string          error_message = 8;
}

message VideoResponse {
  string    session_id = 1;
  VideoMask mask       = 2;
  bool      is_final   = 3;
  string    stream_id  = 4;
}
```
