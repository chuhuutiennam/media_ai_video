package ai

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/encoding/protowire"

	"media-ai-gateway/internal/pipeline"
)

// recognizeRPC là full gRPC method path phía client cần gọi.
const recognizeRPC = "/speech.SpeechStream/Recognize"

// protoCodec encode AudioChunk và decode RecognitionResult theo protobuf binary.
// Content-Type trên wire: "application/grpc+proto" (chuẩn gRPC).
type protoCodec struct{}

func (protoCodec) Name() string { return "proto" }

func (protoCodec) Marshal(v any) ([]byte, error) {
	m, ok := v.(*audioChunkWire)
	if !ok {
		return nil, fmt.Errorf("protoCodec: unsupported marshal type %T", v)
	}
	return marshalAudioChunk(m), nil
}

func (protoCodec) Unmarshal(data []byte, v any) error {
	m, ok := v.(*recognitionResultWire)
	if !ok {
		return fmt.Errorf("protoCodec: unsupported unmarshal type %T", v)
	}
	return unmarshalRecognitionResult(data, m)
}

// marshalAudioChunk encode AudioChunk theo protobuf binary (field numbers từ speech.proto).
func marshalAudioChunk(m *audioChunkWire) []byte {
	var b []byte
	if m.SessionID != "" {
		b = protowire.AppendTag(b, 1, protowire.BytesType)
		b = protowire.AppendString(b, m.SessionID)
	}
	if m.StreamID != "" {
		b = protowire.AppendTag(b, 2, protowire.BytesType)
		b = protowire.AppendString(b, m.StreamID)
	}
	if len(m.PCM) > 0 {
		b = protowire.AppendTag(b, 3, protowire.BytesType)
		b = protowire.AppendBytes(b, m.PCM)
	}
	if m.SampleRate != 0 {
		b = protowire.AppendTag(b, 4, protowire.VarintType)
		b = protowire.AppendVarint(b, uint64(int32(m.SampleRate)))
	}
	if m.Channels != 0 {
		b = protowire.AppendTag(b, 5, protowire.VarintType)
		b = protowire.AppendVarint(b, uint64(int32(m.Channels)))
	}
	if m.RTPTimestamp != 0 {
		b = protowire.AppendTag(b, 6, protowire.VarintType)
		b = protowire.AppendVarint(b, uint64(m.RTPTimestamp))
	}
	if m.DurationMs != 0 {
		b = protowire.AppendTag(b, 7, protowire.VarintType)
		b = protowire.AppendVarint(b, uint64(m.DurationMs))
	}
	if m.EndOfStream {
		b = protowire.AppendTag(b, 8, protowire.VarintType)
		b = protowire.AppendVarint(b, 1)
	}
	if m.Language != "" {
		b = protowire.AppendTag(b, 9, protowire.BytesType)
		b = protowire.AppendString(b, m.Language)
	}
	if m.Task != "" {
		b = protowire.AppendTag(b, 10, protowire.BytesType)
		b = protowire.AppendString(b, m.Task)
	}
	if m.RTPClockRate != 0 {
		b = protowire.AppendTag(b, 11, protowire.VarintType)
		b = protowire.AppendVarint(b, uint64(int32(m.RTPClockRate)))
	}
	if m.ChunkSeq != 0 {
		b = protowire.AppendTag(b, 12, protowire.VarintType)
		b = protowire.AppendVarint(b, m.ChunkSeq)
	}
	return b
}

