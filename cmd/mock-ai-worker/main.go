// Command mock-ai-worker là giả lập AI worker cho môi trường lab/dev.
//
// Implement gRPC service /speech.SpeechStream/Recognize với protobuf codec
// (tương thích gateway). Với mỗi session:
//   - Mỗi 3 AudioChunk nhận được → gửi 1 partial result
//   - Mỗi 6 AudioChunk nhận được → gửi 1 final result
//   - Khi end_of_stream=true     → gửi final result và đóng stream
//
// Video: /video.VideoStream/Segment nhận JPEG và trả mask RLE theo ngưỡng độ sáng.
//
// Usage:
//
//	mock-ai-worker [--addr :50051]
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"media-ai-gateway/internal/mockai"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// ── Protobuf codec ────────────────────────────────────────────────────────────

type protoCodec struct{}

func (protoCodec) Name() string { return "proto" }

func (protoCodec) Marshal(v any) ([]byte, error) {
	switch m := v.(type) {
	case *recognitionResultWire:
		return marshalRecognitionResult(m), nil
	case proto.Message:
		return proto.Marshal(m)
	default:
		return nil, fmt.Errorf("protoCodec: unsupported marshal type %T", v)
	}
}

func (protoCodec) Unmarshal(data []byte, v any) error {
	switch m := v.(type) {
	case *audioChunkWire:
		return unmarshalAudioChunk(data, m)
	case proto.Message:
		return proto.Unmarshal(data, m)
	default:
		return fmt.Errorf("protoCodec: unsupported unmarshal type %T", v)
	}
}

func init() { encoding.RegisterCodec(protoCodec{}) }

// ── Wire types ────────────────────────────────────────────────────────────────

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
}

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
	AudioPayload []byte // field 10: PCMU audio để gateway gửi về MF qua RTP egress
	AudioPT      uint32 // field 11: payload type (0 = PCMU)
}

// ── Protobuf encode/decode ────────────────────────────────────────────────────

