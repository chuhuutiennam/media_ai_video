# Triển khai và theo dõi dịch vụ gesture bằng Bash

Các script tại `workers/gesture/scripts` quản lý trực tiếp bằng Bash, theo cách
triển khai segmentation. Mỗi script tự chứa các hàm cần thiết, chạy độc lập,
không source `_common.sh` hoặc gọi script khác để thực hiện thao tác. Worker dùng entry point
`VENV/bin/mediapipe-gesture-server`. Python của venv chỉ dùng cho worker và các
thao tác đọc/ghi YAML, kiểm tra dependency hoặc bind cổng; stop/status của instance
mới dùng Bash để kiểm tra `/proc`, PID, gửi signal và thăm dò TCP.

| Script | Chức năng |
| --- | --- |
| `install.sh` | Kiểm tra source/Python/checkpoint, tạo venv/layout, cài package và scripts Bash. |
| `check.sh` | Preflight `--mode install`, `runtime`, `upgrade`, `all`; mặc định runtime. |
| `start.sh` | Kiểm tra model/config/cổng, chạy nền, chờ readiness; chỉ báo Ready khi worker còn sống và mở cổng. |
| `status.sh` | In PID/endpoint; exit 0 khi ready, 3 khi dừng, 1 khi lỗi. |
| `stop.sh` | Kiểm tra PID + thời điểm tạo process rồi SIGTERM; giữ metadata nếu shutdown timeout. |
| `restart.sh` | Stop và start lại trong cùng khóa quản lý dịch vụ. |
| `upgrade.sh` | Dừng instance đang chạy, cập nhật package/scripts, giữ config/model và restart nếu trước đó đang chạy. |

Bash/Linux cần `realpath`, `flock`, `nohup`, `timeout`; worker cần Python 3.11+ và
module venv/pip. Các script hỗ trợ `--help`, `--app-dir`, `--venv`, `--dry-run`, `--verbose`.
Trên WSL nên đặt APP_DIR/venv trên filesystem Linux để tránh I/O 9p của `/mnt/c`.

## Cài mới

Từ repository root:

```bash
GESTURE_APP_DIR="$HOME/services/mediapipe_gesture"
bash workers/gesture/scripts/check.sh --mode install
bash workers/gesture/scripts/install.sh \
  --app-dir "$GESTURE_APP_DIR" --delegate cpu \
  --addr 127.0.0.1:50053 --metrics-port 9102

bash "$GESTURE_APP_DIR/scripts/start.sh"
bash "$GESTURE_APP_DIR/scripts/status.sh"
bash "$GESTURE_APP_DIR/scripts/check.sh" --mode runtime
```

Mặc định APP_DIR là `/u01/ims/mediapipe_gesture` hoặc source `.app_dir` của lần
cài trước; script trong APP_DIR tự nhận vị trí deployment. `--python python3.12`
chọn interpreter để tạo venv; `PYTHON=python3.12 bash ...` chọn Python dùng cho
các kiểm tra dữ liệu. Có thể dùng `--venv DIR` thay venv mặc định, nhưng nếu chia
sẻ venv với dịch vụ khác thì pip có thể cập nhật dependencies dùng chung.

Installer tìm `workers/checkpoint/gesture_recognizer.task`, fallback
`SRC_DIR/checkpoint/gesture_recognizer.task`; dùng `--model FILE` nếu model ở nơi khác.
Model được copy vào APP_DIR và config mới dùng đường dẫn tuyệt đối. Config/model
đã tồn tại được giữ nguyên, không bị thay bằng giá trị truyền lại qua flags.
Muốn đổi endpoint/delegate/model đang vận hành, sửa YAML rồi restart.

```text
APP_DIR/
├── bin/mediapipe-gesture-server
├── etc/config.yaml
├── libs/venv/
├── models/gesture_recognizer.task
├── logs/
│   ├── service.log                   # thao tác Bash: check/start/stop/install/upgrade
│   ├── stdout.log                    # stdout/stderr worker, cả log native MediaPipe
│   └── mediapipe-gesture.log          # JSON Python, rotate theo ngày UTC
├── run/
│   ├── mediapipe-gesture.pid
│   ├── mediapipe-gesture.start        # start time từ /proc, tránh PID tái sử dụng
│   ├── mediapipe-gesture.addr
│   ├── mediapipe-gesture.json         # giữ tương thích metadata phiên bản trước
│   └── service.lock
├── scripts/                         # install/check/start/stop/restart/status/upgrade
└── .app_venv
```

## Cập nhật deployment đang dùng manage.py

Chạy script từ source mới; không chỉ copy riêng `start.sh`:

```bash
bash workers/gesture/scripts/upgrade.sh --app-dir "$PWD/run/gesture"
```

Thay APP_DIR bằng vị trí cài thực tế. Upgrade đọc được metadata JSON của instance
cũ để dừng đúng process, cài package mới có logging mới, copy toàn bộ `.sh` và
xóa `manage.py` và `_common.sh` cũ trong APP_DIR. Sau đó start bằng Bash. Các service đang
chạy không tự thay code chỉ vì source đã được sửa.