// unmarshalRecognitionResult decode protobuf binary → RecognitionResult.
func unmarshalRecognitionResult(b []byte, m *recognitionResultWire) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.BytesType:
			v, n := protowire.ConsumeString(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.SessionID = v
			b = b[n:]
		case num == 2 && typ == protowire.BytesType:
			v, n := protowire.ConsumeString(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.StreamID = v
			b = b[n:]
		case num == 3 && typ == protowire.BytesType:
			v, n := protowire.ConsumeString(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.Text = v
			b = b[n:]
		case num == 4 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.IsFinal = v != 0
			b = b[n:]
		case num == 5 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.TsStart = int64(v)
			b = b[n:]
		case num == 6 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.TsEnd = int64(v)
			b = b[n:]
		case num == 7 && typ == protowire.Fixed32Type:
			v, n := protowire.ConsumeFixed32(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.Confidence = math.Float32frombits(v)
			b = b[n:]
		case num == 8 && typ == protowire.BytesType:
			v, n := protowire.ConsumeString(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.Language = v
			b = b[n:]
		case num == 9 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.Seq = v
			b = b[n:]
		case num == 10 && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.AudioPayload = append([]byte(nil), v...)
			b = b[n:]
		case num == 11 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.AudioPT = uint32(v)
			b = b[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			b = b[n:]
		}
	}
	return nil
}

// audioChunkWire là wire type của AudioChunk gửi tới AI worker.
// Language và Task không có trong pipeline.AudioChunk nên được truyền riêng.
type audioChunkWire struct {
	SessionID    string
	StreamID     string
	PCM          []byte
	SampleRate   int
	Channels     int
	RTPTimestamp uint32
	DurationMs   int64
	EndOfStream  bool
	Language     string
	Task         string
	RTPClockRate int    // field 11: clock rate của RTP stream gốc (Hz)
	ChunkSeq     uint64 // field 12: monotonic sequence per stream
}

// recognitionResultWire là wire type nhận từ AI worker.
type recognitionResultWire struct {
	SessionID    string
	StreamID     string
	Text         string
	IsFinal      bool
	TsStart      int64
	TsEnd        int64
	Confidence   float32
	Language     string
	Seq          uint64
	AudioPayload []byte // field 10: audio đã encode để gateway gửi về MF qua RTP
	AudioPT      uint32 // field 11: RTP payload type của AudioPayload
}

// SharedConnPool giữ một *grpc.ClientConn dùng chung per AI worker address.
// Tất cả session tới cùng địa chỉ tái dùng 1 kết nối HTTP/2 duy nhất — mỗi session
// chỉ mở thêm 1 grpc.ClientStream mới trên connection đó (HTTP/2 multiplexing).
//
// Thread-safe. Conn được tạo lazy khi địa chỉ đầu tiên được dial.
type SharedConnPool struct {
	mu    sync.Mutex
	conns map[string]*grpc.ClientConn
	opts  []grpc.DialOption
}

// NewSharedConnPool tạo pool với keepalive params.
//   - keepaliveTime: gửi HTTP/2 PING sau khoảng idle này (0 = không PING).
//   - keepaliveTimeout: đóng conn nếu không nhận PONG sau khoảng này.
func NewSharedConnPool(keepaliveTime, keepaliveTimeout time.Duration) *SharedConnPool {
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}
	if keepaliveTime > 0 {
		opts = append(opts, grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                keepaliveTime,
			Timeout:             keepaliveTimeout,
			PermitWithoutStream: false, // chỉ PING khi có stream active — tránh ENHANCE_YOUR_CALM
		}))
	}
	return &SharedConnPool{
		conns: make(map[string]*grpc.ClientConn),
		opts:  opts,
	}
}

// getOrCreate trả về conn hiện có hoặc tạo conn mới cho addr.
func (p *SharedConnPool) getOrCreate(addr string) (*grpc.ClientConn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if conn, ok := p.conns[addr]; ok {
		return conn, nil
	}
	conn, err := grpc.NewClient(addr, p.opts...)
	if err != nil {
		return nil, err
	}
	p.conns[addr] = conn
	return conn, nil
}

// Preconnect khởi tạo và trigger async TCP dial tới addr ngay khi app start.
// Non-fatal: chỉ log nếu lỗi — gateway vẫn hoạt động, gRPC retry khi có session đầu tiên.
func (p *SharedConnPool) Preconnect(addr string) {
	conn, err := p.getOrCreate(addr)
	if err != nil {
		zap.L().Warn("ai: grpc preconnect failed", zap.String("addr", addr), zap.Error(err))
		return
	}
	conn.Connect() // trigger async dial; gRPC tự retry nếu server chưa sẵn sàng
	zap.L().Info("ai: gRPC connection initiated", zap.String("addr", addr), zap.Stringer("state", conn.GetState()))
}

// State trả về trạng thái kết nối tới addr (dùng để health check / logging).
func (p *SharedConnPool) State(addr string) connectivity.State {
	p.mu.Lock()
	conn, ok := p.conns[addr]
	p.mu.Unlock()
	if !ok {
		return connectivity.Idle
	}
	return conn.GetState()
}

