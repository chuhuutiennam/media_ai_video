# RTP → AI Pipeline — Luồng dữ liệu đầy đủ

Tài liệu này mô tả toàn bộ đường đi của một RTP packet từ khi nhận trên UDP socket cho đến khi kết quả ASR được gửi về callback.

---

## Tổng quan

```
UDP socket
  → rawrtp.Ingress (parse + route)
  → sess.PacketQueue
  → coordinator (jitter pump)
  → pipeline.WorkerPool (decode → resample → chunk)
  → sess.AudioQueue
  → ai.Manager (gRPC stream)
  → AI worker  /speech.SpeechStream/Recognize
  → sess.ResultQueue
  → result.Dispatcher → HTTPCallbackSink / DataChannelSink
```

---

## 1. UDP Ingress — `rawrtp.Ingress.Run(ctx)`

**File:** `internal/ingress/rawrtp/udp_server.go`, `packet_handler.go`

```
Ingress.Run(ctx)
  └─ serve(ctx, conn)
       └─ conn.ReadFromUDP(buf)          ← nhận datagram UDP
              └─ handle(buf[:n], remoteAddr)
                     ├─ pionrtp.Header.Unmarshal()   ← parse RTP header
                     ├─ router.RouteBySSRC(hdr.SSRC) ← lookup session bằng SSRC
                     │    (fallback) RouteByAddr()    ← lookup bằng remote IP:port
                     └─ queue <- MediaPacket          ← push vào sess.PacketQueue
```

- Drop policy: parse error → `DroppedParseError`; SSRC/addr không khớp → `DroppedUnknownSSRC`; queue đầy → `DroppedQueueFull`
- Receive loop không bao giờ block — UDP socket được drain liên tục
- `SO_RCVBUF` = 2 MiB để hấp thụ burst

---

## 2. `sess.PacketQueue` → Jitter Buffer

**File:** `internal/coordinator/coordinator.go` — `startJitterPump()`

3 goroutine per-session:

| Goroutine | Việc làm |
|---|---|
| **packet pump** | `sess.PacketQueue` → `jitter.Buffer.Push()` + `sess.Touch()` |
| **jitter flush** | `buf.Run(ctx, jitterOut)` — flush mỗi 20ms (PacketTimeMs) |
| **submit pump** | `jitterOut` → `WorkerPool.Submit()` (non-blocking, drop nếu pool queue đầy) |

Jitter buffer (`internal/jitter/buffer.go`): min-heap reorder theo sequence number, loss detection, MaxLateMs=120ms drop, RFC 3550 jitter metric.

---

## 3. `WorkerPool.Submit()` → `sessionPipeline.process()`

**File:** `internal/pipeline/worker_pool.go`, `audio_pipeline.go`

```
Submit(job)
  → wp.queue  (chan AudioJob, cap 8192, non-blocking)
    ↓ 1 trong 16 worker goroutine
  sessionPipeline.process(pkt)
    ├─ codec.Decode(payload)        → []int16  (PCMU/PCMA/Opus/AMR)
    ├─ resampler.Resample(pcm)      → []int16  (→ 16kHz mono)
    └─ chunker.Push(resampled, ts)  → []AudioChunk (500ms each)
         ↓
       sess.AudioQueue
```

- `sync.Mutex` per-session: bảo vệ resampler (phase accumulator) và chunker (PCM buffer)
- PCM dump: nếu `PCMDumpDir` được set, ghi raw `int16-LE` ra file để debug
- Khi session đóng: `flush()` emit partial chunk còn lại với `EndOfStream=true`

---

## 4. `sess.AudioQueue` → AI gRPC Stream

**File:** `internal/ai/stream_manager.go`, `grpc_client.go`

`ai.Manager.Open()` khởi động 3 goroutine per-session:

**Bridge goroutine** (khi `QueueSize=20`):
```
sess.AudioQueue → q (cap 20, drop nếu đầy) → sendInput
```

**`runSend()` goroutine:**
```
sendInput → sendWithTimeout(500ms) → client.Send(chunk)
```
- `FirstChunkTimeout` = 3s: thoát nếu không nhận chunk nào sau 3s stream mở
- `chunk.EndOfStream=true` → đánh dấu `endOfStreamSent`, trigger `CloseSend()`

**`runRecv()` goroutine** (song song với send):
```
client.Recv() → sess.ResultQueue
```
- `RecvIdleTimeout` = 30s: thoát nếu không nhận result trong 30s
- Partial (`IsFinal=false`): drop nếu `ResultQueue` đầy (non-blocking)
- Final (`IsFinal=true`): block cho đến khi deliver

**Reconnect:** lỗi → exponential backoff (1s → 2s → 4s → max 30s), tối đa 3 lần.

---

## 5. `client.Send()` — Wire encoding

**File:** `internal/ai/grpc_dialer.go`

```
AudioChunk{PCM, SampleRate, Channels, RTPTimestamp, DurationMs, EndOfStream}
  ↓ grpcStreamClient.Send()
audioChunkWire  (+ Language, Task từ stream context)
  ↓ protoCodec.Marshal() → marshalAudioChunk()
protobuf binary (protowire thủ công, không codegen):
  field 1  bytes   SessionID
  field 2  bytes   StreamID
  field 3  bytes   PCM  (raw int16-LE)
  field 4  varint  SampleRate
  field 5  varint  Channels
  field 6  varint  RTPTimestamp
  field 7  varint  DurationMs
  field 8  varint  EndOfStream (1/0)
  field 9  bytes   Language
  field 10 bytes   Task
  ↓ gRPC ClientStream.SendMsg()
HTTP/2 DATA frame → AI worker  /speech.SpeechStream/Recognize
```

**Connection model — `SharedConnPool`:**
```
1 addr → 1 grpc.ClientConn (1 TCP/HTTP2 connection)
               ↓
         N session × 1 ClientStream  (HTTP/2 multiplexing)
```
- Lazy-init per addr, thread-safe
- `Preconnect()` trigger async TCP dial khi app start
- `WatchAndReconnect()` goroutine: phát hiện `TRANSIENT_FAILURE`/`IDLE` → `conn.Connect()` ngay

---

## 6. Kết quả ASR → Dispatcher

**File:** `internal/coordinator/coordinator.go` — `resultPump()`

```
sess.ResultQueue
  ↓ resultPump goroutine
result.Dispatcher.Push()
  ↓
  ├─ HTTPCallbackSink  (H/2 h2c+TLS, retry 5xx, dead-letter)
  └─ DataChannelSink   (WebRTC DataChannel, backpressure)
```

---

## Toàn bộ pipeline (one-liner)

```
UDP → Ingress → PacketQueue → JitterBuffer → WorkerPool → AudioQueue → gRPC → AI worker → ResultQueue → Dispatcher → Callback/DataChannel
```
