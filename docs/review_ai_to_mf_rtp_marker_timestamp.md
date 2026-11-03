# Review RTP Egress: AI → RTPGW → MF

## 1. Phạm vi review

Tài liệu này tổng hợp review luồng gửi audio từ AI sang MF qua RTP, tập trung vào các vấn đề:

- RTP timestamp không phản ánh khoảng silence giữa các talkspurt.
- Marker bit luôn bằng `false`.
- Cách tính timestamp gap giữa các utterance/talkspurt.
- Rủi ro khi dùng `idle++` theo `time.Ticker`.
- Drop frame khi queue đầy nhưng không advance timestamp.
- Phần dư cuối `AudioPayload` bị bỏ.
- `AudioPT` từ AI chưa được validate/sử dụng.
- `frameSize = tsIncr` chỉ đúng với G.711, chưa generic cho codec khác.

---

## 2. Luồng hiện tại AI → MF

Luồng hiện tại trong code:

```text
AI Worker
   │ gRPC Recv()
   ▼
internal/ai/grpc_client.go
   │
   ▼
sess.ResultQueue
   │
   ▼
Coordinator.resultPump()
   │ r.AudioPayload
   ▼
sess.SendRTP()
   │
   ▼
SetRTPEgress callback
internal/ingress/rawrtp/port_manager.go
   │
   ▼
pacer.Push(audioPayload)
   │ split 160 bytes/frame
   ▼
frameQ
   │
   │ 1 frame / 20 ms
   ▼
EgressPacer.Run()
   │
   ▼
Sender.Send()
   │
   ▼
UDP/RTP
   │
   ▼
MF
```

`Coordinator.resultPump()` lấy `AudioPayload` từ result của AI và đưa sang `sess.SendRTP()`.

`port_manager.go` khởi tạo:

- `Sender`
- `EgressPacer`

Sau đó `pacer.Push()` chia payload thành các frame và `EgressPacer.Run()` gửi 1 frame mỗi 20 ms.

---

# 3. Issue 1 — RTP timestamp không phản ánh khoảng silence

## 3.1. Hiện trạng

Trong `sender.go`, timestamp chỉ tăng khi thực sự gửi RTP packet:

```go
Timestamp: s.ts,
...
s.seq++
s.ts += s.tsIncr
```

Với PCMU/8000, `ptime = 20ms`:

```text
tsIncr = 8000 × 20 / 1000
       = 160
```

Trong `pacer.go`:

```go
case <-ticker.C:
    select {
    case frame := <-p.frameQ:
        p.sender.Send(frame)

    default:
        // nothing
    }
```

Khi `frameQ` rỗng:

- Không gửi packet.
- Không tăng RTP timestamp.
- Khoảng thời gian silence không xuất hiện trên RTP media timeline.

## 3.2. Hậu quả

Ví dụ utterance đầu có 42 frame:

```text
frame 1      ts = 0
frame 2      ts = 160
...
frame 42     ts = 6560
```

Sau khi `Send()` frame cuối hoàn thành, state nội bộ đã là:

```text
Sender.ts = 6720
```

Nếu sau đó có 500 ms silence:

```text
500 ms / 20 ms = 25 frame-times

25 × 160 = 4000 timestamp units
```

Timestamp packet đầu của talkspurt tiếp theo phải là:

```text
6720 + 4000 = 10720
```

Timeline đúng:

```text
Utterance #1:
0, 160, 320, ... 6560

500 ms silence:
6720 ... 10559

Utterance #2:
10720, 10880, 11040, ...
```

Nếu không advance timestamp trong silence thì packet đầu utterance tiếp theo sẽ có:

```text
ts = 6720
```

MF sẽ hiểu hai đoạn audio nằm liên tục trên media timeline.

Hậu quả:

- Khoảng im lặng bị mất.
- Hai câu có thể bị nghe như dính liền nhau.
- Jitter buffer không có đúng timeline để phục hồi thời gian phát.

---

# 4. Issue 2 — RTP Marker bit luôn `false`

## 4.1. Hiện trạng

Header RTP đang được tạo mà không set `Marker`:

```go
Header: pionrtp.Header{
    Version:        2,
    PayloadType:    s.pt,
    SequenceNumber: s.seq,
    Timestamp:      s.ts,
    SSRC:           s.ssrc,
},
```

