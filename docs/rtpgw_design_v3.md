# RTPGW - Thiết kế chi tiết Audio + Video AI Pipeline

## 1. Mục tiêu

RTPGW là Media Gateway nhận RTP realtime từ MF/SBC/Media Plane, quản lý session/port, xử lý media theo từng loại audio/video, sau đó stream dữ liệu đã chuẩn hóa sang AI Worker.

Thiết kế này mở rộng kiến trúc RTPGW hiện tại theo các nguyên tắc:

- Mỗi media stream dùng **UDP port riêng**.
- Dùng `pion/rtp` để parse RTP packet.
- Audio và video dùng pipeline riêng nhưng chia sẻ session lifecycle, admission control, AI routing và result dispatching.
- `selectedService` quyết định media type, codec, AI capability và preprocessing profile.
- Video H.264 được depacketize + decode ngay tại RTPGW bằng C/C++/FFmpeg thông qua cgo.
- Mỗi video stream sở hữu **một decoder context riêng**.
- Một decoder context chỉ có **một owner goroutine** gọi tuần tự.
- RTPGW gửi decoded frame sang AI Worker bằng gRPC bidirectional streaming.
- AI Worker dùng MediaPipe `LIVE_STREAM` và nhận `VideoFrame + timestamp_ms`.
- Queue đều bounded; video ưu tiên frame mới nhất để giữ realtime latency.

---

## 2. Kiến trúc tổng thể

```text
                         +-----------------------+
                         |         DCSF          |
                         | ANSWER / RELEASE      |
                         +-----------+-----------+
                                     |
                            selectedService
                                     |
                                     v
                         +-----------------------+
                         |   RTPGW Control Plane |
                         | - Session Manager     |
                         | - Service Resolver    |
                         | - Port Allocator      |
                         | - Admission Control   |
                         +-----------+-----------+
                                     |
                                     v
                                    MF
                                     |
                    +----------------+----------------+
                    |                                 |
                 Audio RTP                         Video RTP
                    |                                 |
              dedicated port                   dedicated port
                    |                                 |
                    v                                 v
          +-------------------+             +-------------------+
          | Audio RTP Ingress |             | Video RTP Ingress |
          | pion/rtp          |             | pion/rtp          |
          +---------+---------+             +---------+---------+
                    |                                 |
                    v                                 v
             Audio Pipeline                    Video Pipeline
                    |                                 |
         Decode / Resample                    Jitter / H264
                    |                         C++ Decode
                    v                                 |
               AudioChunk                            v
                                               VideoFrame
                    |                                 |
                    +---------------+-----------------+
                                    |
                                    v
                          +-------------------+
                          | AI Stream Manager |
                          +---------+---------+
                                    |
                                AI Router
                                    |
                              selectedService
                                    |
                +-------------------+-------------------+
                |                   |                   |
            ASR Worker        Face AI Worker     Segmentation AI
                |                   |                   |
         speech_to_text          sticker        change_background
                |                   |                   |
                +-------------------+-------------------+
                                    |
                                    v
                            Result Dispatcher
                                    |
                         +----------+----------+
                         |                     |
                     DCSF/App                 MF
```

---

## 3. Service Profile và selectedService

`selectedService` không chỉ dùng để chọn AI Worker mà còn quyết định toàn bộ media profile của session.

### 3.1 ServiceProfile

```go
type ServiceProfile struct {
    Name string

    MediaTypes []MediaType

    AudioCodec string
    VideoCodec string

    VideoClockRate int
    TargetFPS      int

    AIService string

    VideoInputFormat string // RGB24

    NeedTCore   bool
    NeedTAccess bool
}
```

### 3.2 Mapping đề xuất

| selectedService | Media | Codec | AI Service | AI Input | Target FPS |
|---|---|---|---|---|---|
| `speech_to_text` | audio | PCMU/PCMA/Opus | `asr` | PCM16 | - |
| `realtime_translation` | audio | PCMU/PCMA/Opus | `translation` | PCM16 | - |
| `sticker` | video | H264 | `face_landmark` | RGB24 | 15 |
| `change_background` | video | H264 | `segmentation` | RGB24 | 15 |
| `video_augmentation` | video | H264 | `video_augmentation` | RGB24 | 15 |

---

## 4. Session model mới

