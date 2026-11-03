package pipeline

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"go.uber.org/zap"
	"media-ai-gateway/internal/audio"
	"media-ai-gateway/internal/codec"
)

// SessionConfig mô tả audio pipeline cần tạo cho một session.
type SessionConfig struct {
	// Input từ codec
	Codec      string // "PCMU", "PCMA", "opus"
	SampleRate int    // input sample rate; 0 = dùng default của codec
	Channels   int    // input channels; 0 = dùng default của codec

	// Output về chunker
	OutSampleRate int // Hz đầu ra (ví dụ: 16000)
	OutChannels   int // kênh đầu ra (thường 1)
	ChunkMs       int // độ dài mỗi AudioChunk (ms)

	// Metadata
	SessionID string
	StreamID  string

	// Debug: nếu non-empty, ghi raw decoded PCM (int16-LE) vào thư mục này.
	// Tên file: <session_id>.<codec>.<rate>hz.<ch>ch.s16le
	// Phát lại: ffplay -f s16le -ar <rate> -ac <ch> <file>
	// Empty = tắt.
	PCMDumpDir string
}

// sessionPipeline giữ state xử lý âm thanh theo từng session.
// Mỗi sessionPipeline có một goroutine riêng (run) là consumer duy nhất của jobCh,
// đảm bảo thứ tự xử lý packet trong một session không bị đảo lộn.
type sessionPipeline struct {
	jobCh  chan MediaPacket
	done   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc

	// stats được chia sẻ với WorkerPool (pointer tới atomic của WorkerPool).
	processed    *atomic.Uint64
	decodeErrors *atomic.Uint64

	decoder   codec.Decoder
	resampler *audio.Resampler
	chunker   *audio.Chunker
	audioOut  chan<- AudioChunk
	pcmFile   *os.File
	pcmBuf    *bufio.Writer
	pcmPath   string
	pcmBytes  int64
}

// run là vòng lặp chính của goroutine per-session.
// Đây là consumer duy nhất của jobCh — đảm bảo ordering tuyệt đối.
// Thoát khi ctx bị cancel, sau đó gọi flush() để phát partial chunk còn lại.
func (sp *sessionPipeline) run() {
	defer close(sp.done)
	defer sp.flush()
	for {
		select {
		case pkt := <-sp.jobCh:
			_, decErr := sp.process(pkt)
			if decErr != nil {
				sp.decodeErrors.Add(1)
			} else {
				sp.processed.Add(1)
			}
		case <-sp.ctx.Done():
			return
		}
	}
}

// process decode + resample + chunk một MediaPacket.
// Chỉ được gọi từ run() — không cần mutex vì single-goroutine.
// Trả về số chunks đã emit và decode error (nếu có).
func (sp *sessionPipeline) process(pkt MediaPacket) (int, error) {
	pcm, err := sp.decoder.Decode(pkt.Payload)
	if err != nil {
		return 0, fmt.Errorf("pipeline: decode: %w", err)
	}
	if len(pcm) == 0 {
		return 0, nil
	}

	if sp.pcmBuf != nil {
		if err := binary.Write(sp.pcmBuf, binary.LittleEndian, pcm); err != nil {
			zap.L().Warn("pcm dump write error", zap.String("path", sp.pcmPath), zap.Error(err))
			sp.pcmBuf = nil
		} else {
			sp.pcmBytes += int64(len(pcm) * 2)
		}
	}

	resampled := sp.resampler.Resample(pcm)
	chunks := sp.chunker.Push(resampled, pkt.Timestamp)

	emitted := 0
	for _, ac := range chunks {
		chunk := AudioChunk{
			SessionID:    ac.SessionID,
			StreamID:     ac.StreamID,
			PCM:          ac.PCM,
			SampleRate:   ac.SampleRate,
			Channels:     ac.Channels,
			RTPTimestamp: ac.RTPTimestamp,
			RTPClockRate: ac.RTPClockRate,
			DurationMs:   ac.DurationMs,
			ChunkSeq:     ac.ChunkSeq,
		}
		select {
		case sp.audioOut <- chunk:
			emitted++
		case <-sp.ctx.Done():
			return emitted, nil
		}
	}
	return emitted, nil
}

