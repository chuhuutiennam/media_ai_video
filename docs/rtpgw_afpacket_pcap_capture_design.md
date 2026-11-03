# Thiết kế tích hợp PCAP Capture vào RTPGW bằng Golang

## 1. Mục tiêu

Tích hợp khả năng capture packet trực tiếp trong RTPGW để ghi ra file `.pcap`, phục vụ debug và phân tích RTP bằng Wireshark.

Giải pháp cần đảm bảo:

- Capture packet trước khi Linux kernel bóc Ethernet/IP/UDP.
- Giữ nguyên đầy đủ packet:
  - Ethernet
  - IP
  - UDP
  - RTP
  - RTP Payload
- Không thay đổi luồng xử lý RTP hiện tại của RTPGW.
- Không để việc ghi PCAP làm block hoặc tăng jitter cho media path.
- Có thể bật/tắt bằng cấu hình.
- Có thể giới hạn packet cần capture theo interface, port hoặc flow.
- Có cơ chế rotate file để tránh đầy disk.
- Có metric theo dõi packet capture bị drop.

---

## 2. Giải pháp đề xuất

Sử dụng Linux `AF_PACKET` thông qua thư viện:

```text
github.com/google/gopacket/afpacket
github.com/google/gopacket/pcapgo
```

`AF_PACKET` cho phép RTPGW đọc một bản copy của packet tại tầng Layer 2 trước khi packet được Linux network stack xử lý.

Luồng RTP chính vẫn sử dụng UDP socket bình thường.

```text
                         Linux Network Stack
                                │
              ┌─────────────────┴─────────────────┐
              │                                   │
              ▼                                   ▼
        AF_PACKET capture                    UDP socket
              │                                   │
              ▼                                   ▼
        PCAP Recorder                       RTP Ingress
              │                                   │
              ▼                                   ▼
          *.pcap                           pion/rtp.Unmarshal()
                                                  │
                                                  ▼
                                             RTP Processing
                                                  │
                                                  ▼
                                             UDP WriteTo()
```

---

## 3. Packet được capture ở đâu?

Khi packet RTP đi vào interface:

```text
192.168.10.20:30000
        │
        │ UDP/RTP
        ▼
      eth0
        │
        ├──────────── AF_PACKET ────────────> PCAP Capture
        │
        ▼
   Linux IP/UDP Stack
        │
        ▼
   UDP Socket Buffer
        │
        ▼
   ReadFromUDP()
        │
        ▼
      RTPGW
```

`AF_PACKET` không consume packet khỏi network stack.

Packet vẫn tiếp tục được kernel xử lý và chuyển vào UDP socket của RTPGW.

Như vậy cùng một packet sẽ được sử dụng theo hai mục đích:

```text
                    RTP Packet
                        │
           ┌────────────┴────────────┐
           │                         │
           ▼                         ▼
      AF_PACKET                 UDP Socket
           │                         │
           ▼                         ▼
       PCAP file              RTPGW Processing
```

---

## 4. So sánh UDP socket và AF_PACKET

### UDP Socket

RTPGW hiện tại thường nhận packet bằng:

```go
n, addr, err := conn.ReadFromUDP(buf)
```

Dữ liệu nhận được chỉ còn:

```text
[RTP Header][RTP Payload]
```

Kernel đã loại bỏ:

```text
Ethernet Header
IP Header
UDP Header
```

### AF_PACKET

AF_PACKET nhận toàn bộ packet:

```text
Ethernet
  │
IPv4/IPv6
  │
UDP
  │
RTP
  │
Payload
```

Do đó file PCAP có thể được mở trực tiếp bằng Wireshark để phân tích đầy đủ network và RTP.

---

## 5. Kiến trúc module đề xuất

Có thể bổ sung module riêng trong RTPGW:

```text
rtpgw/
├── cmd/
├── config/
├── ingress/
├── egress/
├── session/
├── metrics/
└── debug/
    └── pcap/
        ├── recorder.go
        ├── writer.go
        ├── filter.go
        ├── rotate.go
        └── config.go
```

Trách nhiệm:

| File | Chức năng |
|---|---|
| `recorder.go` | Capture packet từ AF_PACKET |
| `writer.go` | Ghi packet ra PCAP |
| `filter.go` | Filter UDP/RTP flow |
| `rotate.go` | Rotate PCAP file |
| `config.go` | Config PCAP capture |

