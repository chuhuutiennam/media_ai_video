// Package ai quản lý gRPC stream tới AI service theo từng session.
package ai

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"media-ai-gateway/internal/pipeline"
)

// StreamClient abstracts một bidirectional gRPC stream với AI service.
type StreamClient interface {
	Send(chunk pipeline.AudioChunk) error
	Recv() (pipeline.RecognitionResult, error)
	CloseSend() error
}

// Dialer mở một bidirectional stream mới tới AI service.
type Dialer interface {
	Dial(ctx context.Context, sessionID, streamID, language, task string) (StreamClient, error)
}

// Stream quản lý goroutine send và recv cho một AI gRPC stream.
type Stream struct {
	SessionID string
	StreamID  string
	OpenedAt  time.Time

	cancel context.CancelFunc
	wg     sync.WaitGroup
	cfg    Config

	mu      sync.Mutex
	err     error
	retries int

	sendErrors      atomic.Uint64
	recvErrors      atomic.Uint64
	endOfStreamSent atomic.Bool

	// audioDropped: AudioChunk bị drop khi internal send queue (cap QueueSize) đầy.
	audioDropped atomic.Uint64
	// partialDropped: partial result (IsFinal=false) bị drop khi resultOut đầy.
	partialDropped atomic.Uint64

	// First-result latency: ms từ lúc stream mở đến khi nhận result đầu tiên.
	firstResultMs atomic.Int64 // 0 = chưa nhận result nào
}

// StreamStats là snapshot counter của một Stream.
type StreamStats struct {
	SendErrors     uint64
	RecvErrors     uint64
	Retries        int
	AudioDropped   uint64 // AudioChunk bị drop do internal send queue đầy
	PartialDropped uint64 // partial result bị drop do resultOut đầy

	// FirstResultMs: ms từ stream open đến result đầu tiên; 0 nếu chưa có result.
	FirstResultMs int64
}

// Stats trả về snapshot thống kê của stream tại thời điểm gọi.
func (s *Stream) Stats() StreamStats {
	s.mu.Lock()
	retries := s.retries
	s.mu.Unlock()
	return StreamStats{
		SendErrors:     s.sendErrors.Load(),
		RecvErrors:     s.recvErrors.Load(),
		Retries:        retries,
		AudioDropped:   s.audioDropped.Load(),
		PartialDropped: s.partialDropped.Load(),
		FirstResultMs:  s.firstResultMs.Load(),
	}
}

// Err trả về lỗi đầu tiên không phải io.EOF từ send hoặc recv.
func (s *Stream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *Stream) setErr(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}

func (s *Stream) clearErr() {
	s.mu.Lock()
	s.err = nil
	s.mu.Unlock()
}

// Retries trả về số lần reconnect đã thực hiện.
func (s *Stream) Retries() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retries
}

// Wait block cho đến khi wrapper goroutine đã thoát.
func (s *Stream) Wait() { s.wg.Wait() }