// flush phát phần PCM còn lại trong Chunker buffer như partial chunk.
// Chỉ được gọi từ run() sau khi vòng lặp chính thoát — không cần mutex.
func (sp *sessionPipeline) flush() {
	if sp.pcmFile != nil {
		if sp.pcmBuf != nil {
			if err := sp.pcmBuf.Flush(); err != nil {
				zap.L().Warn("pcm dump flush error", zap.String("path", sp.pcmPath), zap.Error(err))
			}
			sp.pcmBuf = nil
		}
		_ = sp.pcmFile.Sync()
		sp.pcmFile.Close()
		sp.pcmFile = nil
		zap.L().Debug("pcm dump closed", zap.String("path", sp.pcmPath), zap.Int64("bytes", sp.pcmBytes))
	}

	ac := sp.chunker.Flush()
	if ac == nil {
		return
	}
	select {
	case sp.audioOut <- AudioChunk{
		SessionID:    ac.SessionID,
		StreamID:     ac.StreamID,
		PCM:          ac.PCM,
		SampleRate:   ac.SampleRate,
		Channels:     ac.Channels,
		RTPTimestamp: ac.RTPTimestamp,
		RTPClockRate: ac.RTPClockRate,
		DurationMs:   ac.DurationMs,
		ChunkSeq:     ac.ChunkSeq,
		EndOfStream:  true,
	}:
	default:
	}
}

// newSessionPipeline tạo sessionPipeline từ SessionConfig.
// queueSize là dung lượng của jobCh (per-session packet queue).
// processed và decodeErrors là pointers tới atomic counters của WorkerPool.
func newSessionPipeline(
	sessCtx context.Context,
	cfg SessionConfig,
	audioOut chan<- AudioChunk,
	queueSize int,
	processed *atomic.Uint64,
	decodeErrors *atomic.Uint64,
) (*sessionPipeline, error) {
	ctx, cancel := context.WithCancel(sessCtx)

	dec, err := codec.New(cfg.Codec, cfg.SampleRate, cfg.Channels)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("pipeline: codec: %w", err)
	}
	resampler := audio.NewResampler(dec.SampleRate(), dec.Channels(), cfg.OutSampleRate)
	chunker := audio.NewChunker(
		audio.ChunkerConfig{
			SampleRate:   cfg.OutSampleRate,
			Channels:     cfg.OutChannels,
			ChunkMs:      cfg.ChunkMs,
			RTPClockRate: dec.SampleRate(),
		},
		cfg.SessionID, cfg.StreamID,
	)

	if queueSize <= 0 {
		queueSize = 32
	}

	var pcmFile *os.File
	var pcmBuf *bufio.Writer
	var pcmPath string
	if cfg.PCMDumpDir != "" {
		if mkErr := os.MkdirAll(cfg.PCMDumpDir, 0o755); mkErr != nil {
			cancel()
			return nil, fmt.Errorf("pipeline: pcm dump dir: %w", mkErr)
		}
		pcmPath = filepath.Join(cfg.PCMDumpDir, fmt.Sprintf("%s.%s.%dhz.%dch.s16le",
			cfg.SessionID,
			strings.ToLower(strings.ReplaceAll(cfg.Codec, "-", "")),
			dec.SampleRate(),
			dec.Channels(),
		))
		pcmFile, err = os.OpenFile(pcmPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("pipeline: pcm dump open: %w", err)
		}
		pcmBuf = bufio.NewWriterSize(pcmFile, 32*1024)
		zap.L().Info("pcm dump opened", zap.String("path", pcmPath), zap.String("session_id", cfg.SessionID))
	}

	return &sessionPipeline{
		jobCh:        make(chan MediaPacket, queueSize),
		done:         make(chan struct{}),
		ctx:          ctx,
		cancel:       cancel,
		processed:    processed,
		decodeErrors: decodeErrors,
		decoder:      dec,
		resampler:    resampler,
		chunker:      chunker,
		audioOut:     audioOut,
		pcmFile:      pcmFile,
		pcmBuf:       pcmBuf,
		pcmPath:      pcmPath,
	}, nil
}