---

## 6. Luồng xử lý đề xuất

Không ghi file đồng bộ trực tiếp trong media path.

### Không nên

```text
Packet
  │
  ▼
Write PCAP file
  │
  ▼
Process RTP
```

Disk I/O có thể làm block goroutine xử lý RTP và gây jitter.

### Nên

```text
                        Packet
                          │
             ┌────────────┴────────────┐
             │                         │
             ▼                         ▼
       RTP Processing             PCAP Capture
                                       │
                                       ▼
                                 Bounded Queue
                                       │
                                       ▼
                                Writer Goroutine
                                       │
                                       ▼
                                   *.pcap
```

RTP processing và PCAP writer phải độc lập.

Nếu queue capture đầy:

```text
DROP PCAP packet
```

Không được block media path.

---

## 7. Cấu trúc dữ liệu

`capturedPacket` là unit truyền giữa capture loop và writer goroutine (unexported):

```go
type capturedPacket struct {
    ci   gopacket.CaptureInfo // Timestamp, CaptureLength, Length
    data []byte               // bản copy của ring buffer (ZeroCopy → phải copy trước khi enqueue)
}
```

`Recorder` quản lý hai AF_PACKET socket độc lập và hai queue riêng cho ingress/egress:

```go
type Recorder struct {
    cfg Config

    ingressTP *afpacket.TPacket   // socket AF_PACKET cho hướng vào (BPF: udp dst portrange)
    egressTP  *afpacket.TPacket   // socket AF_PACKET cho hướng ra (BPF: udp src portrange)

    ingressQ chan capturedPacket
    egressQ  chan capturedPacket

    stopCh chan struct{}
    wg     sync.WaitGroup

    packets   atomic.Uint64 // tổng packet đã ghi (cả ingress + egress)
    bytes     atomic.Uint64 // tổng bytes payload đã ghi
    dropped   atomic.Uint64 // packet bị drop do queue đầy
    writeErrs atomic.Uint64 // lỗi ghi file PCAP
}
```

`pcapWriter` xử lý file I/O trong goroutine riêng (unexported):

```go
type pcapWriter struct {
    cfg     Config
    prefix  string             // "ingress" hoặc "egress" — dùng trong tên file
    queue   <-chan capturedPacket

    packets   *atomic.Uint64  // con trỏ đến counter của Recorder (shared)
    bytes     *atomic.Uint64
    writeErrs *atomic.Uint64

    f       *os.File
    pw      *pcapgo.Writer
    size    int64              // bytes đã ghi vào file hiện tại (dùng để trigger rotate)
    fileNum int                // số thứ tự file trong tên (tăng dần mỗi lần rotate)
}
```

---

## 8. Khởi tạo AF_PACKET

`New()` tạo **hai** TPacket socket — một cho ingress, một cho egress — rồi attach BPF filter vào mỗi socket:

```go
// ingress socket: BPF "udp dst portrange PortMin-PortMax"
ingressTP, err := afpacket.NewTPacket(
    afpacket.OptInterface(cfg.Interface),
    afpacket.OptFrameSize(65536),    // đủ cho max UDP frame (1 frame/packet)
    afpacket.OptBlockSize(1<<23),    // 8 MiB ring buffer
    afpacket.OptNumBlocks(1),        // 1 block × 8 MiB
    afpacket.OptPollTimeout(100*time.Millisecond),
)
ingressTP.SetBPF(udpDstPortRangeBPF(cfg.PortMin, cfg.PortMax))

// egress socket: BPF "udp src portrange PortMin-PortMax"
egressTP, err := afpacket.NewTPacket( /* same options */ )
egressTP.SetBPF(udpSrcPortRangeBPF(cfg.PortMin, cfg.PortMax))
```

Giá trị các tham số và lý do chọn:

| Tham số | Giá trị | Lý do |
|---|---|---|
| `FrameSize` | 65536 | Chứa được toàn bộ Ethernet frame lớn nhất (jumbo frame) |
| `BlockSize` | 8 MiB (1<<23) | Ring buffer đủ lớn để absorb burst RTP mà không drop tại kernel |
| `NumBlocks` | 1 | Một block duy nhất; `BlockSize` = tổng ring buffer |
| `PollTimeout` | 100 ms | `ZeroCopyReadPacketData()` block tối đa 100ms khi không có packet; đủ nhanh để detect `stopCh` |

