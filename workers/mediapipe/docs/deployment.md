# Hướng dẫn triển khai mediapipe-video

## Mục lục

1. [Yêu cầu hệ thống](#1-yêu-cầu-hệ-thống)
2. [Cấu trúc thư mục](#2-cấu-trúc-thư-mục)
3. [Kiểm tra điều kiện (check.sh)](#3-kiểm-tra-điều-kiện-checksh)
4. [Cài đặt lần đầu](#4-cài-đặt-lần-đầu)
5. [Cấu hình sau cài đặt](#5-cấu-hình-sau-cài-đặt)
6. [Khởi động ứng dụng](#6-khởi-động-ứng-dụng)
7. [Dừng ứng dụng](#7-dừng-ứng-dụng)
8. [Nâng cấp code](#8-nâng-cấp-code)
9. [Cài đặt offline](#9-cài-đặt-offline)
10. [Theo dõi log](#10-theo-dõi-log)
11. [Metrics (Prometheus)](#11-metrics-prometheus)
12. [Xử lý lỗi thường gặp](#12-xử-lý-lỗi-thường-gặp)

> **Tip — Dry-run trước khi thực hiện:**  
> Cả `install.sh` và `upgrade.sh` đều hỗ trợ flag `--dry-run`.  
> Script sẽ in ra từng bước sẽ làm mà **không thay đổi bất kỳ file nào** trên hệ thống.

---

## 3. Kiểm tra điều kiện (check.sh)

`check.sh` là script kiểm tra độc lập, được gọi **tự động** bởi `install.sh` và `upgrade.sh` trước khi thực hiện bất kỳ thay đổi nào. Có thể chạy thủ công bất cứ lúc nào.

### Các nhóm kiểm tra

| Nhóm | Nội dung |
|---|---|
| G1: Hệ thống | OS, kiến trúc CPU, bash ≥ 4.0, glibc ≥ 2.17 (manylinux2014) |
| G2: Python | Python 3.10+ có trong PATH, `python -m venv` hoạt động được |
| G3: System libs | `libstdc++.so.6`, `libgomp.so.1`, `libGL.so.1`, `libglib-2.0.so.0`, `libEGL.so`, `libxcb.so.1`, `libX11.so`, `libXext.so`, `libSM.so`, `libXrender.so`, `libz.so` |
| G4: GPU | `nvidia-smi` OK, VRAM, driver ≥ 450 (chỉ khi `delegate=gpu`) |
| G5: Disk & quyền | ≥ 2 GB trống, ghi được vào APP_DIR |
| G6: Source files | `pyproject.toml`, `etc/config.yaml`, `src/`, `scripts/` tồn tại |
| G7: Wheels | Từng package có file `.whl` tương ứng (chỉ khi `--offline`) |
| G8: Wheel compat | Arch tag, Python cp-tag, manylinux glibc version khớp server (chỉ khi `--offline`) |
| G9: Cài đặt hiện tại | venv tồn tại, package đã cài, hiển thị version cũ → mới (chỉ khi `--mode upgrade`) |
| G10: Runtime | model file tồn tại, config hợp lệ, port không bị chiếm |

### Chạy thủ công

```bash
# Kiểm tra toàn bộ (tất cả nhóm)
bash scripts/check.sh --app-dir /u01/ims/mediapipe_video

# Kiểm tra trước khi install offline với GPU
bash scripts/check.sh --mode install --delegate gpu --offline

# Kiểm tra ứng dụng có thể chạy được không
bash scripts/check.sh --mode runtime --app-dir /u01/ims/mediapipe_video

# Kiểm tra trước khi upgrade offline
bash scripts/check.sh --mode upgrade --offline
```

### Ví dụ output

```
[2026-09-25T04:00:00Z] Bắt đầu kiểm tra (mode=install, delegate=gpu, offline=true)
──────────────────────────────────────────────────
[INFO]  Kiểm tra hệ điều hành & kiến trúc ...
[PASS]  OS: ubuntu 22.04 | arch: x86_64
[PASS]  Bash 5.1
[PASS]  glibc 2.35 (tương thích manylinux2014; một số wheel mới cần ≥ 2.28)
[INFO]  Kiểm tra Python ...
[PASS]  Python 3.11.9 → /usr/bin/python3.11
[INFO]  Kiểm tra python venv module ...
[PASS]  python -m venv: OK
[INFO]  Kiểm tra system libraries ...
[PASS]  libstdc++.so.6 (C++ runtime)
[PASS]  libgomp.so.1 (OpenMP)
[PASS]  libGL.so.1 (OpenGL)
[PASS]  libglib-2.0.so.0
[PASS]  libEGL.so (EGL — cần cho GPU delegate)
[PASS]  libxcb.so.1 (X11 XCB)
[PASS]  libX11.so
[PASS]  libXext.so
[PASS]  libSM.so
[PASS]  libXrender.so
[PASS]  libz.so (zlib)
[INFO]  Kiểm tra disk và quyền truy cập ...
[PASS]  Disk space: 45 GB tại /u01/ims
[PASS]  Ghi được vào /u01/ims (APP_DIR sẽ được tạo)
[INFO]  Kiểm tra source files tại /u01/ims/namcht/mediapipe ...
[PASS]  Source: pyproject.toml
[PASS]  Source: etc/config.yaml
[PASS]  Source: src/
[PASS]  Source: scripts/
[INFO]  Kiểm tra GPU (delegate=gpu) ...
[PASS]  GPU: NVIDIA A10 | driver: 525.89.02 | VRAM: 23028 MiB
[INFO]  Kiểm tra offline wheels tại /u01/ims/namcht/mediapipe/wheels ...
[PASS]  Wheels directory tồn tại (21 files)
[PASS]  Wheel hiện diện: mediapipe 1.0.1*
[PASS]  Wheel hiện diện: grpcio 1.83.1*
[PASS]  Wheel hiện diện: grpcio_tools 1.83.1*
[PASS]  Wheel hiện diện: protobuf 7.36.1*
[PASS]  Wheel hiện diện: numpy 2.5.3*
[PASS]  Wheel hiện diện: Pillow 12.3.0*
[PASS]  Wheel hiện diện: PyYAML 6.*
[PASS]  Wheel hiện diện: mediapipe_video 1.*
[INFO]  Kiểm tra tính tương thích wheel (Python=cp311, arch=x86_64, glibc=2.35) ...
[PASS]  Wheel tương thích: mediapipe-1.0.1-cp311-cp311-manylinux_2_17_x86_64.whl
[PASS]  Wheel tương thích: grpcio-1.83.1-cp311-cp311-linux_x86_64.whl
[PASS]  Tất cả compiled wheels tương thích với server
[WARN]  APP_DIR chưa được cài đặt (/u01/ims/mediapipe_video/libs/venv không tồn tại)
──────────────────────────────────────────────────
Kết quả: 28 passed  1 warning  0 failed
[KẾT QUẢ] ĐẠT (có cảnh báo) — kiểm tra các mục WARN ở trên
```

### Kết quả và exit code

| Kết quả | Exit code | Ý nghĩa |
|---|---|---|
| Tất cả PASS | `0` | Sẵn sàng |
| Có WARN, không có FAIL | `0` | Tiếp tục được, cần xem lại cảnh báo |
| Có FAIL | `1` | Dừng lại, sửa lỗi trước |

### Bỏ qua check (không khuyến nghị)

```bash
bash scripts/install.sh --offline --skip-check
bash scripts/upgrade.sh --offline --skip-check
```

---

## 1. Yêu cầu hệ thống

| Thành phần | Yêu cầu |
|---|---|
| OS | RHEL 8.x / CentOS Stream 8 hoặc Ubuntu 20.04+ |
| glibc | ≥ 2.17 (RHEL 8 có 2.28 — đủ cho tất cả manylinux wheels) |
| Python | 3.10, 3.11 hoặc 3.12 |
| GPU (nếu dùng delegate gpu) | NVIDIA GPU có driver ≥ 450 + EGL/OpenGL ES |
| RAM | Tối thiểu 2 GB |
| Disk | Tối thiểu 2 GB (venv + model) |

### Chuẩn bị Python trên RHEL 8

Python mặc định trên RHEL 8 là 3.6 — cần cài 3.11 từ AppStream:

```bash
# Bật AppStream module và cài Python 3.11
sudo dnf module enable python311 -y
sudo dnf install python3.11 -y

# Kiểm tra
python3.11 --version   # Python 3.11.x
python3.11 -m venv --help   # venv bundled — không cần package riêng
```

```bash
nvidia-smi               # kiểm tra GPU (nếu dùng GPU)
```

---

## 2. Cấu trúc thư mục

Sau khi cài đặt, hệ thống gồm hai thư mục độc lập:

```
SRC_DIR  (mặc định: /u01/ims/namcht/mediapipe)   ← source code
APP_DIR  (mặc định: /u01/ims/mediapipe_video)     ← ứng dụng chạy thật
```

### SRC_DIR — Source code

```
mediapipe/
├── etc/config.yaml         template cấu hình
├── scripts/
│   ├── install.sh          cài đặt lần đầu
│   ├── upgrade.sh          nâng cấp code
│   ├── start.sh            khởi động (template)
│   └── stop.sh             dừng (template)
├── src/                    Python package
├── wheels/                 wheel cache (dùng khi offline)
└── pyproject.toml
```

### APP_DIR — Ứng dụng chạy thật

```
mediapipe_video/
├── bin/
│   └── mediapipe-video     wrapper executable
├── etc/
│   └── config.yaml         config runtime (chỉnh tại đây)
├── libs/
│   └── venv/               Python virtual environment
├── logs/
│   ├── mediapipe-video.log  JSON log (rotate theo ngày)
│   ├── mediapipe-video.log.2026-09-24  log ngày hôm qua
│   └── stdout.log           stdout/stderr của process
├── models/
│   └── selfie_segmenter.tflite   ← copy model vào đây
├── scripts/
│   ├── start.sh
│   └── stop.sh
└── logs/mediapipe-video.pid
```

---

## 4. Cài đặt lần đầu

### Bước 1 — Upload source code lên server

```bash
# Trên máy local: đóng gói
tar -czf mediapipe.tar.gz \
    --exclude='.venv' \
    --exclude='__pycache__' \
    --exclude='*.egg-info' \
    -C /path/to/project .

# Upload lên server
scp mediapipe.tar.gz user@server:/u01/ims/namcht/

# Trên server: giải nén
mkdir -p /u01/ims/namcht/mediapipe
tar -xzf /u01/ims/namcht/mediapipe.tar.gz -C /u01/ims/namcht/mediapipe
```

### Bước 2 — Chạy install.sh

**Xem trước sẽ làm gì (không thay đổi gì):**

```bash
cd /u01/ims/namcht/mediapipe
bash scripts/install.sh --dry-run
```

```
[DRY-RUN] ════════════════════════════════════════════
[DRY-RUN] Dry-run mode — no changes will be made
[DRY-RUN] Re-run without --dry-run to apply
[DRY-RUN] ════════════════════════════════════════════

[2026-09-25T04:00:00Z] INFO  SRC_DIR : /u01/ims/namcht/mediapipe
[2026-09-25T04:00:00Z] INFO  APP_DIR : /u01/ims/mediapipe_video
[2026-09-25T04:00:01Z] INFO  GPU detected: NVIDIA A10 → delegate=gpu
[2026-09-25T04:00:01Z] INFO  Python   : /usr/bin/python3.11 (Python 3.11.9)
[2026-09-25T04:00:01Z] INFO  Creating directory layout under /u01/ims/mediapipe_video ...
[DRY-RUN] mkdir -p /u01/ims/mediapipe_video/bin ...
[DRY-RUN] /usr/bin/python3.11 -m venv /u01/ims/mediapipe_video/libs/venv
[DRY-RUN] /u01/ims/mediapipe_video/libs/venv/bin/pip install /u01/ims/namcht/mediapipe --quiet
[DRY-RUN] would: Package installed
[DRY-RUN] would write bin wrapper: /u01/ims/mediapipe_video/bin/mediapipe-video
[DRY-RUN] cp .../etc/config.yaml /u01/ims/mediapipe_video/etc/config.yaml
[DRY-RUN] would: Config written (delegate=gpu)
[DRY-RUN] install -m 0755 .../start.sh /u01/ims/mediapipe_video/scripts/start.sh
[DRY-RUN] install -m 0755 .../stop.sh  /u01/ims/mediapipe_video/scripts/stop.sh

[DRY-RUN] Dry-run complete — nothing was changed
[DRY-RUN] Re-run without --dry-run to apply
```

**Thực sự cài đặt — Server GPU (auto-detect):**

```bash
bash scripts/install.sh
```

**Server GPU (chỉ định rõ):**

```bash
bash scripts/install.sh --delegate gpu
```

**Server CPU (không có GPU):**

```bash
bash scripts/install.sh --delegate cpu
```

**Chỉ định thư mục cài đặt khác:**

```bash
bash scripts/install.sh \
    --delegate gpu \
    --app-dir /u01/ims/mediapipe_video
```

**Dùng venv có sẵn (ví dụ chia sẻ venv với project khác):**

```bash
bash scripts/install.sh --offline --venv /opt/nemotron-venv
```

Script sẽ cài vào venv chỉ định thay vì tạo mới tại `APP_DIR/libs/venv`. Đường dẫn venv được lưu vào `SRC_DIR/.app_venv` để `upgrade.sh` tự đọc lại — không cần truyền `--venv` khi upgrade.

**Kết quả thành công:**

```
[2026-09-25T04:00:00Z] INFO  SRC_DIR : /u01/ims/namcht/mediapipe
[2026-09-25T04:00:00Z] INFO  APP_DIR : /u01/ims/mediapipe_video
[2026-09-25T04:00:01Z] INFO  GPU detected: NVIDIA L4 → delegate=gpu
[2026-09-25T04:00:01Z] INFO  Python   : /usr/bin/python3.12 (Python 3.12.1)
[2026-09-25T04:00:01Z] INFO  Using venv   : /opt/nemotron-venv
[2026-09-25T04:00:05Z] OK    Package installed
[2026-09-25T04:00:05Z] OK    Config written (delegate=gpu)
[2026-09-25T04:00:05Z] OK    Scripts installed: /u01/ims/mediapipe_video/scripts/
[2026-09-25T04:00:05Z] INFO  ═══════════════════════════════════════════
[2026-09-25T04:00:05Z] INFO  Installation complete
[2026-09-25T04:00:05Z] INFO    APP_DIR  : /u01/ims/mediapipe_video
[2026-09-25T04:00:05Z] INFO    delegate : gpu
[2026-09-25T04:00:05Z] INFO    venv     : /opt/nemotron-venv
```

### Bước 3 — Copy model TFLite

```bash
cp selfie_segmenter.tflite /u01/ims/mediapipe_video/models/
```

---

## 5. Cấu hình sau cài đặt

File cấu hình runtime nằm tại: `/u01/ims/mediapipe_video/etc/config.yaml`

```yaml
logging:
  level: "INFO"                                        # DEBUG | INFO | WARNING | ERROR
  file: "/u01/ims/mediapipe_video/logs/mediapipe-video.log"
  backup_days: 30                                      # giữ log 30 ngày

server:
  addr: "127.0.0.1:50052"      # bind address của gRPC server
  max_streams: 8               # số stream đồng thời tối đa
  max_receive_message_bytes: 8388608   # 8 MiB
  shutdown_grace_secs: 2.0

mediapipe:
  model: "/u01/ims/mediapipe_video/models/selfie_segmenter.tflite"   # ← BẮT BUỘC
  delegate: "gpu"              # "gpu" hoặc "cpu"
  foreground_index: null       # null = tự detect
  result_timeout_secs: 10.0

segmentation:
  default_threshold: 0.5
  max_inference_dim: 4096
  allowed_effect_types:
    - bg_blur
    - bg_replace
    - bg_remove
  # Giới hạn số ImageSegmenter đồng thời để tránh vượt EGL context limit của GPU driver.
  # 0 = không giới hạn (chỉ dùng cho CPU delegate).
  # GPU: đặt 4–6 tùy driver; nếu thấy lỗi RESOURCE_EXHAUSTED thì giảm giá trị này.
  max_concurrent_segmenters: 0
```

**Mục bắt buộc phải chỉnh:** `mediapipe.model` — đặt đường dẫn tuyệt đối tới file `.tflite`.

Sau khi chỉnh config, không cần reinstall — chỉ cần restart ứng dụng.

---

## 6. Khởi động ứng dụng

### Khởi động bình thường (background)

```bash
/u01/ims/mediapipe_video/scripts/start.sh
```

```
[2026-09-25T04:10:00Z] INFO  Starting mediapipe-video (APP_DIR=/u01/ims/mediapipe_video) ...
[2026-09-25T04:10:01Z] INFO  Started (PID 12345)
[2026-09-25T04:10:01Z] INFO    config    : /u01/ims/mediapipe_video/etc/config.yaml
[2026-09-25T04:10:01Z] INFO    stdout    : /u01/ims/mediapipe_video/logs/stdout.log
[2026-09-25T04:10:01Z] INFO    json log  : /u01/ims/mediapipe_video/logs/mediapipe-video.log
[2026-09-25T04:10:01Z] INFO    pid file  : /u01/ims/mediapipe_video/logs/mediapipe-video.pid
```

### Khởi động foreground (debug / systemd / docker)

```bash
/u01/ims/mediapipe_video/scripts/start.sh --foreground
```

### Kiểm tra đã chạy chưa

```bash
# Đọc PID
cat /u01/ims/mediapipe_video/logs/mediapipe-video.pid

# Kiểm tra process còn sống không
kill -0 $(cat /u01/ims/mediapipe_video/logs/mediapipe-video.pid) && echo "running" || echo "stopped"

# Xem log khởi động
tail -20 /u01/ims/mediapipe_video/logs/mediapipe-video.log
```

Ứng dụng sẵn sàng khi thấy log:

```json
{"timestamp":"2026-09-25T04:10:02.123456+00:00","level":"INFO","logger":"mediapipe_video.server","source":"server.py:78","message":"server ready  addr=127.0.0.1:50052  model=...  delegate=gpu"}
```

---

## 7. Dừng ứng dụng

### Dừng bình thường (graceful — khuyến nghị)

```bash
/u01/ims/mediapipe_video/scripts/stop.sh
```

```
[2026-09-25T05:00:00Z] INFO  Sending SIGTERM to PID 12345 (grace=5s) ...
[2026-09-25T05:00:02Z] INFO  Stopped (elapsed 2s)
```

### Dừng với timeout tùy chỉnh

```bash
/u01/ims/mediapipe_video/scripts/stop.sh --grace 10
```

Script gửi `SIGTERM` trước, chờ tối đa `--grace` giây cho process tự thoát. Nếu hết thời gian, tự động gửi `SIGKILL`.

---

## 8. Nâng cấp code

### Quy trình

1. Upload source code mới lên `SRC_DIR`
2. Chạy `upgrade.sh` — script tự động stop → cài lại → restart

### Xem trước trước khi nâng cấp

```bash
cd /u01/ims/namcht/mediapipe
bash scripts/upgrade.sh --dry-run
```

```
[DRY-RUN] ════════════════════════════════════════════
[DRY-RUN] Dry-run mode — no changes will be made
[DRY-RUN] ════════════════════════════════════════════

[2026-09-25T06:00:00Z] INFO  SRC_DIR : /u01/ims/namcht/mediapipe
[2026-09-25T06:00:00Z] INFO  APP_DIR : /u01/ims/mediapipe_video
[2026-09-25T06:00:00Z] INFO  Installed version : 1.0.0
[2026-09-25T06:00:00Z] INFO  Source version    : 1.0.1
[2026-09-25T06:00:00Z] INFO  Service is running (PID 12345) — will stop before upgrade
[DRY-RUN] bash /u01/ims/mediapipe_video/scripts/stop.sh
[DRY-RUN] /u01/ims/mediapipe_video/libs/venv/bin/pip install --upgrade ...
[DRY-RUN] would: Package upgraded (1.0.0 → 1.0.1)
[DRY-RUN] install -m 0755 .../start.sh /u01/ims/mediapipe_video/scripts/start.sh
[DRY-RUN] install -m 0755 .../stop.sh  /u01/ims/mediapipe_video/scripts/stop.sh
[DRY-RUN] bash /u01/ims/mediapipe_video/scripts/start.sh

[DRY-RUN] Dry-run complete — nothing was changed
```

### Nâng cấp với restart tự động

```bash
cd /u01/ims/namcht/mediapipe
bash scripts/upgrade.sh
```

```
[2026-09-25T06:00:00Z] INFO  SRC_DIR : /u01/ims/namcht/mediapipe
[2026-09-25T06:00:00Z] INFO  APP_DIR : /u01/ims/mediapipe_video
[2026-09-25T06:00:00Z] INFO  Stopping running instance (PID 12345) ...
[2026-09-25T06:00:02Z] INFO  Upgrading mediapipe-video (offline=false) ...
[2026-09-25T06:00:10Z] OK    Package upgraded
[2026-09-25T06:00:10Z] OK    Scripts synced
[2026-09-25T06:00:10Z] INFO  Restarting ...
[2026-09-25T06:00:11Z] INFO  Started (PID 12399)
[2026-09-25T06:00:11Z] OK    Upgrade complete
```

### Nâng cấp không restart (bảo trì)

```bash
bash scripts/upgrade.sh --no-restart
# Khởi động lại thủ công sau
/u01/ims/mediapipe_video/scripts/start.sh
```

### Lưu ý

- `upgrade.sh` **không** ghi đè `APP_DIR/etc/config.yaml` — mọi chỉnh sửa config được giữ nguyên.
- `upgrade.sh` **có** ghi đè `APP_DIR/scripts/start.sh` và `stop.sh` — các script quản lý luôn được cập nhật cùng code.
- `upgrade.sh` tự đọc venv path từ `SRC_DIR/.app_venv` (ghi bởi `install.sh`) — không cần truyền `--venv` lại khi upgrade.

---

## 9. Cài đặt offline

Dùng khi server không có kết nối internet. Wheel của các dependencies được chuẩn bị sẵn trên máy có internet rồi upload cùng source code.

### Bước 1 — Chuẩn bị wheels trên máy có internet

```bash
cd /path/to/mediapipe  # máy local, cùng OS/arch với server

# Download dependencies khớp Python 3.12 + RHEL 8 (manylinux)
pip3.12 download -d wheels/ \
    "mediapipe==0.10.35" \
    "grpcio==1.81.1" \
    "grpcio-tools==1.81.1" \
    "protobuf==6.33.6" \
    "numpy==2.4.6" \
    "Pillow==12.3.0" \
    "PyYAML>=6.0" \
    "ruamel.yaml>=0.19" \
    "prometheus-client>=0.20"
```

> **Lưu ý:** wheel `mediapipe_video` **không** download từ PyPI — được build trực tiếp trên server ở bước 3.

### Bước 2 — Upload toàn bộ SRC_DIR (gồm wheels/) lên server

```bash
tar -czf mediapipe.tar.gz \
    --exclude='.venv' \
    --exclude='__pycache__' \
    --exclude='*.egg-info' \
    .
scp mediapipe.tar.gz user@server:/u01/ims/namcht/

# Trên server — giải nén
mkdir -p /u01/ims/namcht/mediapipe
tar -xzf /u01/ims/namcht/mediapipe.tar.gz -C /u01/ims/namcht/mediapipe
```

### Bước 3 — Build wheel mediapipe_video trên server

```bash
cd /u01/ims/namcht/mediapipe
python3.12 -m pip wheel . -w wheels/ --no-deps
```

Sau bước này `wheels/` có đủ 9 files (8 dependencies + 1 app wheel).

### Bước 4 — Chạy check rồi cài đặt

```bash
# Kiểm tra môi trường
bash scripts/check.sh --mode install --delegate gpu --offline

# Cài đặt (dùng venv có sẵn nếu cần --venv)
bash scripts/install.sh --offline

# Nâng cấp lần sau
bash scripts/upgrade.sh --offline
```

---

## 10. Theo dõi log

Logs nằm tại `APP_DIR/logs/`:

| File | Nội dung |
|---|---|
| `mediapipe-video.log` | JSON structured log — toàn bộ hoạt động |
| `mediapipe-video.log.YYYY-MM-DD` | Log các ngày trước (rotate lúc 00:00 UTC) |
| `stdout.log` | stdout/stderr raw của process (bắt lỗi crash sớm) |
| `mediapipe-video.pid` | PID của process đang chạy |

### Xem log realtime

```bash
tail -f /u01/ims/mediapipe_video/logs/mediapipe-video.log
```

### Lọc theo level

```bash
# Chỉ xem ERROR
grep '"level":"ERROR"' /u01/ims/mediapipe_video/logs/mediapipe-video.log

# Chỉ xem inference
grep '"event": "inference"' /u01/ims/mediapipe_video/logs/mediapipe-video.log
```

### Format log

Mỗi dòng là một JSON object:

```json
{"timestamp":"2026-09-25T04:44:07.588981+00:00","level":"INFO","logger":"mediapipe_video.segmenter","source":"segmenter.py:138","message":"inference  session=abc123  frame=5  jpeg=18432B  fg=12800/40960px  latency=23.4ms"}
```

### Thay đổi log level không cần restart

Chỉnh `logging.level` trong `APP_DIR/etc/config.yaml`, sau đó restart ứng dụng:

```bash
# Đổi sang DEBUG để debug
sed -i 's/level: "INFO"/level: "DEBUG"/' /u01/ims/mediapipe_video/etc/config.yaml
/u01/ims/mediapipe_video/scripts/stop.sh
/u01/ims/mediapipe_video/scripts/start.sh
```

---

## 11. Metrics (Prometheus)

Ứng dụng expose Prometheus metrics tại `http://<server>:9101/metrics`.

### Bật / tắt trong config

```yaml
metrics:
  enabled: true   # false = không start HTTP server
  port: 9101
```

### Danh sách metrics

| Metric | Loại | Mô tả |
|--------|------|-------|
| `mediapipe_video_active_streams` | Gauge | Số stream gRPC đang xử lý |
| `mediapipe_video_streams_total` | Counter | Tổng stream đã hoàn thành |
| `mediapipe_video_active_segmenters` | Gauge | Số ImageSegmenter đang chạy (quan trọng với GPU EGL limit) |
| `mediapipe_video_frames_total{status="ok"}` | Counter | Frame xử lý thành công |
| `mediapipe_video_frames_total{status="error"}` | Counter | Frame lỗi (JPEG lỗi, dimension mismatch, …) |
| `mediapipe_video_frame_drops_total` | Counter | Frame bị MediaPipe LIVE_STREAM drop |
| `mediapipe_video_inference_latency_seconds` | Histogram | Latency mỗi frame (decode + inference + RLE) |
| `mediapipe_video_frame_bytes` | Histogram | Kích thước JPEG đầu vào (bytes) |
| `mediapipe_video_foreground_ratio` | Histogram | Tỉ lệ pixel foreground mỗi frame [0–1] |

### Kiểm tra thủ công

```bash
curl http://localhost:9101/metrics
```

```
# HELP mediapipe_video_active_streams Number of currently active gRPC streams
# TYPE mediapipe_video_active_streams gauge
mediapipe_video_active_streams 2.0

# HELP mediapipe_video_frames_total Total frames processed
# TYPE mediapipe_video_frames_total counter
mediapipe_video_frames_total{status="ok"} 1284.0
mediapipe_video_frames_total{status="error"} 3.0

# HELP mediapipe_video_inference_latency_seconds Per-frame latency
# TYPE mediapipe_video_inference_latency_seconds histogram
mediapipe_video_inference_latency_seconds_bucket{le="0.01"} 0.0
mediapipe_video_inference_latency_seconds_bucket{le="0.025"} 412.0
mediapipe_video_inference_latency_seconds_bucket{le="0.05"} 1198.0
mediapipe_video_inference_latency_seconds_bucket{le="0.1"} 1280.0
mediapipe_video_inference_latency_seconds_bucket{le="+Inf"} 1284.0
mediapipe_video_inference_latency_seconds_sum 32.741
mediapipe_video_inference_latency_seconds_count 1284.0
```

### Prometheus scrape config

```yaml
# prometheus.yml
scrape_configs:
  - job_name: mediapipe-video
    static_configs:
      - targets: ["<server-ip>:9101"]
    scrape_interval: 15s
```

### Grafana — các query hữu ích

```promql
# Throughput (frame/s)
rate(mediapipe_video_frames_total{status="ok"}[1m])

# Error rate
rate(mediapipe_video_frames_total{status="error"}[1m])
  / rate(mediapipe_video_frames_total[1m])

# P50 / P95 / P99 latency
histogram_quantile(0.50, rate(mediapipe_video_inference_latency_seconds_bucket[5m]))
histogram_quantile(0.95, rate(mediapipe_video_inference_latency_seconds_bucket[5m]))
histogram_quantile(0.99, rate(mediapipe_video_inference_latency_seconds_bucket[5m]))

# Frame drop rate
rate(mediapipe_video_frame_drops_total[1m])

# Active GPU segmenters (monitor EGL context pressure)
mediapipe_video_active_segmenters
```

### Periodic log snapshot

Ngoài HTTP `/metrics`, ứng dụng còn **in định kỳ** snapshot metric ra file log. Cấu hình:

```yaml
metrics:
  log_interval_secs: 60   # 0 = tắt
```

Mỗi dòng log có dạng:

```json
{
  "timestamp": "2026-09-26T08:01:00Z",
  "level": "INFO",
  "logger": "mediapipe_video.metrics",
  "source": "metrics.py:...",
  "message": "metrics_snapshot  interval=60s  active_streams=2  active_segmenters=2  streams=45(+3)  frames_ok=1284(+240)  frames_error=3(+0)  frame_drops=12(+2)  err_rate=0.0%  drop_rate=0.8%  latency_p50=23.1ms  latency_p95=45.2ms  latency_p99=89.1ms  latency_avg=25.4ms  frame_bytes_avg=45678B  foreground_ratio_avg=0.342"
}
```

Giải thích các trường:
- `streams=45(+3)`: tổng 45 streams, interval vừa rồi có thêm 3
- `frames_ok=1284(+240)`: tổng 1284 frame thành công, interval vừa rồi +240
- `latency_p50/p95/p99`: **tính trong interval** (bucket delta), không phải từ đầu server
- `err_rate` / `drop_rate`: tỉ lệ trong interval

Lọc nhanh từ log file:

```bash
grep "metrics_snapshot" /u01/ims/mediapipe_video/logs/mediapipe-video.log | tail -5
```

### Offline — thêm wheel vào wheels/

`prometheus-client` cần được download cùng với các dependency khác:

```bash
pip download prometheus-client>=0.20 -d wheels/
```

---

## 12. Xử lý lỗi thường gặp

### Lỗi: `model file not found`

```
[ERROR] model file not found: /u01/ims/mediapipe_video/models/selfie_segmenter.tflite
```

**Xử lý:** Copy model vào đúng đường dẫn trong config:

```bash
cp selfie_segmenter.tflite /u01/ims/mediapipe_video/models/
```

---

### Lỗi: `Cannot initialize MediaPipe LIVE_STREAM (gpu)`

```
Cannot initialize MediaPipe LIVE_STREAM (gpu): ... GPU requires a MediaPipe build with GPU support...
```

**Xử lý:** Chuyển sang CPU hoặc kiểm tra driver:

```bash
# Chuyển delegate sang CPU
sed -i 's/delegate: "gpu"/delegate: "cpu"/' /u01/ims/mediapipe_video/etc/config.yaml
/u01/ims/mediapipe_video/scripts/start.sh

# Hoặc kiểm tra GPU driver
nvidia-smi
```

---

### Lỗi: `Already running (PID xxxx)`

```
[INFO] Already running (PID 12345) — nothing to do
```

**Xử lý:** Dừng trước rồi khởi động lại:

```bash
/u01/ims/mediapipe_video/scripts/stop.sh
/u01/ims/mediapipe_video/scripts/start.sh
```

---

### Lỗi: `venv not found — run install.sh first`

```
[ERROR] venv not found: /u01/ims/mediapipe_video/libs/venv — run install.sh first
```

**Xử lý:** Chưa cài đặt hoặc APP_DIR sai:

```bash
bash /u01/ims/namcht/mediapipe/scripts/install.sh
```

---

### Lỗi: `Python 3.10+ not found in PATH`

**Xử lý trên RHEL 8:**

```bash
# Bật AppStream module Python 3.11
sudo dnf module enable python311 -y
sudo dnf install python3.11 -y

# Kiểm tra
python3.11 --version
```

**Xử lý trên Ubuntu/Debian:**

```bash
sudo apt install python3.11 python3.11-venv
```

**Hoặc chỉ định đường dẫn trực tiếp:**

```bash
PATH=/usr/bin:$PATH bash scripts/install.sh
```

---

### Lỗi: `[FAIL] libGL.so.1 không tìm thấy` hoặc `libEGL.so không tìm thấy`

Thường gặp trên RHEL 8 headless (server không có màn hình).

**Nếu đã cài NVIDIA driver** — driver đã cung cấp `libGL` và `libEGL`, nhưng ldconfig chưa cập nhật:

```bash
sudo ldconfig
bash scripts/check.sh --mode install --delegate gpu
```

**Nếu chưa cài NVIDIA driver** — cài mesa fallback:

```bash
sudo dnf install mesa-libGL mesa-libEGL -y
sudo ldconfig
```

**Cài NVIDIA driver trên RHEL 8:**

```bash
# Thêm CUDA repo
sudo dnf config-manager --add-repo \
    https://developer.download.nvidia.com/compute/cuda/repos/rhel8/x86_64/cuda-rhel8.repo
sudo dnf module disable nouveau -y
sudo dnf install cuda-drivers -y
sudo reboot
```

Sau khi reboot kiểm tra:

```bash
nvidia-smi
```

---

### Lỗi: `libxcb.so.1: cannot open shared object file`

```
ImportError: libxcb.so.1: cannot open shared object file: No such file or directory
```

OpenCV (cv2) cần các thư viện X11 — thường thiếu trên server headless RHEL 8.

```bash
sudo dnf install -y libxcb libX11 libXext libSM libXrender libGL
sudo ldconfig
```

---

### Lỗi: `RuntimeError: cannot compile video.proto`

```
Could not make proto path relative: .../mediapipe_video/video.proto: No such file or directory
RuntimeError: cannot compile video.proto
```

File `video.proto` chưa được copy vào package sau khi install. `install.sh` và `upgrade.sh` tự động xử lý bước này, nhưng nếu cài thủ công bằng `pip install` thì cần copy thêm:

```bash
PKG_DIR=$(/opt/nemotron-venv/bin/python -c \
    "import mediapipe_video, os; print(os.path.dirname(mediapipe_video.__file__))")

cp /u01/ims/namcht/mediapipe/src/proto/video.proto "$PKG_DIR/video.proto"

# Kiểm tra
/opt/nemotron-venv/bin/python -c "from mediapipe_video.contract import pb; print('OK')"
```

---

### Lỗi: `RESOURCE_EXHAUSTED: max concurrent GPU segmenters reached`

Đạt giới hạn EGL context của GPU driver — quá nhiều stream đồng thời.

**Xử lý:** Tăng `max_concurrent_segmenters` hoặc giảm `max_streams`:

```bash
# Xem số stream đang chạy
grep "inference" /u01/ims/mediapipe_video/logs/mediapipe-video.log | tail -5

# Giảm giới hạn GPU contexts (GPU driver limit thường 4–8)
sed -i 's/max_concurrent_segmenters: .*/max_concurrent_segmenters: 4/' \
    /u01/ims/mediapipe_video/etc/config.yaml

/u01/ims/mediapipe_video/scripts/stop.sh
/u01/ims/mediapipe_video/scripts/start.sh
```

---

### Process crash ngay sau khi start

Kiểm tra stdout.log để xem traceback:

```bash
cat /u01/ims/mediapipe_video/logs/stdout.log
```
