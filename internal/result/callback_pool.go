package result

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"media-ai-gateway/internal/pipeline"
)

// ErrCallbackQueueFull được trả về bởi CallbackPool.Send khi queue đầy.
var ErrCallbackQueueFull = errors.New("result: callback pool queue full")

// CallbackPoolConfig cấu hình CallbackPool.
type CallbackPoolConfig struct {
	Workers     int           // số goroutine worker song song
	QueueSize   int           // giới hạn input queue
	SendTimeout time.Duration // timeout cho mỗi lần gọi sink.Send; 0 = không timeout
}

// DefaultCallbackPoolConfig trả về cấu hình mặc định phù hợp với production.
func DefaultCallbackPoolConfig() CallbackPoolConfig {
	return CallbackPoolConfig{
		Workers:     8,
		QueueSize:   1024,
		SendTimeout: 5 * time.Second,
	}
}

// CallbackPoolStats giữ thống kê của CallbackPool.
type CallbackPoolStats struct {
	Pushed     uint64
	Dropped    uint64
	Sent       uint64
	SendErrors uint64
}

// CallbackPool bọc một Sink với dedicated worker pool riêng.
//
// Dispatcher workers gọi Send() — non-blocking, chỉ push vào bounded queue.
// Callback workers tách biệt drain queue và gọi sink.Send() với SendTimeout,
// nên retry latency của HTTP callback không block dispatcher workers.
//
// Implements Sink. Phải gọi Run() để khởi động workers.
type CallbackPool struct {
	cfg   CallbackPoolConfig
	sink  Sink
	queue chan pipeline.RecognitionResult

	pushed     atomic.Uint64
	dropped    atomic.Uint64
	sent       atomic.Uint64
	sendErrors atomic.Uint64
}

// NewCallbackPool tạo CallbackPool wrapping sink với config cho trước.
func NewCallbackPool(cfg CallbackPoolConfig, sink Sink) *CallbackPool {
	return &CallbackPool{
		cfg:   cfg,
		sink:  sink,
		queue: make(chan pipeline.RecognitionResult, cfg.QueueSize),
	}
}

func (p *CallbackPool) Type() string { return "callback_pool" }

// Send pushes result vào pool queue (non-blocking).
// Trả về ErrCallbackQueueFull nếu queue đầy; không bao giờ block.
// ctx từ caller bị bỏ qua — pool có lifecycle riêng qua Run(ctx).
func (p *CallbackPool) Send(_ context.Context, r pipeline.RecognitionResult) error {
	p.pushed.Add(1)
	select {
	case p.queue <- r:
		return nil
	default:
		p.dropped.Add(1)
		return ErrCallbackQueueFull
	}
}

// Run khởi động worker pool và block cho đến khi ctx done.
func (p *CallbackPool) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < p.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.worker(ctx)
		}()
	}
	wg.Wait()
}

func (p *CallbackPool) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-p.queue:
			p.callSink(ctx, r)
		}
	}
}

func (p *CallbackPool) callSink(ctx context.Context, r pipeline.RecognitionResult) {
	sendCtx := ctx
	var cancel context.CancelFunc
	if p.cfg.SendTimeout > 0 {
		sendCtx, cancel = context.WithTimeout(ctx, p.cfg.SendTimeout)
		defer cancel()
	}
	if err := p.sink.Send(sendCtx, r); err != nil {
		p.sendErrors.Add(1)
	} else {
		p.sent.Add(1)
	}
}

// Stats trả về bản sao thống kê tại thời điểm gọi.
func (p *CallbackPool) Stats() CallbackPoolStats {
	return CallbackPoolStats{
		Pushed:     p.pushed.Load(),
		Dropped:    p.dropped.Load(),
		Sent:       p.sent.Load(),
		SendErrors: p.sendErrors.Load(),
	}
}

// QueueLen trả về số result đang chờ trong queue.
func (p *CallbackPool) QueueLen() int { return len(p.queue) }

// QueueCap trả về tổng dung lượng queue (dùng để tính % sử dụng).
func (p *CallbackPool) QueueCap() int { return p.cfg.QueueSize }