`PollTimeout` phải đủ ngắn: nếu quá dài, `Stop()` phải chờ timeout mới thoát capture loop.

---

## 9. Tạo file PCAP

Khởi tạo writer:

```go
f, err := os.Create("/var/log/rtpgw/pcap/rtpgw.pcap")
if err != nil {
    return err
}

writer := pcapgo.NewWriter(f)

err = writer.WriteFileHeader(
    65535,
    layers.LinkTypeEthernet,
)
```

Sau đó ghi từng packet:

```go
err := writer.WritePacket(ci, data)
```

Trong đó:

- `ci.Timestamp`: thời điểm packet được capture.
- `ci.CaptureLength`: số byte thực tế ghi.
- `ci.Length`: kích thước packet gốc.
- `data`: raw Ethernet frame.

---

## 10. Recorder loop

Ví dụ:

```go
func (r *Recorder) Run() {
    for {
        select {
        case <-r.stopCh:
            return

        default:
            data, ci, err := r.tp.ZeroCopyReadPacketData()
            if err != nil {
                continue
            }

            pkt := CapturedPacket{
                Timestamp:     ci.Timestamp,
                CaptureLength: ci.CaptureLength,
                Length:        ci.Length,
                Data:          append([]byte(nil), data...),
            }

            select {
            case r.queue <- pkt:

            default:
                r.dropped.Add(1)
            }
        }
    }
}
```

Lưu ý:

`ZeroCopyReadPacketData()` có thể trả về vùng nhớ được tái sử dụng ở lần đọc tiếp theo.

Nếu packet được đẩy sang writer goroutine thì phải copy dữ liệu:

```go
append([]byte(nil), data...)
```

Nếu không copy, writer có thể ghi dữ liệu đã bị overwrite.

---

## 11. Writer goroutine

Ví dụ:

```go
func (w *Writer) Run(queue <-chan CapturedPacket) {
    for pkt := range queue {

        ci := gopacket.CaptureInfo{
            Timestamp:     pkt.Timestamp,
            CaptureLength: pkt.CaptureLength,
            Length:        pkt.Length,
        }

        if err := w.writer.WritePacket(ci, pkt.Data); err != nil {
            // update error metric
        }
    }
}
```

Writer goroutine xử lý file I/O độc lập với packet capture.

---

## 12. Queue

Queue nên là bounded queue:

```go
queue := make(chan CapturedPacket, 8192)
```

Không nên dùng queue không giới hạn vì khi disk chậm hoặc lỗi có thể làm memory tăng liên tục.

Khi queue đầy:

```go
select {
case queue <- pkt:
default:
    dropped.Add(1)
}
```

Nguyên tắc:

```text
PCAP capture được phép drop.
RTP media path không được block.
```

---

## 13. Packet filtering

Không nên capture toàn bộ traffic trên interface trong môi trường production.

Ví dụ RTPGW sử dụng RTP port:

```text
20000 - 40000
```

Filter logic:

```text
udp and portrange 20000-40000
```

Hoặc một session:

```text
src = 10.10.1.20:30002
dst = 10.10.1.30:32004
```

Filter:

```text
udp
and host 10.10.1.20
and host 10.10.1.30
and
(
    port 30002
    or
    port 32004
)
```

---

## 14. BPF với AF_PACKET

Nếu dùng `gopacket/afpacket`, có thể attach BPF filter vào socket.

Mục tiêu là drop packet không cần thiết ngay tại capture path thay vì đọc mọi packet lên userspace.

Luồng:

```text
NIC
 │
 ▼
AF_PACKET
 │
 ▼
BPF Filter
 │
 ├── match ─────────> RTPGW PCAP recorder
 │
 └── no match ──────> drop from capture path
```

BPF chỉ ảnh hưởng socket capture.

Không ảnh hưởng UDP socket RTP thật.

---

## 15. Cấu hình đề xuất

```yaml
pcap:
  enabled: false          # mặc định tắt; chỉ bật khi cần debug
  interface: eth0         # interface cần capture; phải khớp với interface thực tế trên host
  output_dir: /var/log/media-ai-gateway/pcap
  queue_size: 8192        # bounded queue per direction (ingress/egress); packet bị drop khi đầy
  snaplen: 65535          # max bytes ghi per packet; 65535 = toàn bộ packet
  rotate:
    max_size_mb: 100      # rotate khi file vượt kích thước này; 0 = không rotate
    max_files: 10         # giữ tối đa N file per direction; 0 = không xóa file cũ
```