Không coi `{callId}-tcore` hay `{callId}-taccess` là một media stream duy nhất nữa. Một termination có thể có audio và video độc lập.

```text
CallSession
 |
 +-- tCore
 |    |
 |    +-- audio -> RTP port riêng
 |    +-- video -> RTP port riêng
 |
 +-- tAccess
      |
      +-- audio -> RTP port riêng
      +-- video -> RTP port riêng
```

### 4.1 Internal stream ID

```text
{callId}:tcore:audio
{callId}:tcore:video
{callId}:taccess:audio
{callId}:taccess:video
```

### 4.2 Go model

```go
type CallSession struct {
    CallID          string
    SelectedService string

    TCore   *Termination
    TAccess *Termination

    CreatedAt time.Time
    Status    string
}

type Termination struct {
    ID string

    Audio *MediaStream
    Video *MediaStream
}

type MediaStream struct {
    ID        string
    CallID    string
    Leg       string // tcore | taccess
    MediaType string // audio | video

    Codec       string
    PayloadType uint8
    ClockRate   int

    RTPPort int
    SSRC    uint32

    PacketQueue chan *rtp.Packet
    AudioQueue  chan AudioChunk
    FrameQueue  *LatestFrameQueue

    AIService string

    Ctx    context.Context
    Cancel context.CancelFunc
}
```

---

## 5. RTP port allocation

Nguyên tắc:

```text
1 media stream = 1 UDP port
```

Ví dụ `sticker` chỉ cần video:

```text
call-001:tcore:video   -> 40100
call-001:taccess:video -> 40101
```

Ví dụ `speech_to_text`:

```text
call-001:tcore:audio   -> 40110
call-001:taccess:audio -> 40111
```

PortAllocator hiện tại vẫn dùng được; thay vì acquire cố định 2 port/call, Control Plane acquire theo danh sách media stream mà `ServiceProfile` yêu cầu.

---

## 6. RTP ingress

### 6.1 UDP receive loop

Mỗi per-session port có một UDP listener riêng.

Receive loop chỉ làm việc nhẹ:

```text
ReadFromUDP
   |
   v
rtp.Packet.Unmarshal()
   |
   v
PacketQueue
```

Không decode hoặc gọi AI trong UDP read loop.

### 6.2 Pion RTP

RTPGW dùng `github.com/pion/rtp` để parse RTP header thay vì C++ tự parse fixed 12-byte header.

```go
var pkt rtp.Packet
if err := pkt.Unmarshal(buf[:n]); err != nil {
    // drop + metric
}
```

Pion xử lý RTP header, CSRC, extension và payload offset.

---

## 7. Audio Pipeline

Giữ nguyên hướng hiện tại:

```text
Audio RTP
   |
PacketQueue
   |
Jitter Buffer
   |
Codec Decoder
PCMU / PCMA / Opus
   |
PCM
   |
Resample 16kHz mono
   |
AudioChunk
   |
AI Stream Manager
   |
ASR / Translation AI
```

### 7.1 AudioChunk

```go
type AudioChunk struct {
    SessionID   string
    StreamID    string
    PCM         []byte
    SampleRate  int
    Channels    int
    TimestampMs int64
    DurationMs  int64
}
```

---

## 8. Video Pipeline

### 8.1 Luồng tổng thể

```text
H264/RTP
   |
PacketQueue
   |
Video Jitter Buffer
   |
ordered RTP packet
   |
C++ H264 Parser
Single / STAP-A / FU-A
   |
H264 Access Unit
   |
FFmpeg H264 Decoder
   |
AVFrame YUV420P/NV12
   |
Frame Processor
- resize
- YUV -> RGB24
- timestamp mapping
   |
Frame Sampler
   |
LatestFrameQueue
   |
AI gRPC stream
   |
MediaPipe LIVE_STREAM
```

---

## 9. Video Jitter Buffer

Chức năng:

- reorder packet theo sequence number;
- detect missing sequence;
- drop late packet;
- không giữ packet quá lâu;
- phát packet theo đúng thứ tự vào C++ parser.

Config đề xuất:

```yaml
video:
  jitter_buffer_ms: 60
  max_packet_late_ms: 120
```

Nếu detect packet gap, C++ parser phải được báo để reset FU-A/AU tương ứng.

---

## 10. C++ H.264 parser/decoder integration

### 10.1 Phạm vi C++

