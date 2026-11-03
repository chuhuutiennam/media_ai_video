# Phân tích pion/webrtc v4 — Thiết kế Media AI Gateway (STT/Translate)

Import path: `github.com/pion/webrtc/v4`

---

## 1. Tổng quan kiến trúc thư viện

```
api.go              -> Factory: tạo PeerConnection từ MediaEngine + SettingEngine + InterceptorRegistry
mediaengine.go      -> Đăng ký codec (Opus, VP8, H264, ...) và header extensions
settingengine.go    -> Tuỳ chọn low-level: ICE network types, port range, DTLS, loggers, vnet...
interceptor.go      -> Pipeline xử lý RTP/RTCP (NACK, RTCP reports, TWCC, jitterbuffer...)
configuration.go    -> ICEServers (STUN/TURN), ICETransportPolicy, BundlePolicy, Certificates
peerconnection.go   -> Core: SDP O/A, transceivers, signaling state machine, OnTrack/OnDataChannel
icetransport.go     -> Wrap pion/ice agent: gathering, connectivity checks
dtlstransport.go    -> Wrap pion/dtls: handshake, SRTP key derivation (DTLS-SRTP)
rtpreceiver.go      -> Nhận RTP/RTCP cho 1 track remote, expose qua TrackRemote
rtpsender.go        -> Gửi RTP cho 1 track local
rtptransceiver.go   -> Cặp (sender, receiver) gắn với 1 "m=" line trong SDP, có Direction
trackremote.go      -> Đại diện track media đến từ remote — ReadRTP()/Read()
tracklocal*.go      -> Track media gửi đi — WriteRTP() hoặc WriteSample()
datachannel.go      -> Wrap pion/sctp + DCEP — gửi/nhận text/binary message
```

Mối quan hệ phân lớp (tương tự stack bạn quen từ IMS):

```
Application
   |
PeerConnection  ------------------------------
   |                                          |
RTPTransceiver (per m= line)         DataChannel (per SCTP stream)
   |
   +-- RTPSender   --> TrackLocal  --> SRTP encrypt --> ICE --> network
   +-- RTPReceiver --> TrackRemote <-- SRTP decrypt <-- ICE <-- network
                              |
                       DTLSTransport (key exchange, SCTP carrier)
                              |
                       ICETransport (connectivity, candidate pairs)
```

---

## 2. Luồng khởi tạo: MediaEngine -> InterceptorRegistry -> API

**File: `api.go`, `mediaengine.go`, `interceptor.go`**

```go
m := &webrtc.MediaEngine{}
if err := m.RegisterDefaultCodecs(); err != nil { panic(err) }
// hoặc đăng ký riêng Opus:
m.RegisterCodec(webrtc.RTPCodecParameters{
    RTPCodecCapability: webrtc.RTPCodecCapability{
        MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
        SDPFmtpLine: "minptime=10;useinbandfec=1",
    },
    PayloadType: 111,
}, webrtc.RTPCodecTypeAudio)

i := &interceptor.Registry{}
if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil { panic(err) }

s := webrtc.SettingEngine{}
// ví dụ: giới hạn UDP port range cho media (giống cấu hình RTP port range trong IMS MGW)
s.SetEphemeralUDPPortRange(20000, 20100)

api := webrtc.NewAPI(
    webrtc.WithMediaEngine(m),
    webrtc.WithInterceptorRegistry(i),
    webrtc.WithSettingEngine(s),
)
```

**Vai trò từng thành phần:**

- **MediaEngine**: "bảng codec" — quyết định payload type nào map sang codec nào. Đây là nơi bạn cố định `PayloadType: 111 -> Opus` để khi đọc RTP, bạn biết payload nào cần decode Opus.
- **InterceptorRegistry**: chuỗi middleware cho RTP/RTCP — `RegisterDefaultInterceptors` thêm NACK generator/responder, RTCP Sender/Receiver reports, TWCC. Tương tự "interceptor chain" trong gRPC — bạn có thể viết interceptor riêng để tap vào RTP stream song song mà không động vào luồng đọc chính (hữu ích để đo metrics hoặc fork audio cho AI mà không ảnh hưởng `ReadRTP`).
- **SettingEngine**: tuỳ chọn vận hành — port range, network types (UDP4 only — quan trọng nếu bạn chạy trong K8s/macvlan không có IPv6), DTLS certificate, loại ICE (mDNS, host candidates...).
- **API**: factory — `api.NewPeerConnection(config)`.