Do đó:

```text
Marker = false
```

cho toàn bộ packet.

## 4.2. Semantics đúng

Không nên hiểu:

```text
Marker=true cho packet đầu tiên của mỗi semantic utterance
```

Mà nên hiểu:

```text
Marker=true cho packet đầu tiên của một talkspurt sau silence
```

Ví dụ:

```text
RTP RTP RTP RTP -------- silence -------- RTP RTP RTP
 M   0   0   0                            M   0   0
```

Nếu AI trả hai utterance liên tiếp và utterance thứ hai đã nằm sẵn trong queue khi utterance thứ nhất chưa phát xong:

```text
[utt1 frames][utt2 frames]
```

thì trên RTP output không có silence.

Trong trường hợp đó không cần set Marker chỉ vì đổi `utterance_id`.

Ngược lại:

```text
utt1
      500ms queue empty
                         utt2
```

packet đầu tiên của `utt2` phải:

```text
Marker = true
```

---

# 5. Issue 3 — Không nên chỉ dùng `idle++`

Một giải pháp ban đầu có thể là:

```go
idle := 0

case frame := <-p.frameQ:
    if idle > 0 {
        p.sender.AdvanceTimestamp(idle)
        idle = 0
        p.sender.SendMarked(frame)
    } else {
        p.sender.Send(frame)
    }

default:
    idle++
```

Hướng này đúng về ý tưởng nhưng có hai vấn đề.

## 5.1. Packet RTP đầu tiên có thể vẫn Marker=false

Khi service vừa start và queue đã có frame:

```text
idle = 0
frame available
```

code sẽ gọi:

```go
Send(frame)
```

Packet đầu tiên sẽ có:

```text
Marker = false
```

Trong khi đây là packet đầu tiên của một talkspurt.

Cần thêm trạng thái:

```go
started bool
```

Packet đầu tiên cần:

```text
Marker = true
```

## 5.2. `time.Ticker` có thể không phản ánh đầy đủ wall-clock gap

Nếu goroutine bị scheduler delay hoặc `WriteTo()` bị block trong một khoảng thời gian, không nên giả định luôn nhận đủ mọi tick 20 ms.

Ví dụ wall-clock thực tế trôi qua 100 ms nhưng goroutine không xử lý đủ 5 ticker events.

Khi đó:

```text
idle++
```

có thể nhỏ hơn số frame-time thực sự đã trôi qua.

Kết quả:

- RTP timestamp vẫn chậm hơn wall clock.
- Media timeline tiếp tục bị compress.

---

# 6. Giải pháp khuyến nghị — tính gap bằng elapsed pacing time

Nên lưu thời điểm tick cuối cùng mà RTP packet được gửi:

```go
var started bool
var lastSendTick time.Time
```

Khi frame mới xuất hiện:

```go
slots := int(tick.Sub(lastSendTick) / interval)
```

Vì `Sender.Send()` đã tự tăng timestamp cho packet kế tiếp một lần, số frame cần advance thêm là:

```go
missingFrames := slots - 1
```

Ví dụ:

```text
last send tick = 840 ms
new send tick  = 1360 ms

elapsed        = 520 ms
slots          = 520 / 20
               = 26

missingFrames  = 26 - 1
               = 25
```

Advance:

```text
25 × 160 = 4000 RTP timestamp units
```

Tương ứng đúng với:

```text
500 ms silence
```

---

# 7. Thay đổi đề xuất trong `Sender`

Nên bổ sung API:

```go
func (s *Sender) AdvanceFrames(frames int)
func (s *Sender) SendMarked(payload []byte) error
```

Không khuyến nghị tên:

```go
AdvanceTimestamp(ticks int)
```

vì `ticks` dễ gây hiểu nhầm với RTP clock tick.

Ở đây thực chất input là:

```text
số packet/frame-time 20 ms bị bỏ qua
```

## Ví dụ implementation