C++ chịu trách nhiệm:

```text
RTP payload metadata
   |
H264 parser
   |
Single NAL / STAP-A / FU-A
   |
Access Unit Assembler
   |
SPS/PPS cache
   |
IDR recovery
   |
FFmpeg decoder
   |
decoded frame
```

Không dùng C++ để:

- đọc pcap;
- parse Ethernet/IP/UDP;
- sort toàn bộ RTP packet;
- encode H264 trở lại;
- mux MP4.

### 10.2 Stateful parser per stream

```cpp
class H264RtpParser {
public:
    ParseStatus Push(
        uint16_t seq,
        uint32_t timestamp,
        bool marker,
        const uint8_t* payload,
        size_t payload_size);

private:
    std::vector<uint8_t> fu_buffer_;

    uint16_t expected_seq_;
    uint32_t current_timestamp_;

    bool fu_in_progress_;
    bool current_au_corrupted_;
    bool wait_for_idr_;

    std::vector<uint8_t> sps_;
    std::vector<uint8_t> pps_;

    H264AccessUnit current_au_;
};
```

### 10.3 H264AccessUnit

```cpp
struct H264AccessUnit {
    uint32_t rtp_timestamp;
    bool key_frame;
    bool corrupted;

    std::vector<uint8_t> data; // Annex-B
};
```

### 10.4 Supported packetization

- Single NAL Unit: type 1-23
- STAP-A: type 24
- FU-A: type 28

Output format nên là Annex-B:

```text
00 00 00 01 SPS
00 00 00 01 PPS
00 00 00 01 IDR
```

---

## 11. Packet loss và IDR recovery

### 11.1 FU-A loss

Nếu sequence không liên tục trong lúc ghép FU-A:

```text
FU-A start
FU-A middle
[packet lost]
FU-A end
```

thì:

```text
clear FU buffer
mark current AU corrupted
drop current AU
```

Không gửi H264 corrupted vào FFmpeg decoder.

### 11.2 Wait for IDR

Sau lỗi nặng hoặc decoder reset:

```text
wait_for_idr = true
```

Policy:

```text
P-frame -> drop
P-frame -> drop
IDR     -> prepend SPS/PPS -> decode -> resume
```

---

## 12. FFmpeg decoder

### 12.1 Per-stream decoder context

Mỗi video stream có một decoder context riêng:

```text
Stream A -> AVCodecContext A
Stream B -> AVCodecContext B
Stream C -> AVCodecContext C
```

Không share `AVCodecContext` giữa nhiều stream.

### 12.2 Decoder class

```cpp
class H264Decoder {
public:
    bool Open();

    int Decode(
        const H264AccessUnit& au,
        DecodedVideoFrame* out);

    void Reset();
    void Close();

private:
    AVCodecContext* codec_ctx_ = nullptr;
    AVFrame* frame_ = nullptr;
    AVPacket* packet_ = nullptr;
};
```

### 12.3 FFmpeg threading

Không nên để mỗi decoder tự sinh quá nhiều internal threads.

Config khởi đầu đề xuất:

```cpp
codec_ctx_->thread_count = 1;
```

hoặc `2`, sau đó benchmark theo số session thực tế.

---

## 13. cgo / C ABI

Go không gọi trực tiếp C++ class. C++ expose C ABI.

### 13.1 C API

```c
typedef void* VideoDecoderHandle;

typedef struct {
    uint64_t frame_id;

    uint32_t rtp_timestamp;
    uint64_t timestamp_ms;

    int width;
    int height;
    int stride;

    int pixel_format;

    uint8_t* data;
    int data_size;
} DecodedVideoFrame;

VideoDecoderHandle video_decoder_create();

int video_decoder_push_rtp(
    VideoDecoderHandle handle,
    uint16_t sequence,
    uint32_t timestamp,
    int marker,
    const uint8_t* payload,
    int payload_size);

int video_decoder_receive_frame(
    VideoDecoderHandle handle,
    DecodedVideoFrame* frame);

int video_decoder_reset(VideoDecoderHandle handle);

void video_decoder_release_frame(
    VideoDecoderHandle handle,
    DecodedVideoFrame* frame);

void video_decoder_destroy(VideoDecoderHandle handle);
```

---

## 14. Concurrency model Go <-> C++

### 14.1 Quy tắc bắt buộc