Lưu ý về thiết kế:

- **Port range không cấu hình trong `pcap:`** — tự động lấy từ `rtp.port_start` và `rtp.port_end`.
  Gateway gọi `ToPCAPConfig()` để map sang `pcap.Config.PortMin/PortMax`.
  Tránh duplicate config và đảm bảo BPF filter luôn khớp với port pool thực tế đang dùng.

- **Ingress và egress luôn capture đồng thời** — hai AF_PACKET socket độc lập, ghi ra hai file riêng
  (`ingress-*.pcap` và `egress-*.pcap`). Không có flag bật/tắt từng hướng.

- **Tắt rotate** bằng cách đặt `rotate.max_size_mb: 0` — không cần field `enabled` riêng.

- **Chỉ bật khi troubleshoot** — production mặc định `enabled: false`.
  AF_PACKET yêu cầu `CAP_NET_RAW`; không bật thường trực trên môi trường không cần debug.

---

## 16. Capture ingress và egress

AF_PACKET trên Linux có thể thấy packet theo cả hai hướng trên interface.

Có thể lưu chung:

```text
rtpgw.pcap
```

hoặc tách:

```text
ingress.pcap
egress.pcap
```

Tách ingress/egress giúp dễ so sánh:

```text
MF -> RTPGW
RTPGW -> AI

AI -> RTPGW
RTPGW -> MF
```

Nếu sử dụng chung một file, Wireshark vẫn có thể filter theo:

```text
ip.src
ip.dst
udp.srcport
udp.dstport
rtp.ssrc
```

---

## 17. File rotation

Không nên ghi vô hạn vào một file.

Ví dụ:

```text
rtpgw-20260820-140000-001.pcap
rtpgw-20260820-140000-002.pcap
rtpgw-20260820-140000-003.pcap
```

Điều kiện rotate:

```text
max_size_mb = 100
max_files   = 10
```

Khi vượt `max_files`, xóa file cũ nhất.

Có thể bổ sung rotate theo thời gian:

```text
max_duration = 10 phút
```

---

## 18. Metrics

Nên có các metric:

```text
rtpgw_pcap_packets_total
rtpgw_pcap_bytes_total
rtpgw_pcap_drop_total
rtpgw_pcap_write_error_total
rtpgw_pcap_queue_size
rtpgw_pcap_rotate_total
```

Ví dụ:

```text
rtpgw_pcap_drop_total{interface="eth0"} 125
```

Nếu `pcap_drop_total` tăng nhanh thì có thể do:

- queue quá nhỏ;
- disk chậm;
- capture traffic quá lớn;
- filter quá rộng;
- writer không ghi kịp.

---

## 19. Logging

Không log từng packet.

Chỉ nên log lifecycle:

```text
PCAP capture started
interface=eth0
filter="udp portrange 20000-40000"

PCAP rotate
old_file=...
new_file=...

PCAP writer queue overflow
dropped=1024

PCAP capture stopped
packets=...
bytes=...
dropped=...
```

Log per-packet sẽ gây overhead rất lớn.

---

## 20. Linux capability

AF_PACKET cần quyền tạo raw socket.

Nếu RTPGW chạy bằng Linux binary, có thể cấp:

```bash
sudo setcap cap_net_raw+ep ./rtpgw
```

Kiểm tra:

```bash
getcap ./rtpgw
```

Kết quả:

```text
./rtpgw cap_net_raw=ep
```

Không nên chạy toàn bộ RTPGW bằng root chỉ để capture packet.

---

## 21. Docker

Nếu RTPGW chạy trong Docker:

```yaml
services:
  rtpgw:
    image: rtpgw:latest

    cap_add:
      - NET_RAW
```

Có thể cần thêm:

```yaml
network_mode: host
```

nếu RTPGW cần capture trực tiếp traffic trên host interface.

Nếu RTPGW chạy trong network namespace riêng của container thì interface cần capture có thể là:

```text
eth0
```

của container, không phải `eth0` của host.

---

## 22. Kubernetes

Pod cần capability:

```yaml
securityContext:
  capabilities:
    add:
      - NET_RAW
```

Ví dụ:

```yaml
containers:
  - name: rtpgw
    image: rtpgw:latest

    securityContext:
      capabilities:
        add:
          - NET_RAW
```

Nếu cần capture host NIC có thể phải dùng:

```yaml
hostNetwork: true
```

Cần đánh giá kỹ yêu cầu bảo mật trước khi bật trong production.

---

## 23. Phân tích RTP bằng Wireshark

Sau khi mở file PCAP:

```text
Telephony
  -> RTP
      -> RTP Streams
```

Có thể kiểm tra:

- SSRC
- Sequence number
- Timestamp
- Payload Type
- Marker Bit
- Jitter
- Packet loss
- Packet reorder
- RTP packet interval
- RTP stream duration

Filter:

```text
rtp
```

hoặc:

```text
udp.port == 30000
```

---

## 24. Kiểm tra RTP timestamp

Ví dụ audio:

```text
Codec: PCMU/PCMA
Clock Rate: 8000 Hz
Packetization: 20 ms
```

Timestamp tăng:

```text
8000 * 20 / 1000 = 160
```

PCAP mong đợi:

```text
Seq      Timestamp
1000     0
1001     160
1002     320
1003     480
1004     640
```

Nếu thấy:

```text
1000     0
1001     160
1002     160
```

có thể phát hiện timestamp không tăng đúng.

---

## 25. Kiểm tra silence giữa các utterance

Ví dụ:

```text
Clock Rate = 8000 Hz
Silence    = 500 ms
```

Timestamp gap tương ứng:

```text
8000 * 500 / 1000 = 4000
```

Ví dụ utterance 1 kết thúc:

```text
seq = 1042
ts  = 6560
```

Utterance 2 sau 500 ms phải bắt đầu gần:

```text
seq = 1043
ts  = 10560
```

Nếu PCAP cho thấy:

```text
seq = 1043
ts  = 6560
```

thì RTP timestamp đang không phản ánh khoảng silence thực tế.

---

## 26. Marker Bit

PCAP cũng cho phép kiểm tra Marker bit:

```text
RTP
 ├── Version
 ├── Payload Type
 ├── Marker
 ├── Sequence
 ├── Timestamp
 └── SSRC
```

Có thể kiểm tra packet đầu của một talkspurt/utterance có được set marker đúng theo thiết kế hay không.

Ví dụ:

```text
Utterance 1
packet #1: M=1
packet #2: M=0
packet #3: M=0

Silence

Utterance 2
packet #1: M=1
packet #2: M=0
```

---

## 27. Điều cần tránh

### 27.1 Ghi PCAP trực tiếp trong goroutine media

Không nên:

```go
func handleRTP(pkt []byte) {
    writer.WritePacket(...)
    processRTP(pkt)
}
```

Disk I/O có thể làm tăng media latency.

---

### 27.2 Queue không giới hạn

Không nên:

```text
capture -> unlimited queue -> disk
```

Nếu disk chậm, memory có thể tăng cho đến khi OOM.

---

### 27.3 Capture toàn interface trong thời gian dài

Ví dụ:

```text
interface = eth0
filter = none
```

có thể tạo PCAP rất lớn và tốn CPU.

---

### 27.4 Chạy RTPGW bằng root

Chỉ nên cấp capability tối thiểu cần thiết:

```text
CAP_NET_RAW
```

---

## 28. Luồng đầy đủ đề xuất

```text
                               NIC
                                │
                ┌───────────────┴────────────────┐
                │                                │
                ▼                                ▼
           AF_PACKET                       Kernel IP Stack
                │                                │
                ▼                                ▼
           BPF Filter                       UDP Socket
                │                                │
                ▼                                ▼
        Packet Capture                    ReadFromUDP()
                │                                │
                ▼                                ▼
         Bounded Queue                    pion/rtp
                │                                │
        ┌───────┴────────┐                       ▼
        │                │                 RTP Processing
        │ queue full     │                       │
        ▼                ▼                       ▼
   drop capture      writer goroutine        AI / MF
                         │
                         ▼
                    rotate manager
                         │
                         ▼
                     *.pcap
```

---

## 29. API thực tế

