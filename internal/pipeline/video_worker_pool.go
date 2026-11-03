package pipeline

import (
	"context"
	"hash/fnv"
	"sync"
	"sync/atomic"
)

// VideoWorkerPoolConfig cấu hình VideoWorkerPool.
type VideoWorkerPoolConfig struct {
	Workers   int // số goroutine worker song song (mỗi worker sở hữu 1 shard riêng)
	QueueSize int // giới hạn queue MỖI shard
}

// DefaultVideoWorkerPoolConfig trả về cấu hình production phù hợp.
// Mỗi job vừa PushPacket vừa decode+JPEG-encode đồng bộ
func DefaultVideoWorkerPoolConfig() VideoWorkerPoolConfig {
	return VideoWorkerPoolConfig{Workers: 8, QueueSize: 1024}
}

// VideoStats giữ thống kê của VideoWorkerPool.
type VideoStats struct {
	Submitted     uint64
	Dropped       uint64 // job bị drop (queue đầy)
	Processed     uint64
	Frames        uint64 // JPEG frames enqueued for AI (before queue eviction)
	FramesDropped uint64
	DecodeErrors  uint64 // H264 decode, pixel conversion or JPEG encode errors
}

// VideoWorkerPool nhận VideoJob (RTP packet H264), đẩy vào decode chain
// per-session. Luồng: Submit() → bounded queue → worker → sessionVideoPipeline.push().
//
// Decode + rút frame + FPS-sample + JPEG-encode đều chạy ĐỒNG BỘ ngay
// trong worker() khi xử lý job (xem sessionVideoPipeline.push() ở
type VideoWorkerPool struct {
	cfg    VideoWorkerPoolConfig
	shards []chan VideoJob

	mu        sync.RWMutex
	pipelines map[string]*sessionVideoPipeline

	submitted     atomic.Uint64
	dropped       atomic.Uint64
	processed     atomic.Uint64
	frames        atomic.Uint64
	framesDropped atomic.Uint64
	decodeErrors  atomic.Uint64
}

// NewVideoWorkerPool tạo VideoWorkerPool với config cho trước.
func NewVideoWorkerPool(cfg VideoWorkerPoolConfig) *VideoWorkerPool {
	if cfg.Workers <= 0 {
		cfg.Workers = 1
	}
	shards := make([]chan VideoJob, cfg.Workers)
	for i := range shards {
		shards[i] = make(chan VideoJob, cfg.QueueSize)
	}
	return &VideoWorkerPool{
		cfg:       cfg,
		shards:    shards,
		pipelines: make(map[string]*sessionVideoPipeline),
	}
}

// shardFor trả về index shard cố định cho 1 sessionID (FNV-1a hash % số shard).
// Cùng sessionID luôn map về cùng shard trong suốt vòng đời pool.
func (wp *VideoWorkerPool) shardFor(sessionID string) int {
	h := fnv.New32a()
	h.Write([]byte(sessionID))
	return int(h.Sum32() % uint32(len(wp.shards)))
}

// RegisterSession tạo decode pipeline cho session. frameOut nhận VideoFrame
// đã JPEG-sample, sẵn sàng gửi AI. Trả về ErrSessionExists nếu đã đăng ký.
func (wp *VideoWorkerPool) RegisterSession(ctx context.Context, cfg VideoSessionConfig, frameOut chan VideoFrame) error {
	sp, err := newSessionVideoPipeline(ctx, cfg, frameOut)
	if err != nil {
		return err
	}

	wp.mu.Lock()
	defer wp.mu.Unlock()
	if _, ok := wp.pipelines[cfg.SessionID]; ok {
		sp.close()
		return ErrSessionExists
	}
	wp.pipelines[cfg.SessionID] = sp
	return nil
}

// UnregisterSession đóng decoder (giải phóng AVCodecContext) của session.
// Trả về false nếu session không tồn tại.
func (wp *VideoWorkerPool) UnregisterSession(sessionID string) bool {
	wp.mu.Lock()
	sp, ok := wp.pipelines[sessionID]
	if ok {
		delete(wp.pipelines, sessionID)
	}
	wp.mu.Unlock()

	if !ok {
		return false
	}
	sp.close()
	return true
}

// Submit đẩy VideoJob vào đúng shard của session (non-blocking). Trả về ErrQueueFull nếu shard đó đầy.
func (wp *VideoWorkerPool) Submit(ctx context.Context, job VideoJob) error {
	wp.submitted.Add(1)
	shard := wp.shards[wp.shardFor(job.SessionID)]
	select {
	case shard <- job:
		return nil
	case <-ctx.Done():
		wp.dropped.Add(1)
		return ctx.Err()
	default:
		wp.dropped.Add(1)
		return ErrQueueFull
	}
}

// Run khởi động 1 worker goroutine cho MỖI shard và block cho đến khi ctx done.
func (wp *VideoWorkerPool) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := range wp.shards {
		wg.Add(1)
		go func(shard chan VideoJob) {
			defer wg.Done()
			wp.worker(ctx, shard)
		}(wp.shards[i])
	}
	wg.Wait()
}

func (wp *VideoWorkerPool) worker(ctx context.Context, shard chan VideoJob) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-shard:
			wp.mu.RLock()
			sp, ok := wp.pipelines[job.SessionID]
			wp.mu.RUnlock()
			if !ok {
				continue
			}
			// One shard owns each session. Accumulate deltas so counters survive
			// UnregisterSession without retaining closed decoders or session IDs.
			frames, dropped, errors := sp.framesOut.Load(), sp.framesDropped.Load(), sp.decodeErrs.Load()
			sp.push(job.Packet)
			wp.frames.Add(sp.framesOut.Load() - frames)
			wp.framesDropped.Add(sp.framesDropped.Load() - dropped)
			wp.decodeErrors.Add(sp.decodeErrs.Load() - errors)
			wp.processed.Add(1)
		}
	}
}

// Stats trả về bản sao thống kê tại thời điểm gọi.
func (wp *VideoWorkerPool) Stats() VideoStats {
	return VideoStats{
		Submitted:     wp.submitted.Load(),
		Dropped:       wp.dropped.Load(),
		Processed:     wp.processed.Load(),
		Frames:        wp.frames.Load(),
		FramesDropped: wp.framesDropped.Load(),
		DecodeErrors:  wp.decodeErrors.Load(),
	}
}

// SessionCount trả về số session video đang đăng ký.
func (wp *VideoWorkerPool) SessionCount() int {
	wp.mu.RLock()
	defer wp.mu.RUnlock()
	return len(wp.pipelines)
}

// QueueLen trả về số job đang chờ trong queue.
func (wp *VideoWorkerPool) QueueLen() int {
	total := 0
	for _, s := range wp.shards {
		total += len(s)
	}
	return total
}

// QueueCap trả về tổng dung lượng job queue.
func (wp *VideoWorkerPool) QueueCap() int { return wp.cfg.QueueSize * len(wp.shards) }
