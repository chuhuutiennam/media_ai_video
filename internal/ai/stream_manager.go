package ai

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"media-ai-gateway/internal/pipeline"
)

// Sentinel errors trả về bởi Open.
var (
	ErrMaxStreams         = errors.New("ai: max active streams reached")
	ErrDuplicateStream   = errors.New("ai: stream already open for session")
	ErrFirstChunkTimeout = errors.New("ai: first chunk deadline exceeded")
	ErrUnexpectedEOF     = errors.New("ai: stream closed by worker before end of stream")
	ErrRecvIdleTimeout   = errors.New("ai: recv idle timeout")
)

// Config giữ cấu hình cho AI Stream Manager.
type Config struct {
	MaxActiveStreams   int
	QueueSize          int           // per-stream bounded send queue; 0 = direct passthrough (unbounded)
	SendTimeout        time.Duration // timeout per Send() call; 0 = no timeout
	StreamTimeout      time.Duration // hard cap on stream lifetime
	MaxRetries         int           // max reconnect attempts on stream error; 0 = no reconnect
	RetryBackoff       time.Duration // initial backoff between reconnect attempts; 0 → 1s
	FirstChunkTimeout  time.Duration // max wait for first AudioChunk after stream opens; 0 = no deadline
	RecvIdleTimeout    time.Duration // max wait between consecutive Recv() results; 0 = no timeout
}

// DefaultConfig trả về cấu hình mặc định phù hợp với production.
func DefaultConfig() Config {
	return Config{
		MaxActiveStreams:  1000,
		QueueSize:         20,
		SendTimeout:       500 * time.Millisecond,
		StreamTimeout:     300 * time.Second,
		MaxRetries:        3,
		RetryBackoff:      time.Second,
		FirstChunkTimeout: 3 * time.Second,
		RecvIdleTimeout:   30 * time.Second,
	}
}

// ManagerStats là snapshot thống kê tích lũy của Manager (bao gồm cả stream đã đóng).
type ManagerStats struct {
	TotalSendErrors    uint64
	TotalRecvErrors    uint64
	TotalRetries       uint64
	TotalAudioDropped  uint64 // AudioChunk bị drop do AI send queue đầy
	TotalPartialDropped uint64 // partial result bị drop do resultOut đầy

	// FirstResultMs: first-result latency (stream open → first result) từ tất cả stream đã đóng + active.
	LatencyFirstCount uint64
	LatencyFirstSum   int64
}

// AvgFirstResultMs trả về first-result latency trung bình (ms); 0 nếu chưa có data.
func (s ManagerStats) AvgFirstResultMs() int64 {
	if s.LatencyFirstCount == 0 {
		return 0
	}
	return s.LatencyFirstSum / int64(s.LatencyFirstCount)
}

// Manager quản lý bidirectional gRPC stream tới AI service, một stream mỗi session.
//
// Goroutine-safe. Stream được tự động xóa khỏi map khi goroutine thoát.
type Manager struct {
	cfg     Config
	dialer  Dialer
	mu      sync.RWMutex
	streams map[string]*Stream // sessionID → Stream

	totalSendErrors       atomic.Uint64
	totalRecvErrors       atomic.Uint64
	totalRetries          atomic.Uint64
	totalAudioDropped     atomic.Uint64
	totalPartialDropped   atomic.Uint64
	totalFirstResultCount atomic.Uint64
	totalFirstResultSum   atomic.Int64
}

// Stats trả về snapshot thống kê tích lũy: tổng lỗi, retries và latency từ cả stream đang chạy và đã đóng.
func (m *Manager) Stats() ManagerStats {
	m.mu.RLock()
	var activeSend, activeRecv, activeAudioDrop, activePartialDrop uint64
	var activeRetries int
	var activeFirstCount uint64
	var activeFirstSum int64
	for _, s := range m.streams {
		st := s.Stats()
		activeSend += st.SendErrors
		activeRecv += st.RecvErrors
		activeRetries += st.Retries
		activeAudioDrop += st.AudioDropped
		activePartialDrop += st.PartialDropped
		if st.FirstResultMs > 0 {
			activeFirstCount++
			activeFirstSum += st.FirstResultMs
		}
	}
	m.mu.RUnlock()

	return ManagerStats{
		TotalSendErrors:     m.totalSendErrors.Load() + activeSend,
		TotalRecvErrors:     m.totalRecvErrors.Load() + activeRecv,
		TotalRetries:        m.totalRetries.Load() + uint64(activeRetries),
		TotalAudioDropped:   m.totalAudioDropped.Load() + activeAudioDrop,
		TotalPartialDropped: m.totalPartialDropped.Load() + activePartialDrop,
		LatencyFirstCount:   m.totalFirstResultCount.Load() + activeFirstCount,
		LatencyFirstSum:     m.totalFirstResultSum.Load() + activeFirstSum,
	}
}