```text
1 stream
  -> 1 ordered queue
  -> 1 owner goroutine
  -> 1 C++ decoder handle
```

Nhiều goroutine được phép gọi C++ song song **nếu mỗi goroutine dùng decoder handle khác nhau**.

Không cho nhiều goroutine gọi đồng thời cùng một decoder handle.

### 14.2 Mô hình đúng

```text
Stream A                  Stream B                  Stream C
   |                         |                         |
PacketQueue              PacketQueue              PacketQueue
   |                         |                         |
owner goroutine          owner goroutine          owner goroutine
   |                         |                         |
C++ Handle A             C++ Handle B             C++ Handle C
```

### 14.3 Decode loop

```go
func (s *VideoStream) decodeLoop() {
    defer s.wg.Done()

    for {
        select {
        case pkt := <-s.packetQueue:
            ordered := s.jitter.Push(pkt)

            for _, p := range ordered {
                frames, err := s.decoder.Push(p)
                if err != nil {
                    continue
                }

                for _, frame := range frames {
                    s.frameQueue.PushLatest(frame)
                }
            }

        case <-s.ctx.Done():
            return
        }
    }
}
```

### 14.4 Global native decode scheduler

Để tránh hàng nghìn cgo decode chạy CPU đồng thời, có thể thêm semaphore toàn cục:

```go
type VideoDecodeScheduler struct {
    slots chan struct{}
}
```

Ví dụ:

```yaml
video:
  max_parallel_native_decodes: 64
```

Decoder context vẫn per-stream; semaphore chỉ giới hạn số call native đang chạy song song.

---

## 15. C++ memory ownership

### 15.1 Input Go -> C++

C++ không được giữ pointer tới Go memory sau khi cgo call return.

Nếu cần giữ FU-A payload qua nhiều packet, phải copy vào C++ owned buffer:

```cpp
fu_buffer_.insert(
    fu_buffer_.end(),
    payload + 2,
    payload + payload_size);
```

### 15.2 Output C++ -> Go

Lifecycle:

```text
C++ decode frame
   |
Go nhận pointer
   |
copy/send gRPC
   |
video_decoder_release_frame()
```

Không được free C++ frame trong khi Go vẫn đang sử dụng buffer.

---

## 16. Video Frame Processor

Sau FFmpeg decoder:

```text
AVFrame YUV420P/NV12
   |
resize optional
   |
YUV -> RGB24
   |
frame sampler
   |
VideoFrame
```

Dùng `libswscale` cho resize + pixel format conversion.

### 16.1 Không hard-code resolution

Resolution input lấy từ decoded `AVFrame`.

Target resolution lấy từ `ServiceProfile`.

Ví dụ:

```yaml
services:
  sticker:
    target_fps: 15
    frame:
      width: 640
      height: 360
      pixel_format: RGB24
```

---

## 17. RTP timestamp -> MediaPipe timestamp

MediaPipe `LIVE_STREAM` cần timestamp ms tăng dần.

H264 RTP clock:

```text
90000 Hz
```

RTPGW giữ:

```text
base_extended_rtp_ts
last_extended_rtp_ts
```

Convert:

```text
timestamp_ms =
    (extended_rtp_ts - base_extended_rtp_ts)
    * 1000 / 90000
```

Không dùng `time.Now()` làm media timestamp chính.

`time.Now()` chỉ dùng cho latency/observability.

### 17.1 Output frame identity

Mỗi frame gồm:

```text
frame_id
rtp_timestamp
timestamp_ms
```

`frame_id` dùng để correlate result với MF.

---

## 18. VideoFrame model RTPGW -> AI

```proto
message VideoFrame {
  string stream_id = 1;

  uint64 frame_id = 2;

  uint32 rtp_timestamp = 3;
  int64 timestamp_ms = 4;

  int32 width = 5;
  int32 height = 6;
  int32 stride = 7;

  PixelFormat pixel_format = 8;

  bytes data = 9;

  int64 received_at_ms = 10;
}

enum PixelFormat {
  PIXEL_FORMAT_UNKNOWN = 0;
  PIXEL_FORMAT_RGB24 = 1;
}
```

---

## 19. AI gRPC protocol

### 19.1 Video service

```proto
service VideoAIService {
  rpc Process(stream VideoRequest)
      returns (stream VideoResult);
}
```