```go
func (s *Sender) AdvanceFrames(frames int) {
    if frames <= 0 {
        return
    }

    s.mu.Lock()
    s.ts += uint32(frames) * s.tsIncr
    s.mu.Unlock()
}

func (s *Sender) Send(payload []byte) error {
    return s.send(payload, false)
}

func (s *Sender) SendMarked(payload []byte) error {
    return s.send(payload, true)
}

func (s *Sender) send(payload []byte, marker bool) error {
    s.mu.Lock()

    pkt := pionrtp.Packet{
        Header: pionrtp.Header{
            Version:        2,
            Marker:         marker,
            PayloadType:    s.pt,
            SequenceNumber: s.seq,
            Timestamp:      s.ts,
            SSRC:           s.ssrc,
        },
        Payload: payload,
    }

    s.seq++
    s.ts += s.tsIncr

    s.mu.Unlock()

    raw, err := pkt.Marshal()
    if err != nil {
        return fmt.Errorf("rtpout: marshal: %w", err)
    }

    if _, err := s.conn.WriteTo(raw, s.remoteAddr); err != nil {
        return fmt.Errorf("rtpout: write: %w", err)
    }

    return nil
}
```

Lưu ý:

- Chỉ `Sender` quản lý `seq` và `ts`.
- Sequence number chỉ tăng khi thực sự gửi packet.
- Trong silence chỉ advance timestamp, không advance sequence number.

---

# 8. Thay đổi đề xuất trong `EgressPacer.Run`

```go
func (p *EgressPacer) Run(ctx context.Context) {
    interval := time.Duration(p.ptimeMs) * time.Millisecond

    ticker := time.NewTicker(interval)
    defer ticker.Stop()

    var started bool
    var lastSendTick time.Time

    for {
        select {
        case <-ctx.Done():
            return

        case tick := <-ticker.C:
            select {
            case frame := <-p.frameQ:
                marker := !started

                if started {
                    slots := int(tick.Sub(lastSendTick) / interval)

                    // Consecutive RTP packet bình thường => slots = 1
                    if slots < 1 {
                        slots = 1
                    }

                    missingFrames := slots - 1

                    if missingFrames > 0 {
                        p.sender.AdvanceFrames(missingFrames)
                        marker = true
                    }
                }

                var err error

                if marker {
                    err = p.sender.SendMarked(frame)
                } else {
                    err = p.sender.Send(frame)
                }

                if err != nil {
                    zap.L().Debug(
                        "rtp: egress send failed",
                        zap.Error(err),
                    )
                }

                started = true
                lastSendTick = tick

            default:
                // Không cần idle++
            }
        }
    }
}
```

Timeline kết quả:

```text
                     queue empty
                         │
                         │ 500 ms
                         │
                         ▼
RTP RTP RTP RTP ------------------------- RTP RTP RTP
 M   0   0   0                            M   0   0

TS:
0
160
320
...
6560
                                          10720
                                          10880
                                          11040
```

Sequence:

```text
SEQ:
100
101
...
141

500 ms silence

142
143
144
```

Sequence không nhảy trong silence là đúng vì không có RTP packet được gửi.

---

# 9. Issue 4 — Queue full drop frame nhưng RTP timestamp không jump

Trong `pacer.Push()` hiện tại có logic dạng:

```go
select {
case p.frameQ <- frame:
default:
    dropped++
}
```

Ví dụ:

```text
frame 100 enqueue
frame 101 DROP
frame 102 enqueue
```

Pacer có thể gửi:

```text
frame100 timestamp = 16000
frame102 timestamp = 16160
```

Nhưng về mặt media timeline, frame101 đã bị mất 20 ms.

Timestamp của frame102 lẽ ra phải phản ánh gap:

```text
frame100 ts = 16000
frame101    = missing 20ms
frame102 ts = 16320
```

Nếu vẫn gửi `16160`, MF sẽ hiểu audio của frame102 nằm ngay sau frame100.

Kết quả:

- Media timeline bị compress.
- Drop frame trở thành mất thời gian thực, không chỉ mất audio.

## Khuyến nghị

Không nên drop từng frame một cách độc lập.

Các lựa chọn tốt hơn:

1. Queue theo utterance và drop toàn utterance theo policy.
2. Nếu queue không đủ capacity cho payload mới thì reject/drop cả payload mới.
3. Nếu bắt buộc drop frame thì phải carry metadata về số frame bị drop để timestamp được advance tương ứng.

---

# 10. Issue 5 — Phần dư cuối utterance bị bỏ