---

## 3. Tạo PeerConnection & exchange SDP (signaling tự làm)

**File: `peerconnection.go`** — function quan trọng: `NewPeerConnection`, `CreateOffer`, `CreateAnswer`, `SetLocalDescription`, `SetRemoteDescription`, `AddICECandidate`.

```go
pc, err := api.NewPeerConnection(webrtc.Configuration{
    ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
})
```

Pion **không cung cấp signaling server** — bạn tự làm transport cho SDP/ICE candidate (WebSocket, HTTP, SIP/SDP qua INVITE...). Với background IMS của bạn, đây chính là chỗ bạn có thể tái dùng SIP B2BUA: SDP offer/answer của WebRTC client map vào SDP body của INVITE/200OK.

Luồng (server là "answerer", phổ biến cho gateway):

```
Browser/Client                 Signaling (bạn tự viết)         Gateway (pion)
     | -- offer SDP -------------------->|                            |
     |                                    |-- SetRemoteDescription -->|
     |                                    |                            | CreateAnswer()
     |                                    |<---- answer SDP -----------|
     | <-- answer SDP --------------------|                            |
     | -- ICE candidates (trickle) ------>|-- AddICECandidate -------->|
     | <-- ICE candidates -----------------|<-- OnICECandidate ---------|
```

```go
pc.OnICECandidate(func(c *webrtc.ICECandidate) {
    if c == nil { return } // gathering done
    sendToClientViaSignaling(c.ToJSON())
})

offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: remoteOfferSDP}
_ = pc.SetRemoteDescription(offer)

answer, _ := pc.CreateAnswer(nil)
_ = pc.SetLocalDescription(answer)
sendToClientViaSignaling(*pc.LocalDescription())
```

> Để client gửi audio lên gateway, transceiver phải ở chiều `recvonly`/`sendrecv`. Nếu bạn chủ động tạo PeerConnection trước khi có offer (server-initiated), dùng `pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})`.

---

## 4. ICE — connectivity establishment

**File: `icetransport.go`** (wrap `pion/ice`)

- `pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState))` — theo dõi `Checking -> Connected -> Completed`. Nếu rơi về `Disconnected`/`Failed`, đó là dấu hiệu mất kết nối media — cần cleanup session ở Media AI Gateway (đóng gRPC stream sang AI).
- ICE agent thực hiện STUN binding request/response giữa các candidate pair (host/srflx/relay) để tìm đường đi tốt nhất. Với `SettingEngine.SetNetworkTypes`/`SetNAT1To1IPs`, bạn có thể ép server công bố IP cố định — quan trọng khi gateway nằm sau NAT/K8s service.
- **Risk on-prem**: nếu node không có public IP và không cấu hình TURN, client ngoài Internet sẽ không kết nối được (`ICEConnectionState=Failed`). Với macvlan + IP riêng cho media (mô hình bạn đang dùng cho IMS), candidate host IP chính là IP đó — cần đảm bảo SettingEngine không advertise địa chỉ pod-internal không route được.

---

## 5. DTLS — secure transport & key derivation cho SRTP

**File: `dtlstransport.go`** (wrap `pion/dtls`)

- Sau khi ICE connected, DTLS handshake chạy trên cùng kênh UDP (DTLS-SRTP, RFC 5764).
- `pc.OnConnectionStateChange` chuyển sang `Connected` khi cả ICE + DTLS xong.
- DTLS handshake xuất ra SRTP master key/salt — `dtlstransport.go` dùng kết quả này để khởi tạo SRTP context cho RTP/RTCP, và đồng thời là transport carrier cho SCTP (DataChannel chạy trên DTLS, không phải ICE trực tiếp).
- Đây chính là phần bạn đã quen từ "DTLS/SCTP" trong MTSI Data Channel — pion làm đúng RFC 8261 (SCTP over DTLS over UDP).