### 19.2 Stream lifecycle

```proto
message VideoRequest {
  oneof payload {
    OpenVideoStream open = 1;
    VideoFrame frame = 2;
    CloseVideoStream close = 3;
  }
}
```

### 19.3 OpenVideoStream

```proto
message OpenVideoStream {
  string session_id = 1;
  string stream_id = 2;

  string selected_service = 3;

  string source_codec = 4;

  int32 source_width = 5;
  int32 source_height = 6;

  string pixel_format = 7;

  int32 target_fps = 8;

  string leg = 9;
}
```

Thông tin không đổi giữa các frame chỉ gửi một lần lúc mở stream.

---

## 20. MediaPipe LIVE_STREAM integration

AI Worker nhận:

```text
RGB24 bytes
width
height
timestamp_ms
```

Sau đó:

```python
array = np.frombuffer(req.data, dtype=np.uint8)
array = array.reshape(req.height, req.width, 3)

image = mp.Image(
    image_format=mp.ImageFormat.SRGB,
    data=array,
)

landmarker.detect_async(
    image,
    req.timestamp_ms,
)
```

hoặc:

```python
segmenter.segment_async(
    image,
    req.timestamp_ms,
)
```

Một RTP video stream tương ứng với một logical MediaPipe live stream.

---

## 21. Video frame queue và backpressure

MediaPipe LIVE_STREAM ưu tiên latency thấp, do đó video queue không nên dài.

Config:

```yaml
video:
  frame_queue_size: 3
```

Policy:

```text
queue chưa đầy
 -> enqueue

queue đầy
 -> drop oldest
 -> enqueue newest
```

Không block decoder chờ AI.

Mục tiêu:

```text
latest frame > complete frame history
```

---

## 22. FPS sampling

Không drop compressed H264 frame trước decoder.

Đúng:

```text
H264 30 FPS
   |
decode 30 FPS
   |
Decoded Frame Sampler
   |
15 FPS
   |
AI
```

Ví dụ:

```text
Decoded: F1 F2 F3 F4 F5 F6
AI:      F1    F3    F5
```

Sampling sau decode không phá H264 reference chain.

---

## 23. AI Router

WorkerRegistry phải thêm capability cho media type/input format.

```go
type WorkerInfo struct {
    ID   string
    Addr string

    Services     []string
    MediaTypes   []string
    InputFormats []string

    MaxStreams    int
    ActiveStreams int
    GPULoad       float64

    UpdatedAt time.Time
}
```

Ví dụ Face Worker:

```json
{
  "id": "face-ai-01",
  "addr": "10.10.10.31:50051",
  "services": ["sticker"],
  "media_types": ["video"],
  "input_formats": ["RGB24"],
  "max_streams": 100,
  "active_streams": 40,
  "gpu_load": 0.65
}
```

Router select:

```go
workerReg.Select(ai.SelectRequest{
    Service:     sess.SelectedService,
    MediaType:   stream.MediaType,
    InputFormat: "RGB24",
})
```

---

## 24. Result contract

### 24.1 Sticker / Face Landmark

```json
{
  "stream_id": "call-001:taccess:video",
  "frame_id": 1234,
  "rtp_timestamp": 45823000,
  "timestamp_ms": 41333,
  "type": "face_landmark",
  "faces": []
}
```

MF dùng `frame_id` hoặc `rtp_timestamp` để gắn sticker đúng frame.

### 24.2 Change Background

```json
{
  "stream_id": "call-001:taccess:video",
  "frame_id": 1234,
  "rtp_timestamp": 45823000,
  "timestamp_ms": 41333,
  "type": "segmentation",
  "width": 256,
  "height": 144,
  "encoding": "RLE",
  "mask": "..."
}
```

MF composite:

```text
original frame
   +
segmentation mask
   +
background asset
   |
   v
output frame
```

---

## 25. Session lifecycle

### 25.1 Create

```text
DCSF ANSWER
   |
selectedService
   |
ServiceResolver
   |
Create CallSession
   |
Create MediaStream(s)
   |
Allocate port(s)
   |
Start UDP listener(s)
   |
Create decoder handle cho video
   |
Start pipeline goroutine
   |
Open AI stream
```

### 25.2 Close