// runWithReconnect là wrapper goroutine duy nhất. Quản lý reconnect theo cfg.MaxRetries.
// Initial client đã được dial đồng bộ trong Open() — truyền thẳng vào đây.
func (s *Stream) runWithReconnect(
	ctx context.Context,
	dialer Dialer,
	client StreamClient,
	sessionID, streamID, language, task string,
	audioIn <-chan pipeline.AudioChunk,
	resultOut chan<- pipeline.RecognitionResult,
) {
	defer s.wg.Done()

	backoff := s.cfg.RetryBackoff
	if backoff == 0 {
		backoff = time.Second
	}

	for {
		s.runPair(ctx, client, audioIn, resultOut)

		// Nếu ctx bị cancel từ ngoài, thoát ngay — không reconnect.
		if ctx.Err() != nil {
			return
		}

		pairErr := s.Err()
		if pairErr == nil || s.cfg.MaxRetries == 0 {
			return
		}
		if isPermanentGRPCError(pairErr) {
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

		zap.L().Debug("ai: reconnecting", zap.String("session_id", sessionID), zap.Int("attempt", attempt), zap.Duration("backoff", backoff), zap.Error(pairErr))

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

		newClient, err := dialer.Dial(ctx, sessionID, streamID, language, task)
		if err != nil {
			s.setErr(err)
			return
		}
		client = newClient
	}
}

// runPair chạy một cặp send+recv cho một connection attempt.
// Thoát khi send hoặc recv exit; gọi client.CloseSend() khi kết thúc.
func (s *Stream) runPair(
	ctx context.Context,
	client StreamClient,
	audioIn <-chan pipeline.AudioChunk,
	resultOut chan<- pipeline.RecognitionResult,
) {
	s.endOfStreamSent.Store(false)

	pairCtx, pairCancel := context.WithCancel(ctx)
	defer pairCancel()

	var wg sync.WaitGroup

	var sendInput <-chan pipeline.AudioChunk
	if s.cfg.QueueSize > 0 {
		q := make(chan pipeline.AudioChunk, s.cfg.QueueSize)
		sendInput = q
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(q)
			for {
				select {
				case <-pairCtx.Done():
					return
				case chunk, ok := <-audioIn:
					if !ok {
						return
					}
					select {
					case q <- chunk:
					default:
						s.audioDropped.Add(1)
					}
				}
			}
		}()
	} else {
		sendInput = audioIn
	}

	wg.Add(2)
	go func() {
		defer wg.Done()
		defer pairCancel()
		s.runSend(pairCtx, client, sendInput)
		// CloseSend signals the recv side to unblock when send exits first.
		client.CloseSend()
	}()
	go func() {
		defer wg.Done()
		defer pairCancel()
		s.runRecv(pairCtx, client, resultOut)
	}()

	wg.Wait()
	// Đóng kết nối sau khi cả send lẫn recv đã exit — tránh cắt ngang final results.
	// grpcStreamClient implement io.Closer; NullDialer và mock không cần Close.
	if closer, ok := client.(io.Closer); ok {
		_ = closer.Close()
	}
}