Upgrade không restart nếu service trước đó đã dừng; `--no-restart` để cập nhật
và để service dừng. `--dry-run` không dừng process, không chạy pip hoặc ghi file.
Khi gọi script đã deploy, truyền `--src-dir /path/to/workers/gesture` để chỉ source
mới. Upgrade có downtime, không tự drain registry/call hoặc rollback khi pip lỗi.

## Theo dõi log

```bash
tail -f "$GESTURE_APP_DIR/logs/service.log"
tail -f "$GESTURE_APP_DIR/logs/stdout.log"
tail -f "$GESTURE_APP_DIR/logs/mediapipe-gesture.log"
```

Script Bash in timestamp UTC và mức INFO/OK/WARN/ERROR; không thêm nhãn tên
script như `[start.sh]`. Log mặc
định thể hiện các bước kiểm tra → khởi động → chờ gRPC → Ready, kèm PID/endpoint
và lệnh xem trạng thái/log. Trong lúc khởi động lâu, script báo tiến trình mỗi
5 giây. Đường dẫn config/venv/model, giới hạn runtime và câu lệnh nội bộ chỉ hiện
khi thêm `--verbose`; `--dry-run` vẫn in đầy đủ các lệnh dự kiến.

```bash
bash "$GESTURE_APP_DIR/scripts/start.sh" --verbose
```

Console/stdout của worker dùng text dễ đọc; file `mediapipe-gesture.log` vẫn là
JSON có source và traceback để tra cứu lỗi. Mức INFO giữ các mốc khởi động,
sự kiện gesture, mở/đóng stream và thống kê định kỳ. Toàn bộ dữ liệu bàn tay theo
frame và cấu hình startup chi tiết chuyển sang DEBUG. Khi cần chẩn đoán inference,
đặt `logging.level: DEBUG` trong YAML rồi restart; `--verbose` chỉ điều khiển script.

Nếu startup thất bại, script in phần cuối stdout, trả exit code khác 0 và dọn
PID của child đã thoát. Log `service.log` và `stdout.log` append qua các lần chạy;
cấu hình logrotate cho hai file này khi vận hành dài ngày.

Config logging và thống kê giống cách dùng của segmentation:

```yaml
logging:
  level: INFO
  file: logs/mediapipe-gesture.log
  backup_days: 30
metrics:
  enabled: true
  port: 9102
  log_interval_secs: 60
```

File log tương đối được tính từ working directory; các script chạy worker tại
APP_DIR. Installer mới ghi đường dẫn log tuyệt đối. Config cũ chỉ có `level` vẫn
nhận default file `logs/mediapipe-gesture.log`, rotation 30 ngày và interval 60 giây
sau khi nâng cấp package. `logging.file: null` chỉ ghi console; `log_interval_secs: 0`
tắt snapshot định kỳ, độc lập với HTTP metrics.

Mỗi dòng file JSON có `timestamp`, `level`, `logger`, `source`, `message`; lỗi có
traceback thêm trường `exception`, không bị mất khi format JSON. Log native C++/
EGL và lỗi import trước khi logging khởi tạo nằm trong `stdout.log`.

| Message | Dữ liệu theo dõi |
| --- | --- |
| `startup_config` (DEBUG) | PID, Python/MediaPipe version, config, model/delegate, ports, capacity, max hands, timeout, static threshold và Wave. |
| `Initializing gesture model`, `Gesture model initialized`, `gesture server ready` | Phân biệt đang load model với đã sẵn sàng nhận gRPC. |
| `stream_open` | Session/stream, media/recognizer epoch, FPS và max hands của RPC thực tế. |
| `hand_observation` (DEBUG) | Track ID, raw/stable label, score, center/bbox normalized/pixel; không ghi 21 landmarks. |
| `gesture_event` | Frame/time, track, gesture tĩnh/Wave, emoji và score. |
| `frame_processed` (DEBUG) | Frame identity, số tay/event và latency. Bật `logging.level: DEBUG` khi cần điều tra frame. |
| `stream_invalid`, `stream_rejected`, `stream_timeout` | Sai contract/JPEG, hết capacity hoặc callback timeout, kèm session/stream và nguyên nhân. |
| `stream_closed` | Outcome, số frame xử lý và thời gian sống của stream. |
| `metrics_snapshot` | Active RPC/recognizer, tổng stream/frame/event, errors theo reason, latency trung bình ms. |
| `startup_failed`, `server_shutdown`, `server_stopped` | Lỗi cấu hình/model/bind, signal/grace và thống kê cuối khi shutdown. |

Latency trong snapshot là số đo xử lý của worker, không phải latency end-to-end
qua RTPGW và MF.

## Lỗi thường gặp