```text
DCSF RELEASE
   |
cancel session context
   |
stop accepting RTP
   |
close packet queues
   |
wait decode goroutine exit
   |
release outstanding frame
   |
video_decoder_destroy()
   |
close AI stream
   |
release RTP port
```

`video_decoder_destroy()` chỉ được gọi sau khi owner goroutine đã dừng.

---

## 26. Admission Control

Ngoài các điều kiện hiện có, video cần thêm:

```text
video_decoder_capacity
native_decode_slots
frame_queue_pressure
AI video worker capacity
memory pressure
RTP port availability
```

Ví dụ reject khi:

```text
active_video_streams >= max_video_streams
native_decode_utilization > 90%
no AI worker supports selectedService
memory usage > threshold
port pool exhausted
```

---

## 27. Metrics

### 27.1 RTP Video

```text
video_rtp_packets_total
video_rtp_parse_errors_total
video_rtp_lost_total
video_rtp_late_total
video_rtp_queue_dropped_total
```

### 27.2 H264

```text
h264_single_nalu_total
h264_stap_a_total
h264_fu_a_total
h264_fu_loss_total
h264_access_units_total
h264_access_units_dropped_total
h264_wait_idr_total
h264_idr_total
```

### 27.3 Decoder

```text
video_decode_frames_total
video_decode_errors_total
video_decode_latency_ms
video_decoders_active
video_decode_slots_used
```

### 27.4 Frame Pipeline

```text
video_frames_sampled_total
video_frames_dropped_total
video_frame_queue_usage
video_frame_convert_latency_ms
```

### 27.5 AI

```text
video_ai_streams_active
video_ai_send_errors_total
video_ai_recv_errors_total
video_ai_frame_latency_ms
video_ai_results_total
```

---

## 28. Config đề xuất

```yaml
gateway:
  id: "rtpgw-01"

rtp:
  public_ip: "10.10.10.22"
  bind_ip: ""
  port_start: 40000
  port_end: 42000
  socket_read_buffer: 4194304

session:
  max_sessions: 10000
  idle_timeout_sec: 30
  per_stream_packet_queue: 256

video:
  enabled: true

  jitter_buffer_ms: 60
  max_packet_late_ms: 120

  frame_queue_size: 3

  max_parallel_native_decodes: 64

  decoder_threads_per_stream: 1

  wait_for_idr_on_start: true
  drop_until_idr_on_loss: true

  default_target_fps: 15

ai:
  max_active_audio_streams: 1000
  max_active_video_streams: 500

  video_send_timeout_ms: 500
  video_stream_timeout_sec: 300

services:
  sticker:
    media_type: video
    codec: H264
    ai_service: face_landmark
    target_fps: 15
    frame_width: 640
    frame_height: 360
    pixel_format: RGB24

  change_background:
    media_type: video
    codec: H264
    ai_service: segmentation
    target_fps: 15
    frame_width: 640
    frame_height: 360
    pixel_format: RGB24
```

---

## 29. Source code structure đề xuất

```text
cmd/
  rtpgw/
    main.go

internal/
  config/
    config.go

  controlplane/
    server.go
    handler.go
    service_registry.go
    media_port_planner.go
    admission_controller.go
    port_allocator.go

  session/
    call_session.go
    termination.go
    media_stream.go
    manager.go
    lifecycle.go

  ingress/
    rawrtp/
      listener.go
      parser.go

  pipeline/
    audio/
      jitter.go
      decoder.go
      resampler.go
      chunker.go
      worker_pool.go

    video/
      pipeline.go
      jitter.go
      frame.go
      frame_queue.go
      frame_sampler.go
      timestamp.go
      processor.go

      h264/
        decoder.go
        decoder_cgo.go

        native/
          video_decoder.h
          video_decoder.cpp
          h264_rtp_parser.h
          h264_rtp_parser.cpp
          h264_decoder.h
          h264_decoder.cpp
          frame_converter.h
          frame_converter.cpp

  ai/
    router.go
    worker_registry.go
    audio_stream_manager.go
    video_stream_manager.go

    proto/
      audio.proto
      video.proto

  result/
    dispatcher.go
    transcript_sink.go
    video_result_sink.go
    mf_grpc_sink.go

  monitor/
    metrics.go
    health.go
```

---

## 30. Build C++ native library

Production C++ không cần `libpcap` hoặc `libavformat` nếu không mux file.

Dependencies chính:

```text
libavcodec
libavutil
libswscale
```