func unmarshalAudioChunk(b []byte, m *audioChunkWire) error {
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
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.PCM = append(m.PCM[:0], v...)
			b = b[n:]
		case num == 4 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.SampleRate = int(int32(v))
			b = b[n:]
		case num == 5 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.Channels = int(int32(v))
			b = b[n:]
		case num == 6 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.RTPTimestamp = uint32(v)
			b = b[n:]
		case num == 7 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.DurationMs = int64(v)
			b = b[n:]
		case num == 8 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.EndOfStream = v != 0
			b = b[n:]
		case num == 9 && typ == protowire.BytesType:
			v, n := protowire.ConsumeString(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.Language = v
			b = b[n:]
		case num == 10 && typ == protowire.BytesType:
			v, n := protowire.ConsumeString(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.Task = v
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

func marshalRecognitionResult(m *recognitionResultWire) []byte {
	var b []byte
	if m.SessionID != "" {
		b = protowire.AppendTag(b, 1, protowire.BytesType)
		b = protowire.AppendString(b, m.SessionID)
	}
	if m.StreamID != "" {
		b = protowire.AppendTag(b, 2, protowire.BytesType)
		b = protowire.AppendString(b, m.StreamID)
	}
	if m.Text != "" {
		b = protowire.AppendTag(b, 3, protowire.BytesType)
		b = protowire.AppendString(b, m.Text)
	}
	if m.IsFinal {
		b = protowire.AppendTag(b, 4, protowire.VarintType)
		b = protowire.AppendVarint(b, 1)
	}
	if m.TsStart != 0 {
		b = protowire.AppendTag(b, 5, protowire.VarintType)
		b = protowire.AppendVarint(b, uint64(m.TsStart))
	}
	if m.TsEnd != 0 {
		b = protowire.AppendTag(b, 6, protowire.VarintType)
		b = protowire.AppendVarint(b, uint64(m.TsEnd))
	}
	if m.Confidence != 0 {
		b = protowire.AppendTag(b, 7, protowire.Fixed32Type)
		b = protowire.AppendFixed32(b, math.Float32bits(m.Confidence))
	}
	if m.Language != "" {
		b = protowire.AppendTag(b, 8, protowire.BytesType)
		b = protowire.AppendString(b, m.Language)
	}
	if m.Seq != 0 {
		b = protowire.AppendTag(b, 9, protowire.VarintType)
		b = protowire.AppendVarint(b, m.Seq)
	}
	if len(m.AudioPayload) > 0 {
		b = protowire.AppendTag(b, 10, protowire.BytesType)
		b = protowire.AppendBytes(b, m.AudioPayload)
	}
	if m.AudioPT != 0 {
		b = protowire.AppendTag(b, 11, protowire.VarintType)
		b = protowire.AppendVarint(b, uint64(m.AudioPT))
	}
	return b
}

// pcmuSilence tạo N × 160 bytes PCMU silence (µ-law encoding của 0 = 0xFF).
func pcmuSilence(nPackets int) []byte {
	buf := make([]byte, nPackets*160)
	for i := range buf {
		buf[i] = 0xFF
	}
	return buf
}

// linearToMulaw encodes một sample PCM16 signed thành 1 byte G.711 µ-law.
func linearToMulaw(sample int16) byte {
	const (
		bias = 0x84
		clip = 32635
	)
	sign := 0
	s := int(sample)
	if s < 0 {
		s = -s
		sign = 0x80
	}
	if s > clip {
		s = clip
	}
	s += bias
	exp := 7
	for ; exp > 0 && s&0x4000 == 0; exp-- {
		s <<= 1
	}
	mantissa := (s >> 10) & 0x0F
	return byte(^(sign | (exp << 4) | mantissa))
}

// pcm16ToMulaw convert raw PCM16 signed little-endian sang PCMU (G.711 µ-law).
func pcm16ToMulaw(raw []byte) []byte {
	nSamples := len(raw) / 2
	out := make([]byte, nSamples)
	for i := range nSamples {
		s := int16(binary.LittleEndian.Uint16(raw[i*2:]))
		out[i] = linearToMulaw(s)
	}
	return out
}

// pcmuFiles chứa PCMU audio đã load từ --pcm-dir; round-robin qua các stream.
var pcmuFiles [][]byte

// sessionCounter dùng để chọn file round-robin per session.
var sessionCounter atomic.Uint64

// loadPCMDir đọc tất cả file trong dir:
//   - *.pcmu         → dùng trực tiếp
//   - *.s16le / *.pcm → convert PCM16LE → PCMU
func loadPCMDir(dir string) ([][]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})
	var files [][]byte
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		// cũng handle đuôi kép như .8000hz.1ch.s16le
		isS16le := ext == ".s16le" || strings.HasSuffix(strings.ToLower(name), ".s16le")
		isPCMU := ext == ".pcmu" || strings.HasSuffix(strings.ToLower(name), ".pcmu")
		if !isS16le && !isPCMU {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			zap.L().Warn("pcm-dir: skip unreadable file", zap.String("file", name), zap.Error(err))
			continue
		}
		var pcmu []byte
		if isS16le {
			pcmu = pcm16ToMulaw(raw)
		} else {
			pcmu = raw
		}
		files = append(files, pcmu)
		zap.L().Info("pcm-dir: loaded", zap.String("file", name), zap.Int("pcmu_bytes", len(pcmu)))
	}
	return files, nil
}

// nextAudioChunk trả về đoạn PCMU tiếp theo để gửi kèm result.
// Nếu sessionAudio còn dữ liệu: trả về audioPackets×160 bytes (hoặc phần còn lại nếu ít hơn).
// Nếu sessionAudio hết/rỗng và audioPackets > 0: trả về silence.
// audioOffset được cập nhật sau mỗi lần gọi.
func nextAudioChunk(sessionAudio []byte, offset *int, nPackets int) []byte {
	if len(sessionAudio) > 0 && *offset < len(sessionAudio) {
		size := nPackets * 160
		if size <= 0 {
			size = 160
		}
		end := *offset + size
		if end > len(sessionAudio) {
			end = len(sessionAudio)
		}
		chunk := sessionAudio[*offset:end]
		*offset = end
		return chunk
	}
	if nPackets > 0 {
		return pcmuSilence(nPackets)
	}
	return nil
}

// ── Mock phrases ──────────────────────────────────────────────────────────────

