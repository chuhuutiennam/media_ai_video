package ai

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"media-ai-gateway/internal/pipeline"
)

// VideoStreamClient abstracts một bidirectional gRPC stream video với AI service.
type VideoStreamClient interface {
	Send(frame pipeline.VideoFrame) error
	Recv() (pipeline.RecognitionResult, error)
	CloseSend() error
}

// VideoDialer mở một bidirectional video stream mới tới AI service.
type VideoDialer interface {
	Dial(ctx context.Context, sessionID, streamID string, cfg VideoStreamConfig) (VideoStreamClient, error)
}

// VideoStreamConfig mang theo tham số cấu hình gửi 1 lần lúc mở stream
// (tương ứng VideoConfig trong VideoRequest oneof).
type VideoStreamConfig struct {
	EffectType                        string
	InferWidth, InferHeight, InferFPS int
	Threshold                         float32
}

// VideoStream quản lý goroutine send/recv cho một AI video gRPC stream.
// Cấu trúc và reconnect/backoff policy giống hệt Stream (audio) — xem
// grpc_client.go — chỉ khác kiểu dữ liệu gửi đi (VideoFrame thay vì AudioChunk).
type VideoStream struct {
	SessionID string
	StreamID  string
	OpenedAt  time.Time

	cancel context.CancelFunc
	wg     sync.WaitGroup
	cfg    Config

	mu      sync.Mutex
	err     error
	retries int

	sendErrors atomic.Uint64
	recvErrors atomic.Uint64

	firstResultMs atomic.Int64
	totals        *videoCounters
}

func (s *VideoStream) Stats() StreamStats {
	s.mu.Lock()
	retries := s.retries
	s.mu.Unlock()
	return StreamStats{
		SendErrors:    s.sendErrors.Load(),
		RecvErrors:    s.recvErrors.Load(),
		Retries:       retries,
		FirstResultMs: s.firstResultMs.Load(),
	}
}

func (s *VideoStream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *VideoStream) setErr(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}

func (s *VideoStream) clearErr() {
	s.mu.Lock()
	s.err = nil
	s.mu.Unlock()
}

// Wait block cho đến khi wrapper goroutine đã thoát.
func (s *VideoStream) Wait() { s.wg.Wait() }

func (s *VideoStream) runWithReconnect(
	ctx context.Context,
	dialer VideoDialer,
	client VideoStreamClient,
	sessionID, streamID string,
	streamCfg VideoStreamConfig,
	frameIn <-chan pipeline.VideoFrame,
	resultOut chan<- pipeline.RecognitionResult,
) {
	defer s.wg.Done()

	backoff := s.cfg.RetryBackoff
	if backoff == 0 {
		backoff = time.Second
	}

	for {
		s.runPair(ctx, client, frameIn, resultOut)

		if ctx.Err() != nil {
			return
		}

		pairErr := s.Err()
		if pairErr == nil || s.cfg.MaxRetries == 0 {
			return
		}

		s.mu.Lock()
		if s.retries >= s.cfg.MaxRetries {
			s.mu.Unlock()
			return
		}
		s.retries++
		attempt := s.retries
		s.mu.Unlock()

		zap.L().Debug("ai: video reconnecting", zap.String("session_id", sessionID), zap.Int("attempt", attempt), zap.Duration("backoff", backoff), zap.Error(pairErr))

		s.clearErr()

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}

		newClient, err := dialer.Dial(ctx, sessionID, streamID, streamCfg)
		if err != nil {
			s.setErr(err)
			return
		}
		client = newClient
	}
}

func (s *VideoStream) runPair(
	ctx context.Context,
	client VideoStreamClient,
	frameIn <-chan pipeline.VideoFrame,
	resultOut chan<- pipeline.RecognitionResult,
) {
	pairCtx, pairCancel := context.WithCancel(ctx)
	defer pairCancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer pairCancel()
		s.runSend(pairCtx, client, frameIn)
		client.CloseSend()
	}()
	go func() {
		defer wg.Done()
		defer pairCancel()
		s.runRecv(pairCtx, client, resultOut)
	}()
	wg.Wait()

	if closer, ok := client.(io.Closer); ok {
		_ = closer.Close()
	}
}

func (s *VideoStream) runSend(ctx context.Context, client VideoStreamClient, frameIn <-chan pipeline.VideoFrame) {
	var firstTimer <-chan time.Time
	if s.cfg.FirstChunkTimeout > 0 {
		t := time.NewTimer(s.cfg.FirstChunkTimeout)
		defer t.Stop()
		firstTimer = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-firstTimer:
			s.setErr(ErrFirstChunkTimeout)
			return
		case frame, ok := <-frameIn:
			if !ok {
				return
			}
			firstTimer = nil
			if err := client.Send(frame); err != nil {
				if !isCtxErr(err) {
					s.sendErrors.Add(1)
					if s.totals != nil {
						s.totals.sendErrors.Add(1)
					}
					s.setErr(err)
					zap.L().Debug("ai: video send error", zap.String("session_id", s.SessionID), zap.Error(err))
				}
				return
			}
			if s.totals != nil {
				s.totals.framesSent.Add(1)
			}
		}
	}
}