Logic hiện tại:

```go
for len(payload) >= p.frameSize {
    ...
}
```

Nếu payload không chia hết cho 160 byte, phần dư bị bỏ.

Ví dụ:

```text
AudioPayload = 13,700 bytes

85 × 160 = 13,600 bytes
remainder = 100 bytes
```

100 byte cuối tương đương:

```text
100 / 8000 = 12.5 ms
```

audio cuối utterance bị mất.

## Khuyến nghị cho PCMU

Frame cuối nên được pad silence tới đủ 160 bytes.

PCMU silence thường dùng:

```text
0xFF
```

Ví dụ:

```go
frame := make([]byte, frameSize)

for i := range frame {
    frame[i] = 0xFF
}

copy(frame, remainingPayload)
```

Sau đó enqueue frame đầy đủ 160 byte.

---

# 11. Issue 6 — `AudioPT` từ AI chưa được sử dụng/validate

AI result có các thông tin dạng:

```go
AudioPayload []byte
AudioPT      uint8
```

Nhưng RTP Sender hiện được tạo từ RTP inbound:

```go
sender := rtpout.New(
    conn,
    remoteAddr,
    hdr.SSRC^0x80000000,
    hdr.PayloadType,
    sess.SampleRate,
    ptimeMs,
)
```

`r.AudioPT` từ AI chưa được sử dụng hoặc validate.

Nếu contract hệ thống đảm bảo:

```text
AI output codec == MF negotiated codec
```

thì có thể không cần dùng `AudioPT` để tạo RTP Header.

Tuy nhiên nên validate:

```text
AI AudioPT == negotiated RTP egress PT
```

hoặc validate theo codec name/config.

Nếu không, có rủi ro:

```text
AI payload bytes = PCMU
RTP PayloadType  = codec khác
```

MF sẽ decode sai.

---

# 12. Issue 7 — `frameSize = tsIncr` chỉ đúng cho G.711

Hiện tại có logic:

```go
frameSize: int(sender.tsIncr)
```

Với PCMU/8000:

```text
20 ms × 8000 = 160 samples
1 byte/sample
=> 160 bytes
```

nên:

```text
frameSize == tsIncr
```

là đúng.

Tương tự với PCMA.

Nhưng điều này không generic cho codec compressed như:

- AMR
- AMR-WB
- Opus
- EVS

Ví dụ AMR-WB:

```text
clock rate = 16000 Hz
ptime      = 20 ms

timestamp increment = 320
```

Nhưng RTP payload không phải 320 bytes.

Do đó kiến trúc hiện tại nên được hiểu là:

```text
G.711 RTP egress pacer
```

chứ chưa phải generic RTP codec pacer.

## Khuyến nghị

Tách rõ:

```text
timestampIncrement
payloadFrameSize
packetizer
```

Ví dụ:

```go
type Packetizer interface {
    Packetize(payload []byte) ([]RTPFrame, error)
}
```

Mỗi codec tự quyết định:

- Encoded frame size.
- RTP payload layout.
- Timestamp increment.
- Padding hoặc framing rule.

---

# 13. Semantics utterance và talkspurt

Cần phân biệt rõ hai khái niệm.

## AI utterance

Semantic boundary do AI tạo:

```text
"Xin chào"
"Bạn khỏe không?"
```

## RTP talkspurt

Media boundary dựa trên silence trên RTP timeline:

```text
audio → silence → audio
```

Không nên tự động set:

```text
Marker=true
```

chỉ vì `utterance_id` thay đổi.

Nếu hai utterance nối liên tục:

```text
utt1 frames | utt2 frames
```

và không có idle frame-time:

```text
Marker chỉ cần ở đầu talkspurt đầu tiên.
```

Nếu có silence:

```text
utt1 | silence | utt2
```

packet đầu `utt2`:

```text
Marker=true
```

---

# 14. Đề xuất kiến trúc egress

```text
AI AudioPayload
      │
      ▼
Audio Egress Queue
      │
      ▼
Codec Packetizer
      │
      ▼
20ms RTP Pacer
      │
      ├── continuous packet
      │      timestamp += 160
      │      Marker = 0
      │
      └── packet after silence
             timestamp += missingFrames × 160
             Marker = 1
      │
      ▼
RTP Sender
      │
      ▼
MF
```