// sendWithTimeout wraps client.Send với timeout nếu cfg.SendTimeout > 0.
// Trả về context.DeadlineExceeded nếu hết timeout.
func (s *Stream) sendWithTimeout(ctx context.Context, client StreamClient, chunk pipeline.AudioChunk) error {
	type result struct{ err error }
	ch := make(chan result, 1)
	go func() {
		ch <- result{client.Send(chunk)}
	}()
	if s.cfg.SendTimeout > 0 {
		timer := time.NewTimer(s.cfg.SendTimeout)
		defer timer.Stop()
		select {
		case r := <-ch:
			return r.err
		case <-timer.C:
			return context.DeadlineExceeded
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	select {
	case r := <-ch:
		return r.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Stream) runSend(ctx context.Context, client StreamClient, audioIn <-chan pipeline.AudioChunk) {
	var firstChunkTimer <-chan time.Time
	if s.cfg.FirstChunkTimeout > 0 {
		t := time.NewTimer(s.cfg.FirstChunkTimeout)
		defer t.Stop()
		firstChunkTimer = t.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-firstChunkTimer:
			s.setErr(ErrFirstChunkTimeout)
			zap.L().Debug("ai: first chunk timeout", zap.String("session_id", s.SessionID), zap.String("stream_id", s.StreamID), zap.Duration("timeout", s.cfg.FirstChunkTimeout))
			return
		case chunk, ok := <-audioIn:
			if !ok {
				return
			}
			firstChunkTimer = nil // disable after first chunk received
			if err := s.sendWithTimeout(ctx, client, chunk); err != nil {
				if !isCtxErr(err) {
					s.sendErrors.Add(1)
					s.setErr(err)
					zap.L().Debug("ai: send error to worker", zap.String("session_id", s.SessionID), zap.String("stream_id", s.StreamID), zap.Error(err))
				}
				return
			}
			zap.L().Debug("ai: chunk sent",
				zap.String("session_id",  s.SessionID),
				zap.String("stream_id",   s.StreamID),
				zap.Uint32("rtp_ts",      chunk.RTPTimestamp),
				zap.Int64("duration_ms",  chunk.DurationMs),
				zap.Int("pcm_bytes",      len(chunk.PCM)),
				zap.Bool("end_of_stream", chunk.EndOfStream),
			)
			if chunk.EndOfStream {
				s.endOfStreamSent.Store(true)
			}
		}
	}
}

// recvWithTimeout wraps client.Recv với idle timeout nếu cfg.RecvIdleTimeout > 0.
// Trả về ErrRecvIdleTimeout nếu không có result nào trong khoảng RecvIdleTimeout.
func (s *Stream) recvWithTimeout(ctx context.Context, client StreamClient) (pipeline.RecognitionResult, error) {
	if s.cfg.RecvIdleTimeout == 0 {
		return client.Recv()
	}
	type result struct {
		r   pipeline.RecognitionResult
		err error
	}
	ch := make(chan result, 1)
	go func() {
		r, err := client.Recv()
		ch <- result{r, err}
	}()
	timer := time.NewTimer(s.cfg.RecvIdleTimeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		return res.r, res.err
	case <-timer.C:
		return pipeline.RecognitionResult{}, ErrRecvIdleTimeout
	case <-ctx.Done():
		return pipeline.RecognitionResult{}, ctx.Err()
	}
}

// runRecv gọi client.Recv() trong vòng lặp và dispatch kết quả sang resultOut.
//
// Backpressure policy:
//   - Partial result (IsFinal=false) bị drop nếu resultOut đầy.
//   - Final result (IsFinal=true) block cho đến khi được deliver (hoặc ctx done).
func (s *Stream) runRecv(ctx context.Context, client StreamClient, resultOut chan<- pipeline.RecognitionResult) {
	for {
		r, err := s.recvWithTimeout(ctx, client)
		if err != nil {
			if ctx.Err() == nil {
				if err != io.EOF {
					s.recvErrors.Add(1)
					s.setErr(err)
					zap.L().Debug("ai: recv error from worker", zap.String("session_id", s.SessionID), zap.String("stream_id", s.StreamID), zap.Error(err))
				} else if !s.endOfStreamSent.Load() {
					s.recvErrors.Add(1)
					s.setErr(ErrUnexpectedEOF)
					zap.L().Debug("ai: unexpected EOF from worker", zap.String("session_id", s.SessionID), zap.String("stream_id", s.StreamID))
				}
			}
			return
		}

		now := time.Now().UnixMilli()

		// First-result latency (stream open → first result).
		// Clamp to ≥1ms so 0 stays unambiguous as "no result yet".
		latency := now - s.OpenedAt.UnixMilli()
		if latency < 1 {
			latency = 1
		}
		if s.firstResultMs.CompareAndSwap(0, latency) {
			zap.L().Debug("ai: first result received",
				zap.String("session_id", s.SessionID),
				zap.Int64("first_result_ms", latency),
			)
		}

		select {
		case resultOut <- r:
			continue
		case <-ctx.Done():
			return
		default:
		}
		// Partial text-only result: drop khi queue đầy.
		// Partial với AudioPayload: không drop — mất packet = gap âm thanh.
		if !r.IsFinal && len(r.AudioPayload) == 0 {
			s.partialDropped.Add(1)
			continue
		}
		select {
		case resultOut <- r:
		case <-ctx.Done():
			return
		}
	}
}

func isCtxErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// isPermanentGRPCError trả về true cho các gRPC status code không nên retry.
func isPermanentGRPCError(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	switch st.Code() {
	case codes.InvalidArgument, codes.NotFound,
		codes.PermissionDenied, codes.Unauthenticated, codes.Unimplemented:
		return true
	}
	return false
}