func (s *VideoStream) runRecv(ctx context.Context, client VideoStreamClient, resultOut chan<- pipeline.RecognitionResult) {
	for {
		var recvErr error
		var r pipeline.RecognitionResult
		if s.cfg.RecvIdleTimeout > 0 {
			type res struct {
				r   pipeline.RecognitionResult
				err error
			}
			ch := make(chan res, 1)
			go func() {
				rr, err := client.Recv()
				ch <- res{rr, err}
			}()
			timer := time.NewTimer(s.cfg.RecvIdleTimeout)
			select {
			case out := <-ch:
				timer.Stop()
				r, recvErr = out.r, out.err
			case <-timer.C:
				recvErr = ErrRecvIdleTimeout
			case <-ctx.Done():
				timer.Stop()
				return
			}
		} else {
			r, recvErr = client.Recv()
		}

		if recvErr != nil {
			if ctx.Err() == nil && recvErr != io.EOF {
				s.recvErrors.Add(1)
				if s.totals != nil {
					s.totals.recvErrors.Add(1)
				}
				s.setErr(recvErr)
				zap.L().Debug("ai: video recv error", zap.String("session_id", s.SessionID), zap.Error(recvErr))
			}
			return
		}

		now := time.Now().UnixMilli()
		s.firstResultMs.CompareAndSwap(0, now-s.OpenedAt.UnixMilli())
		if s.totals != nil {
			s.totals.resultsReceived.Add(1)
			if r.VideoMask == nil || r.VideoMask.Status != "ok" {
				s.totals.resultErrors.Add(1)
			}
		}

		select {
		case resultOut <- r:
		case <-ctx.Done():
			return
		}
	}
}

// VideoManager quản lý bidirectional gRPC video stream tới AI service, một
// stream mỗi session có VideoEnabled=true. API/behavior đối xứng với
// ai.Manager (audio) — xem stream_manager.go.
type VideoManager struct {
	cfg     Config
	dialer  VideoDialer
	mu      sync.RWMutex
	streams map[string]*VideoStream
	totals  videoCounters
}

// VideoManagerStats contains cumulative counters, including closed streams.
type VideoManagerStats struct {
	FramesSent, ResultsReceived, SendErrors, RecvErrors, ResultErrors uint64
}

type videoCounters struct {
	framesSent, resultsReceived, sendErrors, recvErrors, resultErrors atomic.Uint64
}

func (m *VideoManager) Stats() VideoManagerStats {
	return VideoManagerStats{
		FramesSent: m.totals.framesSent.Load(), ResultsReceived: m.totals.resultsReceived.Load(),
		SendErrors: m.totals.sendErrors.Load(), RecvErrors: m.totals.recvErrors.Load(),
		ResultErrors: m.totals.resultErrors.Load(),
	}
}

// NewVideoManager tạo VideoManager. Có thể dùng cùng ai.Config với audio
// Manager (MaxActiveStreams nên đặt riêng — video là workload GPU nặng hơn).
func NewVideoManager(cfg Config, dialer VideoDialer) *VideoManager {
	return &VideoManager{cfg: cfg, dialer: dialer, streams: make(map[string]*VideoStream)}
}

// Open tạo và khởi động AI video stream cho session.
func (m *VideoManager) Open(
	parentCtx context.Context,
	sessionID, streamID string,
	streamCfg VideoStreamConfig,
	frameIn <-chan pipeline.VideoFrame,
	resultOut chan<- pipeline.RecognitionResult,
) (*VideoStream, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.streams) >= m.cfg.MaxActiveStreams {
		return nil, ErrMaxStreams
	}
	if _, exists := m.streams[sessionID]; exists {
		return nil, ErrDuplicateStream
	}

	ctx, cancel := context.WithTimeout(parentCtx, m.cfg.StreamTimeout)

	client, err := m.dialer.Dial(ctx, sessionID, streamID, streamCfg)
	if err != nil {
		cancel()
		return nil, err
	}

	s := &VideoStream{SessionID: sessionID, StreamID: streamID, OpenedAt: time.Now(), cancel: cancel, cfg: m.cfg, totals: &m.totals}
	s.wg.Add(1)
	go s.runWithReconnect(ctx, m.dialer, client, sessionID, streamID, streamCfg, frameIn, resultOut)

	m.streams[sessionID] = s
	zap.L().Debug("ai: video stream opened", zap.String("session_id", sessionID), zap.String("stream_id", streamID))

	go func() {
		s.wg.Wait()
		m.mu.Lock()
		if m.streams[sessionID] == s {
			delete(m.streams, sessionID)
		}
		m.mu.Unlock()
	}()

	return s, nil
}

// Close đóng video stream của session. Trả về false nếu không tồn tại.
func (m *VideoManager) Close(sessionID string) bool {
	m.mu.Lock()
	s, ok := m.streams[sessionID]
	if ok {
		delete(m.streams, sessionID)
	}
	m.mu.Unlock()
	if !ok {
		return false
	}
	s.cancel()
	return true
}

// Count trả về số video stream đang mở.
func (m *VideoManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.streams)
}
