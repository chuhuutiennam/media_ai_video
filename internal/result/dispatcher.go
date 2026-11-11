package result

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"media-ai-gateway/internal/pipeline"
)

// Config giữ cấu hình cho Dispatcher.
type Config struct {
	OrderedGestures     bool // keyword callbacks use one FIFO consumer per session
	Workers             int
	QueueSize           int
	DropPartialWhenFull bool
	SendTimeout         time.Duration
}

// DefaultConfig trả về cấu hình mặc định phù hợp với production.
func DefaultConfig() Config {
	return Config{
		Workers:             16,
		QueueSize:           4096,
		DropPartialWhenFull: true,
		SendTimeout:         2 * time.Second,
	}
}

// Stats giữ thống kê của Dispatcher tại một thời điểm.
type Stats struct {
	Pushed      uint64 // tổng Push() calls
	Dropped     uint64 // items bị drop (queue đầy + partial, hoặc ctx done)
	Sent        uint64 // items được deliver thành công tới sink (tổng)
	SentPartial uint64 // trong đó: partial results (IsFinal=false)
	SentFinal   uint64 // trong đó: final results (IsFinal=true)
	SendErrors  uint64 // items mà sink.Send trả về error
}

// Dispatcher nhận RecognitionResult và phân phối sang đúng sink theo session_id.
//
// Luồng: Push() → task-specific bounded queue → dedicated worker pool → per-session Sink list.
// Workers and QueueSize are per lane: audio/default, video_segment, gesture_recognition.
//
// Drop policy (khi queue đầy):
//   - Partial result (IsFinal=false) + DropPartialWhenFull=true → drop ngay, trả nil.
//   - Final result (IsFinal=true) → block cho đến khi có chỗ hoặc ctx done.
//
// Goroutine-safe. Run() phải được gọi để khởi động worker pool.
type Dispatcher struct {
	cfg   Config
	queue chan pipeline.RecognitionResult

	mu    sync.RWMutex
	sinks map[string][]Sink // sessionID → danh sách sink

	pushed      atomic.Uint64
	dropped     atomic.Uint64
	sent        atomic.Uint64
	sentPartial atomic.Uint64
	sentFinal   atomic.Uint64
	sendErrors  atomic.Uint64

	maskQueue     chan pipeline.RecognitionResult
	gestureQueue  chan pipeline.RecognitionResult
	gestureShards []chan pipeline.RecognitionResult
	taskHTTP      map[string]map[string]*HTTPCallbackSink // optional task URL overrides
}

// NewDispatcher tạo Dispatcher với config cho trước.
func NewDispatcher(cfg Config) *Dispatcher {
	d := &Dispatcher{
		cfg:       cfg,
		queue:     make(chan pipeline.RecognitionResult, cfg.QueueSize),
		maskQueue: make(chan pipeline.RecognitionResult, cfg.QueueSize),
		sinks:     make(map[string][]Sink),
		taskHTTP:  make(map[string]map[string]*HTTPCallbackSink),
	}
	if cfg.OrderedGestures && cfg.Workers > 0 {
		for i := 0; i < cfg.Workers; i++ {
			d.gestureShards = append(d.gestureShards, make(chan pipeline.RecognitionResult, max(1, (cfg.QueueSize+cfg.Workers-1)/cfg.Workers)))
		}
	} else {
		d.gestureQueue = make(chan pipeline.RecognitionResult, cfg.QueueSize)
	}
	return d
}

// RegisterSink thêm một sink cho session.
// Gọi nhiều lần để thêm nhiều sink (vd: HTTP + WebSocket).
func (d *Dispatcher) RegisterSink(sessionID string, sink Sink) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sinks[sessionID] = append(d.sinks[sessionID], sink)
}

// UnregisterSinks xóa toàn bộ sink của session. Gọi khi session đóng.
func (d *Dispatcher) UnregisterSinks(sessionID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.sinks, sessionID)
	delete(d.taskHTTP, sessionID)
}

// Push đẩy result vào hàng đợi. Áp dụng drop policy khi queue đầy.
// Trả về nil khi drop partial (không phải lỗi); trả về ctx.Err() khi ctx done.
func (d *Dispatcher) Push(ctx context.Context, r pipeline.RecognitionResult) error {
	d.pushed.Add(1)
	queue := d.queueFor(r)

	// Non-blocking attempt — try queue before ctx so results that fit are
	// delivered even when ctx was just cancelled (e.g. session teardown).
	select {
	case queue <- r:
		return nil
	default:
		if ctx.Err() != nil {
			d.dropped.Add(1)
			return ctx.Err()
		}
	}

	// Queue đầy: áp dụng drop policy.
	if d.cfg.DropPartialWhenFull && !r.IsFinal {
		d.dropped.Add(1)
		return nil // drop partial silently
	}

	// Final result (hoặc DropPartialWhenFull=false): block cho đến khi có chỗ.
	select {
	case queue <- r:
		return nil
	case <-ctx.Done():
		d.dropped.Add(1)
		return ctx.Err()
	}
}

// Run khởi động worker pool và block cho đến khi ctx done.
// Mọi worker thoát sạch sau khi ctx bị cancel.
func (d *Dispatcher) Run(ctx context.Context) {
	var wg sync.WaitGroup
	queues := []chan pipeline.RecognitionResult{d.queue, d.maskQueue}
	if len(d.gestureShards) == 0 {
		queues = append(queues, d.gestureQueue)
	}
	for _, queue := range queues {
		for i := 0; i < d.cfg.Workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				d.worker(ctx, queue)
			}()
		}
	}
	for _, queue := range d.gestureShards {
		wg.Add(1)
		go func() { defer wg.Done(); d.worker(ctx, queue) }()
	}
	wg.Wait()
}