// WatchAndReconnect theo dõi trạng thái kết nối tới addr và gọi conn.Connect()
// ngay khi phát hiện TRANSIENT_FAILURE — thay vì đợi gRPC built-in backoff.
// Block cho đến khi ctx bị cancel hoặc conn bị Shutdown.
// Nên gọi dưới dạng goroutine sau Preconnect.
func (p *SharedConnPool) WatchAndReconnect(ctx context.Context, addr string) {
	p.mu.Lock()
	conn, ok := p.conns[addr]
	p.mu.Unlock()
	if !ok {
		zap.L().Warn("ai: grpc watch: no connection", zap.String("addr", addr))
		return
	}

	state := conn.GetState()
	for {
		if !conn.WaitForStateChange(ctx, state) {
			return // ctx cancelled
		}
		state = conn.GetState()
		zap.L().Debug("ai: grpc state change", zap.String("addr", addr), zap.Stringer("state", state))
		switch state {
		case connectivity.Idle:
			// Sau GOAWAY (graceful disconnect), conn về IDLE — reconnect ngay.
			conn.Connect()
		case connectivity.TransientFailure:
			zap.L().Warn("ai: grpc transient failure, triggering reconnect", zap.String("addr", addr))
			conn.Connect()
		case connectivity.Shutdown:
			zap.L().Error("ai: grpc connection shutdown", zap.String("addr", addr))
			return
		}
	}
}

// Addrs trả về danh sách địa chỉ đã có conn trong pool.
func (p *SharedConnPool) Addrs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	addrs := make([]string, 0, len(p.conns))
	for addr := range p.conns {
		addrs = append(addrs, addr)
	}
	return addrs
}

// DialFunc trả về GRPCDialFunc mở grpc.ClientStream mới trên shared conn.
// Wire encoding: protobuf binary (Content-Type: application/grpc+proto).
func (p *SharedConnPool) DialFunc() GRPCDialFunc {
	return func(ctx context.Context, workerAddr, _, _, language, task string) (StreamClient, error) {
		conn, err := p.getOrCreate(workerAddr)
		if err != nil {
			return nil, fmt.Errorf("grpc: connect %s: %w", workerAddr, err)
		}
		desc := &grpc.StreamDesc{
			StreamName:    "Recognize",
			ClientStreams: true,
			ServerStreams: true,
		}
		cs, err := conn.NewStream(ctx, desc, recognizeRPC, grpc.ForceCodec(protoCodec{}))
		if err != nil {
			return nil, fmt.Errorf("grpc: open stream to %s: %w", workerAddr, err)
		}
		return &grpcStreamClient{cs: cs, language: language, task: task}, nil
	}
}

// grpcStreamClient wraps gRPC ClientStream thành StreamClient interface.
// conn được quản lý bởi SharedConnPool — không đóng ở đây.
type grpcStreamClient struct {
	cs        grpc.ClientStream
	language  string
	task      string
	firstSent atomic.Bool
}

func (c *grpcStreamClient) Send(chunk pipeline.AudioChunk) error {
	if c.firstSent.CompareAndSwap(false, true) {
		zap.L().Debug("ai: first chunk sent to worker",
			zap.String("session_id", chunk.SessionID),
			zap.String("stream_id", chunk.StreamID),
			zap.String("language", c.language),
			zap.String("task", c.task),
			zap.Int("pcm_bytes", len(chunk.PCM)),
			zap.Int("sample_rate", chunk.SampleRate),
			zap.Bool("end_of_stream", chunk.EndOfStream),
		)
	}
	return c.cs.SendMsg(&audioChunkWire{
		SessionID:    chunk.SessionID,
		StreamID:     chunk.StreamID,
		PCM:          chunk.PCM,
		SampleRate:   chunk.SampleRate,
		Channels:     chunk.Channels,
		RTPTimestamp: chunk.RTPTimestamp,
		DurationMs:   chunk.DurationMs,
		EndOfStream:  chunk.EndOfStream,
		Language:     c.language,
		Task:         c.task,
		ChunkSeq:     chunk.ChunkSeq,
		RTPClockRate: chunk.RTPClockRate,
	})
}

func (c *grpcStreamClient) Recv() (pipeline.RecognitionResult, error) {
	var w recognitionResultWire
	if err := c.cs.RecvMsg(&w); err != nil {
		return pipeline.RecognitionResult{}, err
	}
	return pipeline.RecognitionResult{
		SessionID:    w.SessionID,
		StreamID:     w.StreamID,
		Text:         w.Text,
		IsFinal:      w.IsFinal,
		TsStart:      w.TsStart,
		TsEnd:        w.TsEnd,
		Confidence:   w.Confidence,
		Language:     w.Language,
		Seq:          w.Seq,
		AudioPayload: w.AudioPayload,
		AudioPT:      uint8(w.AudioPT), //nolint:gosec
	}, nil
}

// CloseSend gửi gRPC half-close (END_STREAM) lên server.
func (c *grpcStreamClient) CloseSend() error {
	return c.cs.CloseSend()
}

// Close là no-op: conn được giữ bởi SharedConnPool và tái dùng qua các session.
func (c *grpcStreamClient) Close() error { return nil }