Build shared library:

```text
libvideo_decoder.so
```

Go cgo link:

```go
/*
#cgo CFLAGS: -I${SRCDIR}/native
#cgo LDFLAGS: -L${SRCDIR}/native/lib -lvideo_decoder -lavcodec -lavutil -lswscale -lstdc++
#include "video_decoder.h"
*/
import "C"
```

---

## 31. Failure handling

### 31.1 RTP packet loss

```text
loss detected
   |
mark AU corrupted
   |
drop AU
   |
optional wait IDR
```

### 31.2 Decoder error

```text
avcodec decode error
   |
metric++
   |
reset decoder
   |
wait IDR
```

### 31.3 AI slow

```text
FrameQueue full
   |
drop oldest
   |
keep newest
```

Không block decoder hoặc RTP ingress.

### 31.4 AI Worker failure

```text
AI stream failure
   |
AI Router select new worker
   |
Open new stream
   |
continue with latest decoded frames
```

Không cần restart H264 decoder vì decoder nằm tại RTPGW.

---

## 32. Test Plan

### 32.1 H264 parser

- Single NAL Unit
- STAP-A nhiều NALU
- FU-A start/middle/end
- FU-A packet loss
- sequence wrap-around
- timestamp change
- marker boundary
- SPS/PPS update
- IDR detection

### 32.2 Decoder

- SPS/PPS + IDR decode
- P-frame sequence
- resolution change
- decoder reset
- corrupted AU
- packet loss + recover next IDR

### 32.3 Concurrency

- nhiều stream decode song song
- cùng stream chỉ một owner goroutine
- destroy race test
- reset while session closing
- max native decode slots

### 32.4 MediaPipe contract

- timestamp_ms monotonic
- RGB24 dimensions đúng
- frame_id correlation
- sticker result correlation
- segmentation mask correlation

### 32.5 Load

- 100 / 500 / 1000 RTP sessions
- CPU usage
- RSS / native memory
- number of threads
- cgo calls/s
- decode latency p95/p99
- frame drop rate
- AI send bandwidth

---

## 33. Acceptance Criteria

Thiết kế được coi là đạt khi:

1. RTPGW nhận H264/RTP realtime qua dedicated port.
2. `pion/rtp` parse packet thành công.
3. Packet được reorder và packet loss được phát hiện.
4. C++ parser xử lý Single NAL/STAP-A/FU-A streaming.
5. Không giữ pointer Go memory sau cgo call.
6. Mỗi video stream có decoder handle riêng.
7. Cùng decoder handle không bị gọi concurrent.
8. FFmpeg decode thành frame ổn định sau IDR.
9. Packet loss không làm decoder nhận corrupted AU kéo dài.
10. RTP timestamp được giữ tới decoded frame.
11. `timestamp_ms` tăng đơn điệu cho MediaPipe LIVE_STREAM.
12. Frame output được chuẩn hóa RGB24.
13. Frame queue bounded và drop oldest khi AI chậm.
14. AI Worker được chọn theo `selectedService`.
15. Result chứa `frame_id + rtp_timestamp + timestamp_ms` để MF correlate.
16. Session close không xảy ra use-after-free decoder/frame.
17. Port, decoder, goroutine và native memory được cleanup đầy đủ.
18. Hệ thống không sinh goroutine/cgo/native thread không giới hạn theo packet.

---

## 34. Kết luận

Kiến trúc RTPGW sau khi mở rộng video được chia rõ thành:

```text
Control Plane
   -> selectedService
   -> media profile
   -> port allocation

RTP Data Plane
   -> pion/rtp
   -> ordered packet queue

Audio Pipeline
   -> decode/resample/chunk
   -> Audio AI

Video Pipeline
   -> H264 RTP parser
   -> FFmpeg decode C++/cgo
   -> RGB VideoFrame
   -> MediaPipe LIVE_STREAM AI

AI Router
   -> route theo selectedService/capability/load

Result Layer
   -> transcript hoặc video metadata/mask
   -> DCSF/App/MF
```

Nguyên tắc concurrency quan trọng nhất:

```text
1 video stream
  = 1 ordered packet flow
  = 1 owner goroutine
  = 1 C++ decoder handle
```

Concurrency xảy ra giữa nhiều stream, không xảy ra bên trong cùng một H264 decoder context.