func (d *Dispatcher) worker(ctx context.Context, queue <-chan pipeline.RecognitionResult) {
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-queue:
			d.dispatch(ctx, r)
		}
	}
}

// dispatch tìm sink của session và gọi Send cho từng sink.
// Copy sink list trước khi gọi để không giữ lock trong suốt quá trình Send.
func (d *Dispatcher) dispatch(ctx context.Context, r pipeline.RecognitionResult) {
	d.mu.RLock()
	src := d.sinks[r.SessionID]
	sinks := make([]Sink, len(src))
	copy(sinks, src)
	task := ""
	if r.IsVideoGesture() {
		task = "gesture_recognition"
	} else if r.IsVideoMask() {
		task = "video_segment"
	}
	if override := d.taskHTTP[r.SessionID][task]; override != nil {
		filtered := sinks[:0]
		for _, sink := range sinks {
			if sink.Type() != "http_callback" {
				filtered = append(filtered, sink)
			}
		}
		sinks = append(filtered, override)
	}
	d.mu.RUnlock()

	if len(sinks) == 0 {
		d.dropped.Add(1)
		zap.L().Debug("result: no sinks registered — result dropped",
			zap.String("session_id", r.SessionID),
			zap.String("kind", r.Kind),
			zap.Bool("is_final", r.IsFinal),
		)
		return
	}

	for _, sink := range sinks {
		if ctx.Err() != nil {
			return // dispatcher shutting down
		}
		err := sink.Send(ctx, r)
		httpSink, isHTTP := sink.(*HTTPCallbackSink)
		if err != nil {
			d.sendErrors.Add(1)
			// Callback bắt đầu lỗi: Warn một lần (thấy được ở log level info); các lần lỗi tiếp theo
			// chỉ Debug để không tràn log khi MF chết. Metric dispatcher_send_errors_total đếm mọi lần.
			if isHTTP && !errors.Is(err, context.Canceled) && httpSink.markFailed() {
				zap.L().Warn("result: callback delivery failing",
					zap.String("session_id", r.SessionID), zap.String("url", httpSink.URL),
					zap.String("kind", r.Kind), zap.Error(err))
			} else {
				zap.L().Debug("result: sink send error",
					zap.String("session_id", r.SessionID),
					zap.String("sink_type", sink.Type()),
					zap.String("kind", r.Kind),
					zap.Bool("is_final", r.IsFinal),
					zap.Error(err),
				)
			}
		} else {
			if isHTTP && httpSink.markDelivered() {
				zap.L().Info("result: callback delivery recovered",
					zap.String("session_id", r.SessionID), zap.String("url", httpSink.URL))
			}
			d.sent.Add(1)
			if r.IsFinal {
				d.sentFinal.Add(1)
			} else {
				d.sentPartial.Add(1)
			}
		}
	}
}

// Stats trả về bản sao thống kê tại thời điểm gọi.
func (d *Dispatcher) Stats() Stats {
	return Stats{
		Pushed:      d.pushed.Load(),
		Dropped:     d.dropped.Load(),
		Sent:        d.sent.Load(),
		SentPartial: d.sentPartial.Load(),
		SentFinal:   d.sentFinal.Load(),
		SendErrors:  d.sendErrors.Load(),
	}
}

// ReplaceHTTPSink thay thế http_callback sink của session.
// Các sink loại khác (DataChannel, v.v.) được giữ nguyên.
// sink=nil → chỉ xóa sink cũ, không thêm mới.
func (d *Dispatcher) ReplaceHTTPSink(sessionID string, sink *HTTPCallbackSink) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var filtered []Sink
	for _, s := range d.sinks[sessionID] {
		if s.Type() != "http_callback" {
			filtered = append(filtered, s)
		}
	}
	if sink != nil {
		filtered = append(filtered, sink)
	}
	if len(filtered) == 0 {
		delete(d.sinks, sessionID)
	} else {
		d.sinks[sessionID] = filtered
	}
}

// QueueLen trả về số result đang chờ trong queue.
func (d *Dispatcher) QueueLen() int {
	return len(d.queue) + len(d.maskQueue) + d.TaskQueueLen("gesture_recognition")
}

// TaskQueueLen exposes each bounded callback lane without session labels.
func (d *Dispatcher) TaskQueueLen(task string) int {
	switch task {
	case "video_segment":
		return len(d.maskQueue)
	case "gesture_recognition":
		n := len(d.gestureQueue)
		for _, queue := range d.gestureShards {
			n += len(queue)
		}
		return n
	default:
		return len(d.queue)
	}
}

func (d *Dispatcher) queueFor(r pipeline.RecognitionResult) chan pipeline.RecognitionResult {
	if r.IsVideoGesture() {
		if len(d.gestureShards) > 0 {
			// FNV-1a provides session affinity without retaining session IDs.
			h := uint32(2166136261)
			for i := 0; i < len(r.SessionID); i++ {
				h = (h ^ uint32(r.SessionID[i])) * 16777619
			}
			return d.gestureShards[int(h)%len(d.gestureShards)]
		}
		return d.gestureQueue
	}
	if r.IsVideoMask() {
		return d.maskQueue
	}
	return d.queue
}

// ReplaceTaskHTTPSink overrides only HTTP delivery for one video task. A nil
// sink removes the override, restoring the session's callback URL.
func (d *Dispatcher) ReplaceTaskHTTPSink(sessionID, task string, sink *HTTPCallbackSink) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if sink == nil {
		delete(d.taskHTTP[sessionID], task)
		return
	}
	if d.taskHTTP[sessionID] == nil {
		d.taskHTTP[sessionID] = make(map[string]*HTTPCallbackSink)
	}
	d.taskHTTP[sessionID][task] = sink
}
