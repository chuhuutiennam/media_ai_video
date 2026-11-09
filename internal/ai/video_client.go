package ai

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"media-ai-gateway/internal/pipeline"
)

var ErrVideoSendTimeout = errors.New("ai: video send timeout")

// ErrVideoStreamEnded: worker kết thúc stream (OK/EOF) khi session còn sống và gateway chưa gửi EOS.
var ErrVideoStreamEnded = errors.New("ai: worker ended the video stream before the session")

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
	// Local output routing for mixed source RPCs; never serialized to the worker.
	GestureResultOut                  chan<- pipeline.RecognitionResult
	Source                            *SourceStreamConfig
	AIService                         string
	SelectedService                   string
	MediaEpoch                        string
	MaxHands                          int
	EffectType                        string
	InferWidth, InferHeight, InferFPS int
	Threshold                         float32
}

// VideoStream quản lý goroutine send/recv cho một AI video gRPC stream.
// Cấu trúc giống Stream (audio, grpc_client.go); khác ở kết nối lại: mở lại lỗi vẫn thử tiếp,
// max_retry tính số lần lỗi liên tiếp, và stream dừng hẳn thì log + đếm (runWithReconnect).
type VideoStream struct {
	SessionID string
	StreamID  string
	OpenedAt  time.Time

	cancel context.CancelFunc
	wg     sync.WaitGroup
	cfg    Config

	mu      sync.Mutex
	err     error
	retries int // số lần mở lại liên tiếp chưa có kết quả; về 0 khi stream mới nhận kết quả
	// reopened: stream vừa được mở lại, chờ kết quả đầu tiên để đặt lại retries.
	reopened atomic.Bool
	// eosSent: gateway đã kết thúc chiều gửi của stream hiện tại (EOS / đóng frame input).
	eosSent atomic.Bool

	sendErrors atomic.Uint64
	recvErrors atomic.Uint64

	firstResultMs atomic.Int64
	totals        *videoCounters

	sourceMode       bool
	aiService        string
	gestureResultOut chan<- pipeline.RecognitionResult
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

// Lý do stream AI video dừng hẳn giữa session (log + media_ai_video_ai_streams_stopped_total).
const (
	videoStopStreamTimeout    = "stream_timeout"    // hết video.ai.stream_timeout_sec
	videoStopPermanentError   = "permanent_error"   // mã gRPC không thử lại (vd INVALID_ARGUMENT)
	videoStopRetriesExhausted = "retries_exhausted" // hết video.ai.max_retry lần mở lại liên tiếp
)

const videoMaxRetryBackoff = 30 * time.Second

// videoRetryBackoff: base × 2^(attempt−1), tối đa 30 s; base 0 = 1 s.
func videoRetryBackoff(base time.Duration, attempt int) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	d := base
	for i := 1; i < attempt && d < videoMaxRetryBackoff; i++ {
		d *= 2
	}
	return min(d, videoMaxRetryBackoff)
}

// stopped ghi nhận stream dừng hẳn vì lỗi hoặc hết hạn (không gọi khi session đóng hay EOS).
func (s *VideoStream) stopped(reason string, err error) {
	s.mu.Lock()
	retries := s.retries
	s.mu.Unlock()
	if s.totals != nil {
		s.totals.streamsStopped.Add(1)
	}
	zap.L().Warn("ai: video stream stopped", zap.String("session_id", s.SessionID), zap.String("stream_id", s.StreamID),
		zap.String("ai_service", s.aiService), zap.String("reason", reason), zap.Int("retries", retries), zap.Error(err))
}