---

## 6. SRTP — bảo vệ RTP/RTCP

Không có file `srtp.go` riêng ở module chính (logic nằm trong `pion/srtp`, được `dtlstransport` wire vào). Về mặt application, bạn không cần đụng tới SRTP — `TrackRemote.ReadRTP()` trả về RTP packet đã decrypt, `TrackLocal.WriteRTP()` nhận RTP packet rồi tự encrypt trước khi gửi. Toàn bộ encrypt/decrypt là transparent.

---

## 7. Nhận audio: OnTrack callback & TrackRemote

**File: `peerconnection.go` (OnTrack), `rtpreceiver.go`, `trackremote.go`**

```go
pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
    log.Printf("Track: id=%s kind=%s codec=%s payloadType=%d ssrc=%d",
        track.ID(), track.Kind(), track.Codec().MimeType, track.PayloadType(), track.SSRC())

    go readRTPLoop(track) // QUAN TRỌNG: mỗi track 1 goroutine riêng
})
```

`OnTrack` được gọi khi pion nhận RTP packet đầu tiên cho một SSRC mới mà khớp với một transceiver đang ở chiều nhận. Callback này chạy trên goroutine nội bộ của pion — không block lâu, không gọi `pc.Close()`/đổi state đồng bộ trong này; chỉ nên `go func(){...}()` rồi return ngay.

### Đọc RTP bằng `TrackRemote.ReadRTP()`

```go
func readRTPLoop(track *webrtc.TrackRemote) {
    for {
        pkt, _, err := track.ReadRTP()
        if err != nil {
            // io.EOF khi track bị remove / PeerConnection đóng
            return
        }
        // pkt.Header: SequenceNumber, Timestamp, SSRC, PayloadType, Marker
        // pkt.Payload: bytes Opus đã decrypt, theo RFC 7587 (Opus RTP payload)
        handleOpusPayload(pkt.Payload, pkt.Header)
    }
}
```

- `ReadRTP()` block đến khi có packet mới hoặc track đóng. Nó đọc từ một internal buffer được interceptor chain ghi vào.
- `track.Kind()` = `audio`/`video`, `track.Codec()` cho biết MimeType/ClockRate — dùng để chọn decoder (Opus 48kHz stereo, theo cấu hình MediaEngine ở bước 2).
- Có `track.ReadRTCP()` qua `RTPReceiver` để lấy RTCP (sender reports...) nếu cần đo loss/jitter từ phía client.

> `RTPReceiver` (file `rtpreceiver.go`) là object quản lý lifecycle: `receiver.Track()` trả về `TrackRemote` hiện tại, `receiver.Stop()` để dừng.

---

## 8. Xử lý payload theo codec — Opus

RTP packet với `PayloadType` khớp Opus (theo `MediaEngine`, ví dụ PT 111) chứa 1 Opus frame trong `pkt.Payload` (RFC 7587 — không có header phụ ngoài RTP header chuẩn, trừ khi dùng DTX/FEC trong-band).

```go
import "gopkg.in/hraban/opus.v2" // hoặc binding libopus khác

decoder, _ := opus.NewDecoder(48000, 2) // sampleRate, channels - khớp track.Codec()
pcm := make([]int16, 960*2) // 20ms @ 48kHz stereo

n, err := decoder.Decode(pkt.Payload, pcm)
pcmFrame := pcm[:n*2]
// pcmFrame -> resample nếu AI yêu cầu 16kHz mono -> đẩy vào gRPC stream
```