| Thông báo | Xử lý |
| --- | --- |
| Worker/venv missing | Chạy install hoặc kiểm tra `--venv`/APP_DIR `.app_venv`. |
| Set gesture.model / checkpoint missing | Sửa `gesture.model` trong config tới file `.task` thực tế; `--model` chỉ dùng lúc cài mới. |
| Port occupied / conflicting metrics port | Kiểm tra `ss -ltnp`; dùng hai cổng khác nhau cho gRPC và metrics. Không tự kill process đang giữ cổng. |
| Worker exited / startup_failed | Xem stdout và JSON `exception`; kiểm tra checkpoint, native dependencies và delegate. GPU phải có EGL/OpenGL phù hợp. |
| Startup timeout | Xem `model_probe_start/ok`; kiểm tra I/O/model init. Dùng `--timeout SECONDS` nếu init thực tế cần lâu hơn, không bỏ qua lỗi model. |
| stream_timeout | Xem timestamp/frame, tải worker và `gesture.result_timeout_secs`; không biến timeout thành snapshot rỗng. |
| Shutdown timeout | Metadata được giữ để tiếp tục quản lý. Chỉ dùng `stop.sh --force` khi chấp nhận SIGKILL. |
| Missing/invalid PID start time | Không gửi signal tới PID chưa xác minh; kiểm tra metadata và process thực tế trước khi xử lý. |
| Empty/relative marker | Sửa `.app_dir`/`.app_venv` thành đường dẫn tuyệt đối hoặc truyền `--app-dir`/`--venv`. |

## Cổng và tích hợp

Mặc định gesture gRPC `127.0.0.1:50053`, metrics HTTP `:9102/metrics`. Để nhận từ
host khác, cấu hình `server.addr: 0.0.0.0:50053` hoặc IP phù hợp; registry gateway
phải dùng IP/hostname thực. Dịch vụ bind cổng; firewall/network do deployment quản lý.

RTPGW gửi JPEG và nhận hands/reactions trên cùng RPC `/video.VideoStream/Segment`.
Worker không mở HTTP callback riêng tới MF; RTPGW gửi callback mask/gesture tới MF.
Contract vẫn là `internal/ai/proto/video.proto`, không thay transport/routing.
Ví dụ registry đầy đủ: [gateway-video-external.yaml](../../../config/gateway-video-external.yaml).
Mặc định 8 RPC nhưng 4 recognizer, mỗi call hai leg dùng hai recognizer; quảng bá
capacity worker phù hợp. Segmentation dùng process/venv/PID/cổng riêng.

## Offline và foreground

```bash
python3.12 -m pip wheel ./workers/gesture --wheel-dir /tmp/gesture-wheels
python3.12 -m pip download 'setuptools>=65' wheel --dest /tmp/gesture-wheels
bash workers/gesture/scripts/install.sh \
  --app-dir "$GESTURE_APP_DIR" --offline --wheels-dir /tmp/gesture-wheels

bash "$GESTURE_APP_DIR/scripts/start.sh" --foreground
```

Wheelhouse cần cùng Python/ABI/OS/kiến trúc và đủ dependencies/build tools. Offline
không dùng index/build isolation. Foreground exec trực tiếp worker; supervisor
như systemd/container sở hữu lifecycle và log stdout/stderr, không tạo PID nền.
Dừng foreground bằng supervisor/SIGTERM, không dùng stop.sh của background instance.

## Kiểm thử

```bash
python3 workers/gesture/tests/test_deployment.py -v
bash scripts/test-video-external-e2e.sh \
  --ai-target 127.0.0.1:50052 --gesture-target 127.0.0.1:50053 \
  --input data/gesture_sample/static_gesture.mp4 \
  --width 640 --height 360 --duration 6 --require-gesture \
  --artifacts-dir /tmp/video-external
```

Test lifecycle chạy trực tiếp `.sh` với worker TCP giả lập tại thư mục/cổng tạm,
bao gồm lỗi, metadata JSON cũ, bảo toàn config/model, dry-run/offline và force-stop.
Test log kiểm tra JSON exception/rotation, config và metrics snapshot. Native smoke
riêng xác minh JSON log, JPEG Thumb_Up, lỗi JPEG và HTTP metrics với model thật.
Xem [server gRPC](grpc.md) và [E2E hai endpoint](../../../docs/video-external-e2e.md).

Xác minh ngày 2026-10-27: 55 tests PASS, 3 native tests opt-in skip; trong đó 15
tests chạy trực tiếp script Bash. Native smoke riêng nhận Thumb_Up, từ chối JPEG
hỏng với INVALID_ARGUMENT, HTTP metrics và các message JSON/lifecycle/stream đạt.
Artifacts tại `/tmp/gesture-bash-native-bb_vdoh8`, log xác minh
`verification-resume.log`. Pip được stub trong smoke layout này để không thay đổi
dependencies của deployment đang chạy; worker/model và gRPC là thật.

Lượt restart đầu dùng dependency venv trên `/mnt/c` hết thời gian chờ 120 giây;
script đã báo lỗi và dọn process. Lượt xác minh tiếp dùng allowance startup 300
giây đạt start/stop/JSON checks. Đây là kiểm chứng chức năng, không phải benchmark
startup hay tải nhiều call. Venv/model trên filesystem Linux là lựa chọn triển khai
khuyến nghị; chỉ tăng timeout sau khi kiểm tra log và nguyên nhân init chậm.
