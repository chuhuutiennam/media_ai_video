package pipeline

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

var (
	ErrQueueFull       = errors.New("pipeline: submit queue full")
	ErrSessionExists   = errors.New("pipeline: session already registered")
	ErrSessionNotFound = errors.New("pipeline: session not found")
)

// WorkerPoolConfig cấu hình WorkerPool.
type WorkerPoolConfig struct {
	Workers   int // không còn dùng — mỗi session có goroutine riêng; giữ để tương thích config cũ
	QueueSize int // dung lượng per-session packet queue
}

// DefaultWorkerPoolConfig trả về cấu hình production phù hợp.
func DefaultWorkerPoolConfig() WorkerPoolConfig {
	return WorkerPoolConfig{Workers: 16, QueueSize: 8192}
}

// Stats giữ thống kê của WorkerPool.
type Stats struct {
	Submitted    uint64
	Dropped      uint64
	Processed    uint64
	DecodeErrors uint64
}

// WorkerPool quản lý per-session audio pipeline.
//
// Mỗi session có một goroutine riêng (sessionPipeline.run) là consumer duy nhất
// của jobCh per-session, đảm bảo thứ tự xử lý packet không bị đảo lộn.
//
// Submit() non-blocking: nếu per-session queue đầy, job bị drop và trả ErrQueueFull.
// Goroutine-safe.
type WorkerPool struct {
	cfg WorkerPoolConfig

	mu        sync.RWMutex
	pipelines map[string]*sessionPipeline

	submitted    atomic.Uint64
	dropped      atomic.Uint64
	processed    atomic.Uint64
	decodeErrors atomic.Uint64
}

// NewWorkerPool tạo WorkerPool với config cho trước.
func NewWorkerPool(cfg WorkerPoolConfig) *WorkerPool {
	return &WorkerPool{
		cfg:       cfg,
		pipelines: make(map[string]*sessionPipeline),
	}
}

// RegisterSession tạo pipeline và khởi động goroutine per-session.
// audioOut nhận AudioChunk đã xử lý.
// Trả về ErrSessionExists nếu session đã được đăng ký.
func (wp *WorkerPool) RegisterSession(ctx context.Context, cfg SessionConfig, audioOut chan<- AudioChunk) error {
	sp, err := newSessionPipeline(ctx, cfg, audioOut, wp.cfg.QueueSize, &wp.processed, &wp.decodeErrors)
	if err != nil {
		return err
	}

	wp.mu.Lock()
	defer wp.mu.Unlock()

	if _, ok := wp.pipelines[cfg.SessionID]; ok {
		sp.cancel() // dọn dẹp internal ctx đã tạo
		return ErrSessionExists
	}
	wp.pipelines[cfg.SessionID] = sp
	go sp.run()
	return nil
}

// UnregisterSession dừng goroutine của session, đợi flush() hoàn tất, rồi xóa session.
// Trả về false nếu session không tồn tại.
func (wp *WorkerPool) UnregisterSession(sessionID string) bool {
	wp.mu.Lock()
	sp, ok := wp.pipelines[sessionID]
	if ok {
		delete(wp.pipelines, sessionID)
	}
	wp.mu.Unlock()

	if !ok {
		return false
	}
	sp.cancel()  // báo goroutine thoát (hoạt động dù sess.Ctx chưa cancel)
	<-sp.done    // đợi flush() hoàn tất trước khi return
	return true
}

// Submit đẩy packet vào per-session queue (non-blocking).
// Trả về ErrQueueFull nếu queue đầy; ctx.Err() nếu ctx đã cancel; nil nếu session không tồn tại (drop silently).
func (wp *WorkerPool) Submit(ctx context.Context, job AudioJob) error {
	wp.submitted.Add(1)

	// Kiểm tra ctx trước — cancelled ctx luôn trả lỗi (deterministic).
	select {
	case <-ctx.Done():
		wp.dropped.Add(1)
		return ctx.Err()
	default:
	}

	wp.mu.RLock()
	sp, ok := wp.pipelines[job.SessionID]
	wp.mu.RUnlock()

	if !ok {
		wp.dropped.Add(1)
		return nil // session không tồn tại — drop silently
	}

	select {
	case sp.jobCh <- job.Packet:
		return nil
	default:
		wp.dropped.Add(1)
		return ErrQueueFull
	}
}

// Run block cho đến khi ctx done.
// Giữ signature cũ để tương thích với caller dùng go wp.Run(ctx).
// Per-session goroutines được quản lý bởi RegisterSession/UnregisterSession.
func (wp *WorkerPool) Run(ctx context.Context) {
	<-ctx.Done()
}

// Stats trả về bản sao thống kê tại thời điểm gọi.
func (wp *WorkerPool) Stats() Stats {
	return Stats{
		Submitted:    wp.submitted.Load(),
		Dropped:      wp.dropped.Load(),
		Processed:    wp.processed.Load(),
		DecodeErrors: wp.decodeErrors.Load(),
	}
}

// SessionCount trả về số session đang đăng ký.
func (wp *WorkerPool) SessionCount() int {
	wp.mu.RLock()
	defer wp.mu.RUnlock()
	return len(wp.pipelines)
}

// QueueLen trả về tổng số packet đang chờ trong tất cả per-session queues.
func (wp *WorkerPool) QueueLen() int {
	wp.mu.RLock()
	defer wp.mu.RUnlock()
	total := 0
	for _, sp := range wp.pipelines {
		total += len(sp.jobCh)
	}
	return total
}

// QueueCap trả về tổng dung lượng của tất cả per-session queues (SessionCount × QueueSize).
// Dùng cùng với QueueLen để tính tỷ lệ fill cho admission control.
func (wp *WorkerPool) QueueCap() int {
	if wp.cfg.QueueSize == 0 {
		return 0
	}
	wp.mu.RLock()
	n := len(wp.pipelines)
	wp.mu.RUnlock()
	if n == 0 {
		return wp.cfg.QueueSize // chưa có session — dùng 1 slot làm denominator
	}
	return n * wp.cfg.QueueSize
}