**Lưu ý quan trọng (rủi ro #2 ở mục 14):**
- RTP không tự đảm bảo thứ tự/không mất gói. Nếu cần audio liên tục cho STT, bạn cần jitter buffer tối thiểu (reorder theo `SequenceNumber`, xử lý gap bằng silence/PLC) trước khi decode — Opus decoder có thể nhận biết packet loss qua `Decode` với input `nil` (PLC) nếu bạn track được seq bị mất.
- `pkt.Header.Marker` đánh dấu cuối 1 "talk spurt" (RFC 3551) — có thể dùng làm gợi ý ranh giới câu cho STT nhưng không đủ tin cậy để VAD.

---

## 9. Gửi media ra client — TrackLocal

**File: `tracklocal.go`, `tracklocal_static.go`, `rtpsender.go`**

Hai cách:

### 9a. `TrackLocalStaticSample` — bạn có raw sample (PCM/Opus đã encode theo frame), pion tự đóng gói RTP (tăng seq/timestamp tự động)

```go
audioTrack, err := webrtc.NewTrackLocalStaticSample(
    webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
    "tts-audio", "ai-gateway",
)
sender, err := pc.AddTrack(audioTrack)

// Khi có audio TTS trả về từ AI:
_ = audioTrack.WriteSample(media.Sample{
    Data:     opusFrameBytes,
    Duration: 20 * time.Millisecond,
})
```

### 9b. `TrackLocalStaticRTP` — bạn tự tạo `rtp.Packet` hoàn chỉnh (dùng khi forward/relay RTP từ nguồn khác, ví dụ pipeline RTP server bạn vừa xây)

```go
rtpTrack, _ := webrtc.NewTrackLocalStaticRTP(codecCap, "relay", "ai-gateway")
sender, _ := pc.AddTrack(rtpTrack)
_ = rtpTrack.WriteRTP(pkt) // pkt từ pipeline RTP nhận được
```

`rtpsender.go` (`RTPSender`) quản lý lifecycle gửi, có `sender.ReadRTCP()` để nhận RTCP feedback (NACK, REMB...) từ client phía bên kia — nếu bạn implement retransmission thì xử lý ở đây.

Sau `AddTrack`, cần renegotiate (CreateOffer mới) nếu track được add sau khi đã có 1 SDP O/A — `pc.OnNegotiationNeeded` báo hiệu việc này.

---

## 10. DataChannel — gửi transcript về client

**File: `datachannel.go`** (wrap `pion/sctp` + DCEP, chạy trên DTLS transport)

```go
// Server tạo channel (negotiated=false -> DCEP tự handshake)
dc, err := pc.CreateDataChannel("transcript", nil)

dc.OnOpen(func() {
    log.Println("data channel open, ready to push transcript")
})
dc.OnMessage(func(msg webrtc.DataChannelMessage) {
    // (tuỳ chọn) nhận control message từ client, ví dụ "start"/"stop" STT
})

// Khi AI trả transcript:
transcriptJSON, _ := json.Marshal(map[string]any{"text": "...", "final": true})
_ = dc.SendText(string(transcriptJSON))
```

Hoặc phía client tạo channel trước, server nhận qua:

```go
pc.OnDataChannel(func(dc *webrtc.DataChannel) {
    dc.OnOpen(func() { ... })
    dc.OnMessage(func(msg webrtc.DataChannelMessage) { ... })
})
```

> `SendText`/`Send` không block lâu nhưng không có backpressure tự nhiên về phía application nếu SCTP buffer đầy — pion sẽ buffer nội bộ; gửi quá nhanh (transcript streaming partial results mỗi 100ms) có thể tích buffer nếu client xử lý chậm. Nên có giới hạn rate hoặc theo dõi `dc.BufferedAmount()`.

---

## 11. Examples — điểm cần đọc

| Example | Trọng tâm | Áp dụng vào Media AI Gateway |
|---|---|---|
| `examples/rtp-forwarder` | Nhận RTP từ PeerConnection, forward ra UDP socket khác | Mẫu cho việc "fork" RTP sang pipeline xử lý riêng (decode/STT) thay vì UDP |
| `examples/rtp-to-webrtc` | Nhận RTP từ UDP, đẩy vào `TrackLocalStaticRTP`, gửi qua PeerConnection | Mẫu cho chiều "AI TTS -> client" nếu TTS audio ra ở dạng RTP |
| `examples/save-to-disk` | `OnTrack` -> `ReadRTP` -> ghi container (Ogg cho Opus, IVF cho VP8) | Pattern đọc RTP + đóng gói theo codec — đổi "ghi file" thành "đẩy decoder" |
| `examples/data-channels` | Tạo/nhận DataChannel, gửi/nhận message | Trực tiếp dùng cho kênh trả transcript |
| `examples/broadcast` | 1 input track -> N output tracks (fan-out) | Nếu cần fan-out audio cho nhiều consumer (STT + recording + ...) |

---

## 12. Sequence diagram tổng hợp — Media AI Gateway

```
Client (Browser)      Signaling(bạn)        Gateway (pion/webrtc)        STT/AI (gRPC)
      |                    |                        |                         |
      | 1. getUserMedia +  |                        |                         |
      |    createOffer     |                        |                         |
      |---- SDP offer ---->|                        |                         |
      |                    |-- SetRemoteDescription>|                         |
      |                    |   AddTransceiver(audio, recvonly)                |
      |                    |   CreateAnswer/SetLocalDescription               |
      |                    |<---- SDP answer -------|                         |
      |<---- SDP answer ---|                        |                         |
      | 2. ICE candidates (trickle, 2 chieu) ------->|                         |
      |                    |                        |                         |
      | ===== ICE connectivity check (STUN) =====   |                         |
      | ===== DTLS handshake -> SRTP keys   =====   |                         |
      |                    |                        |                         |
      | 3. RTP audio (SRTP) ------------------------>|                         |
      |                    |                  OnTrack() fired                 |
      |                    |                  go readRTPLoop()                |
      |                    |                  ReadRTP() loop                  |
      |                    |                  unmarshal Opus payload          |
      |                    |                  decode Opus -> PCM              |
      |                    |                  resample/chunk                  |
      |                    |                  ----- AudioChunk -------------->|
      |                    |                                      (gRPC stream)
      |                    |                                            | STT decode
      |                    |                  <---- PartialTranscript --|
      |                    |                  dc.SendText(json)         |
      |<=== DataChannel: {"text":"...","final":false} =================|
      |                    |                  <---- FinalTranscript ----|
      |<=== DataChannel: {"text":"...","final":true} ===================|
      |                    |                        |                         |
      | (tuy chon) 4. AI tra audio dich (TTS)        |                         |
      |                    |                  <---- TTS audio chunk ----------|
      |                    |                  encode Opus -> WriteSample()    |
      |<-- RTP audio (SRTP, track moi) ---------------|                         |
```

---

## 13. Điểm hook để tích hợp AI STT — checklist code

1. **MediaEngine**: cố định `PayloadType` cho Opus (ví dụ 111) để biết payload nào cần decode — `mediaengine.go`.
2. **PeerConnection setup**: nếu server là callee, `AddTransceiverFromKind(audio, recvonly)` trước `CreateAnswer` — `peerconnection.go`.
3. **`OnTrack`**: entrypoint duy nhất để bắt audio track — spawn goroutine riêng, gắn `context` theo session để cancel khi `OnConnectionStateChange` -> `Closed/Failed`.
4. **`TrackRemote.ReadRTP()`**: hot loop — copy `pkt.Payload` nếu cần giữ sau khi loop tiếp tục (giống lưu ý ở RTP server bạn vừa build).
5. **Decode Opus**: dùng `pion/opus` (pure Go, không cần cgo/libopus — phù hợp build trong container minimal) hoặc `hraban/opus` (cgo, nhanh hơn nhưng cần libopus.so).
6. **gRPC streaming sang AI**: 1 goroutine riêng nhận PCM từ channel, gọi `stream.Send(&AudioChunk{...})`; 1 goroutine khác `stream.Recv()` nhận transcript — không gọi gRPC trực tiếp trong `ReadRTP` loop (block sẽ làm rớt packet RTP tiếp theo, vì jitter buffer nội bộ pion có giới hạn).
7. **Trả transcript**: `DataChannel.SendText()` — tạo channel ngay sau `NewPeerConnection`, trước khi `CreateAnswer`, để channel có sẵn trong SDP đầu tiên (negotiated qua SCTP sau khi DTLS xong).
8. **Cleanup**: `pc.OnConnectionStateChange` -> khi `Closed/Failed/Disconnected`, đóng gRPC stream, cancel context, để goroutine `ReadRTP` tự thoát qua `io.EOF`.

---

## 14. Rủi ro & vấn đề cần lường trước

**Signaling tự làm**: pion không cung cấp signaling — bạn chịu trách nhiệm transport SDP/ICE candidates (WebSocket/HTTP/SIP). Lỗi đồng bộ thứ tự `SetRemoteDescription`/`SetLocalDescription` (glare, rollback khi cả 2 bên offer cùng lúc) là nguồn bug phổ biến — cần state machine rõ ràng phía signaling, đặc biệt nếu tích hợp với SIP B2BUA hiện có.

**Codec/decode**: Opus decode tốn CPU đáng kể ở quy mô nhiều session đồng thời (mỗi 20ms 1 frame/stream). Pure-Go decoder (`pion/opus`) dễ deploy nhưng có thể chậm hơn cgo binding — cần benchmark theo target concurrency trước khi chọn.

**Jitter buffer**: pion cung cấp interceptor cơ bản (NACK, RTCP) nhưng không có jitter buffer hoàn chỉnh kiểu WebRTC client cho audio playout timing. Nếu thứ tự/độ trễ giữa packet quan trọng cho STT (ảnh hưởng đến segmentation), bạn cần buffer riêng theo `SequenceNumber`/`Timestamp` trước khi decode, giống `SessionTable` bạn đã thiết kế ở RTP server.

**Backpressure**: `ReadRTP()` blocking, gRPC `Send`/decode chậm sẽ làm goroutine đọc bị giữ lâu, nếu pion buffer nội bộ đầy, packet mới có thể bị drop âm thầm. Thiết kế theo mẫu: `ReadRTP` -> push vào channel có buffer cố định -> worker riêng xử lý decode+gRPC, drop + log khi đầy (đúng pattern bạn đã áp dụng ở `WorkerPool`).

**Goroutine leak**: mỗi `OnTrack` spawn 1 goroutine `ReadRTP` — nếu không có đường thoát khi PeerConnection đóng bất thường (ví dụ network drop không trigger `Closed` ngay), goroutine có thể leak chờ `ReadRTP` block vô hạn. Luôn gắn `context` per-session và đảm bảo `pc.Close()` được gọi (timeout dựa trên `ICEConnectionState=Disconnected` kéo dài).

**Packet loss**: RTP qua UDP — loss là bình thường. STT chất lượng phụ thuộc PLC (packet loss concealment) của Opus decoder; cần track `SequenceNumber` gap để gọi decode với "lost frame" flag đúng cách, tránh audio bị "dính" sai gây nhiễu transcript.

**Scaling**: mỗi PeerConnection mở ICE candidates trên `SettingEngine.SetEphemeralUDPPortRange` — với nhiều session đồng thời trên K8s on-prem, cần tính toán port range đủ lớn và cấu hình macvlan/hostNetwork tương tự RTP server đã thiết kế. gRPC stream tới AI nên dùng connection pooling/multiplexing (HTTP/2 đã hỗ trợ multiplexing nhiều stream trên 1 connection) để tránh mở quá nhiều TCP connection khi số session lớn — đây là điểm bạn đã quen với HTTP/2 Gateway trong IMS, có thể tái dùng kinh nghiệm đó.

---

## 15. Đề xuất cấu trúc package cho Media AI Gateway

```
/internal/
  signaling/        # WebSocket/HTTP/SIP <-> SDP/ICE exchange
  gateway/
    session.go      # 1 struct/session: pc, audioTrack, dataChannel, gRPC stream, context
    ontrack.go       # OnTrack handler, spawn readRTPLoop
    rtploop.go       # ReadRTP -> jitter reorder -> channel
  audio/
    opusdecoder.go   # decode + PLC + resample
  ai/
    sttclient.go     # gRPC bidi stream wrapper: SendAudio / RecvTranscript
  transcript/
    sender.go        # format JSON, DataChannel.SendText, rate limiting
```

Mỗi `Session` đóng vai trò giống "session process" trong supervisor bạn đã thiết kế ở RTP server — `SessionSupervisor` quản lý map session theo call-id từ signaling, "one_for_one" khi 1 session lỗi không ảnh hưởng session khác.