// runWithReconnect chạy stream và mở lại khi lỗi. Mở lại lỗi (vd worker chưa chạy lại) cũng tính
// một lượt và thử tiếp sau backoff. retries về 0 khi stream mới nhận được kết quả (runRecv), nên
// MaxRetries giới hạn số lần lỗi liên tiếp, không phải cả đời stream.
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

	for {
		if client != nil {
			s.runPair(ctx, client, frameIn, resultOut)
			client = nil
		}

		if err := ctx.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				s.stopped(videoStopStreamTimeout, err)
			}
			return // session đóng
		}

		pairErr := s.Err()
		if pairErr == nil {
			return // frame input đóng hoặc EOS
		}
		if isPermanentGRPCError(pairErr) {
			s.stopped(videoStopPermanentError, pairErr)
			return
		}

		s.mu.Lock()
		if s.retries >= s.cfg.MaxRetries {
			s.mu.Unlock()
			s.stopped(videoStopRetriesExhausted, pairErr)
			return
		}
		s.retries++
		attempt := s.retries
		s.mu.Unlock()

		backoff := videoRetryBackoff(s.cfg.RetryBackoff, attempt)
		zap.L().Debug("ai: video reconnecting", zap.String("session_id", sessionID), zap.Int("attempt", attempt), zap.Duration("backoff", backoff), zap.Error(pairErr))

		s.clearErr()

		select {
		case <-ctx.Done():
			continue // đầu vòng lặp phân biệt hết hạn và session đóng
		case <-time.After(backoff):
		}

		newClient, err := dialer.Dial(ctx, sessionID, streamID, streamCfg)
		if err != nil {
			s.setErr(err)
			if ctx.Err() == nil {
				zap.L().Warn("ai: video stream reopen failed", zap.String("session_id", sessionID), zap.String("stream_id", streamID),
					zap.String("ai_service", s.aiService), zap.Int("attempt", attempt), zap.Int("max_retry", s.cfg.MaxRetries), zap.Error(err))
			}
			continue
		}
		s.reopened.Store(true)
		zap.L().Info("ai: video stream reopened", zap.String("session_id", sessionID), zap.String("stream_id", streamID),
			zap.String("ai_service", s.aiService), zap.Int("attempt", attempt))
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
	s.eosSent.Store(false) // mỗi stream mở lại bắt đầu chưa gửi EOS

	// Cancel the actual RPC as well: a blocked Recv must not retain task slots.
	if closer, ok := client.(io.Closer); ok {
		go func() { <-pairCtx.Done(); _ = closer.Close() }()
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		graceful := s.runSend(pairCtx, client, frameIn)
		if !graceful {
			pairCancel()
		}
		_ = client.CloseSend()
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

func (s *VideoStream) runSend(ctx context.Context, client VideoStreamClient, frameIn <-chan pipeline.VideoFrame) bool {
	var firstTimer <-chan time.Time
	if s.cfg.FirstChunkTimeout > 0 {
		t := time.NewTimer(s.cfg.FirstChunkTimeout)
		defer t.Stop()
		firstTimer = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return false
		case <-firstTimer:
			s.setErr(ErrFirstChunkTimeout)
			return false
		case frame, ok := <-frameIn:
			if !ok {
				s.eosSent.Store(true) // trước CloseSend: EOF của worker sau đó là bình thường
				return true
			}
			if frame.EndOfStream {
				s.eosSent.Store(true) // trước khi gửi: worker có thể trả EOF ngay
			}
			firstTimer = nil
			var sendErr error
			if s.sourceMode && s.cfg.SendTimeout > 0 {
				var expired atomic.Bool
				timer := time.AfterFunc(s.cfg.SendTimeout, func() {
					expired.Store(true)
					if closer, ok := client.(io.Closer); ok {
						_ = closer.Close()
					}
				})
				sendErr = client.Send(frame)
				timer.Stop()
				if expired.Load() {
					sendErr = ErrVideoSendTimeout
				}
			} else {
				sendErr = client.Send(frame)
			}
			if err := sendErr; err != nil {
				if !isCtxErr(err) {
					s.sendErrors.Add(1)
					if s.totals != nil {
						s.totals.sendErrors.Add(1)
					}
					s.setErr(err)
					zap.L().Debug("ai: video send error", zap.String("session_id", s.SessionID), zap.Error(err))
				}
				return false
			}
			if s.totals != nil {
				s.totals.framesSent.Add(1)
			}
			if frame.EndOfStream {
				return true
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
			if recvErr == io.EOF && !s.eosSent.Load() {
				// Gateway video không kết thúc stream giữa phiên: worker tự đóng (trạng thái OK)
				// là bất thường → lỗi để mở lại, thay vì mất AI im lặng.
				recvErr = ErrVideoStreamEnded
			}
			if ctx.Err() == nil && recvErr != io.EOF {
				s.recvErrors.Add(1)
				if s.totals != nil {
					s.totals.recvErrors.Add(1)
				}
				s.setErr(recvErr)
				zap.L().Warn("ai: video recv error", zap.String("session_id", s.SessionID), zap.String("stream_id", s.StreamID), zap.String("ai_service", s.aiService), zap.Error(recvErr))
			}
			return
		}

		if s.reopened.CompareAndSwap(true, false) {
			s.mu.Lock()
			s.retries = 0 // stream mở lại đã chạy được: lần lỗi sau có lại đủ max_retry lượt
			s.mu.Unlock()
		}
		now := time.Now().UnixMilli()
		s.firstResultMs.CompareAndSwap(0, now-s.OpenedAt.UnixMilli())
		if s.totals != nil {
			s.totals.resultsReceived.Add(1)
			if r.IsVideoGesture() {
				s.totals.gestureResults.Add(1)
			}
			if (r.IsVideoGesture() && !r.GestureResult.Valid()) || (!r.IsVideoGesture() && (r.VideoMask == nil || r.VideoMask.Status != "ok")) {
				s.totals.resultErrors.Add(1)
			}
		}

		target := resultOut
		if r.IsVideoGesture() && s.gestureResultOut != nil {
			target = s.gestureResultOut
		}
		select {
		case target <- r:
		case <-ctx.Done():
			return
		default:
			if r.IsVideoGesture() {
				if s.totals != nil {
					s.totals.gestureDropped.Add(1)
				}
				continue
			}
			if s.sourceMode {
				// A congested mask callback must not stop reading gesture/credit
				// messages from the same source RPC. Media snapshots are bounded.
				if s.totals != nil {
					s.totals.maskDropped.Add(1)
				}
				continue
			}
			select {
			case target <- r:
			case <-ctx.Done():
				return
			}
		}
	}
}

// VideoManager manages one stream per legacy session or (session, AI task). API/behavior đối xứng với
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
	GestureResults                                                    uint64
	GestureDropped                                                    uint64
	MaskDropped                                                       uint64
	StreamsStopped                                                    uint64 // stream dừng hẳn giữa session (xem videoStop*)
}