var partialPhrases = []string{
	"xin chào...",
	"đây là kết quả...",
	"đang nhận dạng giọng nói...",
	"âm thanh đang được xử lý...",
}

var finalPhrases = []string{
	"Xin chào, tôi cần hỗ trợ.",
	"Cuộc gọi đến từ khách hàng.",
	"Vui lòng chờ trong giây lát.",
	"Cảm ơn bạn đã liên hệ.",
	"Tôi muốn biết thêm thông tin.",
}

// ── gRPC handler ──────────────────────────────────────────────────────────────

// audioPackets là số 20ms PCMU packet (× 160 bytes) đính kèm mỗi final result
// khi không có pcm-dir. 0 = không gửi audio.
var audioPackets int

func recognizeHandler(_ any, stream grpc.ServerStream) error {
	var (
		chunkCount uint64
		seq        uint64
		sessionID  string
		streamID   string
		language   string
		startRTPTs uint32
	)

	// Chọn audio source cho session này.
	// Ưu tiên pcmuFiles (--pcm-dir); fallback về pcmuSilence (--audio-packets).
	var sessionAudio []byte // toàn bộ PCMU của session
	var audioOffset int     // byte đã gửi trong sessionAudio
	if len(pcmuFiles) > 0 {
		idx := int(sessionCounter.Add(1)-1) % len(pcmuFiles)
		sessionAudio = pcmuFiles[idx]
	}

	for {
		var chunk audioChunkWire
		if err := stream.RecvMsg(&chunk); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		if chunkCount == 0 {
			sessionID = chunk.SessionID
			streamID = chunk.StreamID
			language = chunk.Language
			if language == "" {
				language = "vi"
			}
			startRTPTs = chunk.RTPTimestamp
			zap.L().Info("stream opened",
				zap.String("session_id", sessionID),
				zap.String("stream_id", streamID),
				zap.String("language", language),
				zap.String("task", chunk.Task),
			)
		}
		chunkCount++

		if chunkCount%3 == 0 {
			seq++
			partial := &recognitionResultWire{
				SessionID:  sessionID,
				StreamID:   streamID,
				Text:       partialPhrases[(chunkCount/3-1)%uint64(len(partialPhrases))],
				IsFinal:    false,
				TsStart:    int64(startRTPTs),
				TsEnd:      int64(chunk.RTPTimestamp),
				Confidence: 0,
				Language:   language,
				Seq:        seq,
			}
			if err := stream.SendMsg(partial); err != nil {
				return err
			}
			zap.L().Debug("sent partial", zap.String("session_id", sessionID), zap.Uint64("seq", seq), zap.String("text", partial.Text))
		}

		if chunkCount%6 == 0 {
			seq++
			final := &recognitionResultWire{
				SessionID:  sessionID,
				StreamID:   streamID,
				Text:       finalPhrases[(chunkCount/6-1)%uint64(len(finalPhrases))],
				IsFinal:    true,
				TsStart:    int64(startRTPTs),
				TsEnd:      int64(chunk.RTPTimestamp),
				Confidence: 0.92,
				Language:   language,
				Seq:        seq,
			}
			final.AudioPayload = nextAudioChunk(sessionAudio, &audioOffset, audioPackets)
			if err := stream.SendMsg(final); err != nil {
				return err
			}
			zap.L().Info("sent final",
				zap.String("session_id", sessionID),
				zap.Uint64("seq", seq),
				zap.String("text", final.Text),
				zap.Uint64("chunks", chunkCount),
				zap.Int("audio_bytes", len(final.AudioPayload)),
			)
			startRTPTs = chunk.RTPTimestamp
		}

		if chunk.EndOfStream {
			seq++
			final := &recognitionResultWire{
				SessionID:  sessionID,
				StreamID:   streamID,
				Text:       fmt.Sprintf("[end] %s", finalPhrases[seq%uint64(len(finalPhrases))]),
				IsFinal:    true,
				TsStart:    int64(startRTPTs),
				TsEnd:      int64(chunk.RTPTimestamp),
				Confidence: 0.95,
				Language:   language,
				Seq:        seq,
			}
			final.AudioPayload = nextAudioChunk(sessionAudio, &audioOffset, audioPackets)
			if err := stream.SendMsg(final); err != nil {
				return err
			}
			zap.L().Info("stream closed (end_of_stream)",
				zap.String("session_id", sessionID),
				zap.Uint64("total_chunks", chunkCount),
				zap.Uint64("total_seq", seq),
			)
			return nil
		}
	}

	zap.L().Info("stream closed (client EOF)",
		zap.String("session_id", sessionID),
		zap.Uint64("total_chunks", chunkCount),
		zap.Uint64("total_seq", seq),
	)
	return nil
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	addr := flag.String("addr", envOr("MOCK_AI_ADDR", ":50051"), "gRPC listen address")
	logLevel := flag.String("log-level", envOr("MOCK_AI_LOG", "info"), "debug|info|warn|error")
	pcmDir := flag.String("pcm-dir", envOr("MOCK_AI_PCM_DIR", ""), "thư mục chứa file *.pcmu hoặc *.s16le để gửi làm audio_payload (round-robin per session)")
	flag.IntVar(&audioPackets, "audio-packets", 1, "số 20ms PCMU packet đính kèm mỗi final result khi không có --pcm-dir (0 = không gửi audio)")
	videoEndAfter := flag.Int("video-end-after", 0, "test: sau N kết quả video, worker kết thúc stream với trạng thái OK (như worker tự đóng stream giữa cuộc gọi); 0 = tắt")
	flag.Parse()
	if *videoEndAfter < 0 {
		fmt.Fprintln(os.Stderr, "--video-end-after must be >= 0")
		os.Exit(2)
	}

	zapLevel := zapcore.InfoLevel
	switch *logLevel {
	case "debug":
		zapLevel = zapcore.DebugLevel
	case "warn":
		zapLevel = zapcore.WarnLevel
	case "error":
		zapLevel = zapcore.ErrorLevel
	}
	zapCfg := zap.NewProductionConfig()
	zapCfg.Level = zap.NewAtomicLevelAt(zapLevel)
	zapCfg.EncoderConfig.EncodeTime = zapcore.TimeEncoderOfLayout("2006-01-02T15:04:05.000000Z07:00")
	logger, err := zapCfg.Build()
	if err != nil {
		panic(err)
	}
	defer logger.Sync() //nolint:errcheck
	zap.ReplaceGlobals(logger)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		zap.L().Error("listen failed", zap.String("addr", *addr), zap.Error(err))
		os.Exit(1)
	}

	srv := grpc.NewServer(
		grpc.MaxRecvMsgSize(32<<20),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             20 * time.Second, // gateway mặc định ping mỗi 30s — cho phép xuống 20s
			PermitWithoutStream: true,             // cho phép ping khi không có stream active
		}),
	)
	srv.RegisterService(&grpc.ServiceDesc{
		ServiceName: "speech.SpeechStream",
		HandlerType: (*any)(nil),
		Methods:     []grpc.MethodDesc{},
		Streams: []grpc.StreamDesc{
			{
				StreamName:    "Recognize",
				Handler:       recognizeHandler,
				ServerStreams: true,
				ClientStreams: true,
			},
		},
	}, struct{}{})
	mockai.RegisterVideoWithOptions(srv, mockai.VideoOptions{EndStreamAfter: *videoEndAfter})
	if *videoEndAfter > 0 {
		zap.L().Warn("mock video streams end normally after N results (test mode)", zap.Int("video_end_after", *videoEndAfter))
	}

	if *pcmDir != "" {
		var err error
		pcmuFiles, err = loadPCMDir(*pcmDir)
		if err != nil {
			zap.L().Error("pcm-dir: load failed", zap.String("dir", *pcmDir), zap.Error(err))
			os.Exit(1)
		}
		if len(pcmuFiles) == 0 {
			zap.L().Warn("pcm-dir: no .pcmu or .s16le files found", zap.String("dir", *pcmDir))
		}
	}

	zap.L().Info("mock-ai-worker listening",
		zap.String("addr", *addr),
		zap.Int("audio_packets_per_result", audioPackets),
		zap.Int("pcm_files_loaded", len(pcmuFiles)),
	)
	if err := srv.Serve(ln); err != nil {
		zap.L().Error("serve error", zap.Error(err))
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