`Recorder` là concrete struct, không phải interface.
Constructor trả về error nếu AF_PACKET socket không tạo được (thiếu `CAP_NET_RAW`, interface không tồn tại, v.v.).
`Start()` và `Stop()` không trả về error — lỗi I/O được log và đếm qua `WriteErrs`.

```go
// New tạo Recorder. Gọi Start() để bắt đầu capture.
// Trả về error nếu không tạo được AF_PACKET socket.
func New(cfg Config) (*Recorder, error)

// Start khởi động 4 goroutine: ingress capture, egress capture,
// ingress writer, egress writer. Non-blocking.
func (r *Recorder) Start()

// Stop dừng tất cả goroutine và chờ chúng kết thúc.
func (r *Recorder) Stop()

// Stats trả về snapshot counters tại thời điểm gọi.
func (r *Recorder) Stats() Stats
```

Config:

```go
type Config struct {
    Interface string    // network interface, e.g. "eth0"
    OutputDir string    // thư mục ghi PCAP file
    PortMin   uint16    // lấy từ rtp.port_start qua ToPCAPConfig()
    PortMax   uint16    // lấy từ rtp.port_end qua ToPCAPConfig()
    QueueSize int       // bounded queue per direction; 0 → default 8192
    SnapLen   uint32    // max bytes per packet; 0 → default 65535
    Rotate    RotateConfig
}
```

Stats:

```go
type Stats struct {
    Packets   uint64 // tổng packet đã ghi vào file
    Bytes     uint64 // tổng bytes payload đã ghi (không tính PCAP overhead)
    Dropped   uint64 // packet bị drop do queue đầy
    WriteErrs uint64 // lỗi ghi file PCAP
}
```

Rotate:

```go
type RotateConfig struct {
    MaxSizeMB int // rotate khi file vượt size; 0 = không rotate
    MaxFiles  int // giữ tối đa N file per direction; 0 = không xóa
}
```

---

## 30. Khởi động cùng RTPGW

Ví dụ:

```go
func main() {

    cfg := loadConfig()

    var recorder *pcaprecorder.Recorder

    if cfg.PCAP.Enabled {
        pcapCfg := cfg.ToPCAPConfig() // port range lấy từ rtp.port_start/port_end
        recorder, err := pcap.New(pcapCfg)
        if err != nil {
            // AF_PACKET lỗi không làm gateway chết — chỉ log, RTP vẫn chạy.
            zap.L().Warn("pcap: disabled", zap.Error(err))
        } else {
            recorder.Start()
            defer recorder.Stop()
        }
    }

    runRTPGateway()
}
```

`Start()` không trả về error — lỗi I/O trong writer goroutine được log và đếm vào `Stats.WriteErrs`.
Chỉ `New()` trả về error (AF_PACKET socket, BPF compile).

---

## 31. Khuyến nghị cho RTPGW

Giải pháp khuyến nghị:

```text
AF_PACKET
    +
BPF filter
    +
bounded queue
    +
dedicated writer goroutine
    +
pcapgo
    +
file rotation
    +
metrics
```

Các nguyên tắc chính:

1. PCAP là chức năng debug/observability.
2. Không đặt disk I/O vào RTP media path.
3. Khi overload, drop PCAP trước, không drop RTP vì recorder.
4. Capture filter càng hẹp càng tốt.
5. Có rotate và giới hạn dung lượng disk.
6. Chỉ cấp `CAP_NET_RAW`, tránh chạy RTPGW bằng root.
7. Cho phép bật/tắt runtime/config nếu có thể.
8. Theo dõi packet capture drop bằng metric.

---

## 32. Kết luận

Có thể tích hợp capture packet trước kernel UDP processing trực tiếp vào RTPGW bằng Golang.

Giải pháp phù hợp nhất trên Linux là:

```text
gopacket/afpacket
```

để lấy raw Ethernet frame và:

```text
gopacket/pcapgo
```

để ghi file PCAP.

PCAP capture nên chạy song song với RTP data plane:

```text
AF_PACKET -> PCAP
UDP Socket -> RTPGW Processing
```

Không thay thế UDP socket hiện tại.

Giải pháp này đặc biệt hữu ích để debug các vấn đề:

- RTP timestamp
- sequence number
- marker bit
- packet pacing
- packet loss
- packet reorder
- RTP gap giữa các utterance
- traffic ingress/egress giữa MF, RTPGW và AI