type videoCounters struct {
	framesSent, resultsReceived, sendErrors, recvErrors, resultErrors atomic.Uint64
	gestureResults                                                    atomic.Uint64
	gestureDropped                                                    atomic.Uint64
	maskDropped                                                       atomic.Uint64
	streamsStopped                                                    atomic.Uint64
}

func (m *VideoManager) Stats() VideoManagerStats {
	return VideoManagerStats{
		FramesSent: m.totals.framesSent.Load(), ResultsReceived: m.totals.resultsReceived.Load(),
		SendErrors: m.totals.sendErrors.Load(), RecvErrors: m.totals.recvErrors.Load(),
		ResultErrors:   m.totals.resultErrors.Load(),
		GestureResults: m.totals.gestureResults.Load(),
		GestureDropped: m.totals.gestureDropped.Load(),
		MaskDropped:    m.totals.maskDropped.Load(),
		StreamsStopped: m.totals.streamsStopped.Load(),
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
	key := sessionID
	if streamCfg.Source != nil {
		key = sessionID + "/source/" + streamID
	} else if streamCfg.AIService != "" {
		key += "/" + streamCfg.AIService
	}
	ctx, cancel := context.WithCancel(parentCtx)
	if m.cfg.StreamTimeout > 0 && streamCfg.Source == nil {
		cancel()
		ctx, cancel = context.WithTimeout(parentCtx, m.cfg.StreamTimeout)
	}
	aiService := streamCfg.AIService // chỉ để log
	if aiService == "" {
		aiService = "segmentation"
		if streamCfg.Source != nil {
			aiService = "source"
		}
	}
	s := &VideoStream{SessionID: sessionID, StreamID: streamID, OpenedAt: time.Now(),
		cancel: cancel, cfg: m.cfg, totals: &m.totals, sourceMode: streamCfg.Source != nil, aiService: aiService, gestureResultOut: streamCfg.GestureResultOut}
	s.wg.Add(1)
	m.mu.Lock()
	if len(m.streams) >= m.cfg.MaxActiveStreams {
		m.mu.Unlock()
		cancel()
		s.wg.Done()
		return nil, ErrMaxStreams
	}
	if _, exists := m.streams[key]; exists {
		m.mu.Unlock()
		cancel()
		s.wg.Done()
		return nil, ErrDuplicateStream
	}
	m.streams[key] = s // reserve under a short lock; no network I/O under m.mu
	m.mu.Unlock()
	client, err := m.dialer.Dial(ctx, sessionID, streamID, streamCfg)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		cancel()
		if closer, ok := client.(io.Closer); ok {
			_ = closer.Close()
		}
		m.mu.Lock()
		if m.streams[key] == s {
			delete(m.streams, key)
		}
		m.mu.Unlock()
		s.wg.Done()
		return nil, err
	}
	go s.runWithReconnect(ctx, m.dialer, client, sessionID, streamID, streamCfg, frameIn, resultOut)

	go func() {
		s.wg.Wait()
		m.mu.Lock()
		if m.streams[key] == s {
			delete(m.streams, key)
		}
		m.mu.Unlock()
	}()

	return s, nil
}

// Close đóng video stream của session. Trả về false nếu không tồn tại.
func (m *VideoManager) Close(sessionID string) bool {
	m.mu.Lock()
	var found []*VideoStream
	for key, s := range m.streams {
		if s.SessionID == sessionID {
			found = append(found, s)
			delete(m.streams, key)
		}
	}
	m.mu.Unlock()
	for _, s := range found {
		s.cancel()
	}
	return len(found) > 0
}

// Count trả về số video stream đang mở.
func (m *VideoManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.streams)
}