Với PCMU/8000/20ms:

```text
Payload/frame = 160 bytes
TS increment  = 160
Packet rate   = 50 packet/s
```

---

# 15. Metrics/log nên bổ sung

Để verify sau khi fix, nên log:

```text
utterance_id
audio_payload_bytes
audio_duration_ms

queue_depth_frames
queue_audio_ms
dropped_frames
dropped_audio_ms

rtp_sequence
rtp_timestamp
rtp_marker
rtp_payload_bytes

missing_frames_before_send
timestamp_advance
time_since_last_packet_ms

egress_first_packet_time
egress_last_packet_time
```

Ví dụ sau 500 ms silence:

```text
last_rtp_timestamp        = 6560
time_since_last_packet_ms = 520
missing_frames            = 25
timestamp_advance         = 4000
next_rtp_timestamp        = 10720
marker                    = true
```

---

# 16. Test case đề xuất

## TC01 — Continuous audio

Input:

```text
10 frames liên tục
```

Expected:

```text
TS = 0,160,320,...,1440
SEQ tăng liên tục
Marker=true ở packet đầu
Marker=false ở packet còn lại
```

---

## TC02 — 500 ms silence giữa hai talkspurt

Input:

```text
42 frames
500 ms silence
23 frames
```

Expected:

```text
last TS talkspurt 1 = 6560
first TS talkspurt 2 = 10720
Marker talkspurt 2 first packet = true
Sequence không jump trong silence
```

---

## TC03 — Hai AI utterance nối ngay nhau

Input:

```text
utt1 frames đã queue
utt2 frames đã queue ngay sau utt1
```

Expected:

```text
Không timestamp gap
Không Marker mới chỉ vì utterance_id thay đổi
```

---

## TC04 — Packet đầu tiên của session

Expected:

```text
Marker=true
```

---

## TC05 — Payload không chia hết 160 bytes

Input:

```text
13700 bytes
```

Expected:

```text
86 RTP packets
85 full data frames
1 final padded frame
Không mất 100 bytes cuối
```

---

## TC06 — Queue overflow

Cần verify policy mới:

```text
Không drop arbitrary frame mà làm RTP timeline bị compress.
```

Nếu drop tương ứng 20 ms:

```text
timestamp phải phản ánh 20 ms bị mất.
```

---

## TC07 — Scheduler delay

Giả lập pacer goroutine bị delay 100–200 ms.

Expected:

```text
Timestamp gap dựa trên elapsed time,
không dựa đơn thuần vào số ticker event đọc được.
```

---

# 17. Mức độ ưu tiên

| Priority | Issue | Mức độ |
|---|---|---|
| P0 | Timestamp không advance trong silence | Critical |
| P0 | RTP burst/pacing sai | Critical |
| P1 | Marker bit không set | High |
| P1 | Queue drop frame không advance timestamp | High |
| P1 | Payload remainder bị bỏ | High |
| P2 | AI AudioPT chưa validate | Medium |
| P2 | `frameSize = tsIncr` không generic | Medium |

---

# 18. Kết luận

Hướng sửa bằng:

```text
AdvanceTimestamp + SendMarked
```

là đúng, nhưng nên triển khai theo các nguyên tắc sau:

1. Không dùng `idle++` làm nguồn thời gian chính.
2. Tính timestamp gap từ elapsed pacing time.
3. Packet đầu tiên của RTP talkspurt phải có `Marker=true`.
4. Marker biểu diễn talkspurt, không phải semantic AI utterance.
5. Sequence number chỉ tăng khi gửi packet.
6. Timestamp phải advance cả khi media timeline có silence/drop.
7. Không được drop remainder cuối `AudioPayload`.
8. Queue overflow phải có policy đảm bảo RTP timeline không bị compress.
9. Validate codec/Payload Type giữa AI output và RTP negotiated egress.
10. Tách `payloadFrameSize` khỏi `timestampIncrement` nếu muốn hỗ trợ codec ngoài G.711.

Sau khi áp dụng các thay đổi này, RTPGW sẽ giữ được đúng media timeline giữa các đợt TTS từ AI và MF có đủ thông tin để jitter buffer xử lý talkspurt/silence đúng hơn.