// NewManager tạo Manager với config và dialer cho trước.
func NewManager(cfg Config, dialer Dialer) *Manager {
	return &Manager{
		cfg:     cfg,
		dialer:  dialer,
		streams: make(map[string]*Stream),
	}
}

// Open tạo và khởi động AI stream cho session.
//
//   - parentCtx kiểm soát lifetime: cancel nó (hoặc để StreamTimeout hết hạn) đóng stream.
//   - audioIn  được đọc bởi goroutine send và gửi lên AI worker.
//   - resultOut nhận RecognitionResult từ AI worker.
//
// Stream tự xóa khỏi Manager khi goroutine wrapper thoát.
func (m *Manager) Open(
	parentCtx context.Context,
	sessionID, streamID, language, task string,
	audioIn <-chan pipeline.AudioChunk,
	resultOut chan<- pipeline.RecognitionResult,
) (*Stream, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.streams) >= m.cfg.MaxActiveStreams {
		return nil, ErrMaxStreams
	}
	if _, exists := m.streams[sessionID]; exists {
		return nil, ErrDuplicateStream
	}

	// StreamTimeout là hard cap; parentCtx cancel thường xảy ra trước (session idle 30s).
	ctx, cancel := context.WithTimeout(parentCtx, m.cfg.StreamTimeout)

	// Dial synchronously so Open() returns an error immediately if AI is unreachable.
	client, err := m.dialer.Dial(ctx, sessionID, streamID, language, task)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("ai: dial %s: %w", sessionID, err)
	}

	s := &Stream{
		SessionID: sessionID,
		StreamID:  streamID,
		OpenedAt:  time.Now(),
		cancel:    cancel,
		cfg:       m.cfg,
	}
	s.wg.Add(1)
	go s.runWithReconnect(ctx, m.dialer, client, sessionID, streamID, language, task, audioIn, resultOut)

	m.streams[sessionID] = s
	zap.L().Debug("ai: stream opened", zap.String("session_id", sessionID), zap.String("stream_id", streamID), zap.String("language", language), zap.String("task", task))

	// Auto-cleanup: accumulate stream stats then remove from map.
	go func() {
		s.wg.Wait()
		st := s.Stats()
		m.totalSendErrors.Add(st.SendErrors)
		m.totalRecvErrors.Add(st.RecvErrors)
		m.totalRetries.Add(uint64(st.Retries))
		m.totalAudioDropped.Add(st.AudioDropped)
		m.totalPartialDropped.Add(st.PartialDropped)
		if st.FirstResultMs > 0 {
			m.totalFirstResultCount.Add(1)
			m.totalFirstResultSum.Add(st.FirstResultMs)
		}
		m.mu.Lock()
		if m.streams[sessionID] == s {
			delete(m.streams, sessionID)
		}
		m.mu.Unlock()
	}()

	return s, nil
}

// Close đóng stream của session, cancel context của nó.
// Trả về false nếu session không tồn tại.
// Goroutine thoát asynchronously — dùng s.Wait() nếu cần đồng bộ.
func (m *Manager) Close(sessionID string) bool {
	m.mu.Lock()
	s, ok := m.streams[sessionID]
	if ok {
		delete(m.streams, sessionID)
	}
	m.mu.Unlock()
	if !ok {
		return false
	}
	zap.L().Debug("ai: stream closed", zap.String("session_id", sessionID))
	s.cancel()
	return true
}

// Count trả về số stream đang mở.
func (m *Manager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.streams)
}

// MaxStreams trả về giới hạn số stream tối đa (dùng để tính % sử dụng).
func (m *Manager) MaxStreams() int { return m.cfg.MaxActiveStreams }
