// Package jitter cung cấp jitter buffer per-session cho RTP audio stream.
//
// Thiết kế:
//   - Giữ packet trong min-heap theo sequence number.
//   - Chờ BufferMs từ packet đầu tiên trước khi bắt đầu release (accumulation window).
//   - Flush mỗi PacketTimeMs (20ms): release packet đúng thứ tự.
//   - Nếu packet thiếu và đã chờ quá BufferMs: ghi nhận lost, tiếp tục.
//   - Packet đến quá trễ (> MaxLateMs kể từ ReceivedAtMs): drop ngay.
package jitter

import (
	"container/heap"
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"media-ai-gateway/internal/pipeline"
)

// Config cấu hình jitter buffer.
type Config struct {
	BufferMs     int // cửa sổ chờ reorder và holdUntil sau mỗi gap (mặc định 60ms)
	MaxLateMs    int // packet cũ hơn mức này theo wall clock sẽ bị drop (mặc định 120ms)
	PacketTimeMs int // chu kỳ flush, khớp ptime codec (mặc định 20ms)
}

func DefaultConfig() Config {
	return Config{
		BufferMs:     60,
		MaxLateMs:    120,
		PacketTimeMs: 20,
	}
}

// Stats là snapshot metrics của buffer tại một thời điểm.
type Stats struct {
	Received uint64
	Released uint64
	Dropped  uint64 // đến quá trễ, duplicate, hoặc output channel đầy
	Lost     uint64 // gap được phát hiện (packet không bao giờ đến)
	Jitter   uint32 // inter-arrival jitter theo RFC 3550 §6.4.1 (RTP timestamp units)
}

// Buffer là jitter buffer cho một RTP session.
// Goroutine-safe: Push() gọi được từ nhiều goroutine; Run() chạy một goroutine flush riêng.
type Buffer struct {
	cfg Config

	mu           sync.Mutex
	h            packetHeap
	started      bool      // đã thấy packet đầu tiên chưa
	firstAt      time.Time // thời điểm packet đầu tiên đến
	nextSet      bool      // nextExpected đã được khởi tạo chưa
	nextExpected uint16
	holdUntil    time.Time // deadline chờ nextExpected hiện tại

	// lastProcessed bảo vệ khỏi duplicate sau khi đã release.
	hasProcessed  bool
	lastProcessed uint16

	// RFC 3550 §6.4.1 inter-arrival jitter state (under mu).
	jSampleRate  int
	jLastArrival int64   // previous arrival time in RTP timestamp units
	jLastSend    uint32  // previous RTP Timestamp field
	jHasLast     bool
	jVal         float64 // running jitter estimate

	received    atomic.Uint64
	released    atomic.Uint64
	dropped     atomic.Uint64
	lost        atomic.Uint64
	jitterBits  atomic.Uint64 // math.Float64bits(jVal), written under mu, read lock-free
}

// New tạo jitter buffer mới.
func New(cfg Config) *Buffer {
	return &Buffer{cfg: cfg}
}

// Push nhận một MediaPacket vào buffer.
// Trả về false nếu packet bị drop.
func (b *Buffer) Push(pkt pipeline.MediaPacket) bool {
	now := time.Now()

	b.mu.Lock()
	defer b.mu.Unlock()

	// Luôn kiểm tra tuổi wall-clock — không phụ thuộc trạng thái initialized.
	age := now.Sub(time.UnixMilli(pkt.ReceivedAtMs))
	if age > time.Duration(b.cfg.MaxLateMs)*time.Millisecond {
		b.dropped.Add(1)
		return false
	}

	// Drop duplicate của seq đã được release hoặc declare lost.
	if b.hasProcessed && SeqDiff(pkt.Sequence, b.lastProcessed) <= 0 {
		b.dropped.Add(1)
		return false
	}

	heap.Push(&b.h, entry{pkt: pkt, arrivedAt: now})
	b.received.Add(1)

	if !b.started {
		b.firstAt = now
		b.started = true
	}

	b.updateJitter(pkt)

	return true
}

// Run chạy flush loop. Phải gọi trong goroutine riêng.
// Thoát khi ctx bị cancel.
func (b *Buffer) Run(ctx context.Context, out chan<- pipeline.MediaPacket) {
	ticker := time.NewTicker(time.Duration(b.cfg.PacketTimeMs) * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			b.flush(now, out)
		}
	}
}

// Stats trả về snapshot metrics hiện tại (thread-safe).
func (b *Buffer) Stats() Stats {
	return Stats{
		Received: b.received.Load(),
		Released: b.released.Load(),
		Dropped:  b.dropped.Load(),
		Lost:     b.lost.Load(),
		Jitter:   uint32(math.Float64frombits(b.jitterBits.Load())),
	}
}

// updateJitter tính inter-arrival jitter theo RFC 3550 §6.4.1.
// Phải gọi dưới b.mu.Lock().
func (b *Buffer) updateJitter(pkt pipeline.MediaPacket) {
	if pkt.SampleRate <= 0 {
		return
	}
	if b.jSampleRate == 0 {
		b.jSampleRate = pkt.SampleRate
	}
	// Arrival time converted to RTP timestamp units.
	arrivalRTP := pkt.ReceivedAtMs * int64(b.jSampleRate) / 1000
	if b.jHasLast {
		// D(i-1,i) = (arrival_i - arrival_{i-1}) - (send_i - send_{i-1})
		// Use int32 cast for send diff to handle RTP timestamp wraparound.
		sendDiff := int64(int32(pkt.Timestamp - b.jLastSend))
		arrivalDiff := arrivalRTP - b.jLastArrival
		d := arrivalDiff - sendDiff
		if d < 0 {
			d = -d
		}
		b.jVal += (float64(d) - b.jVal) / 16
		b.jitterBits.Store(math.Float64bits(b.jVal))
	}
	b.jLastArrival = arrivalRTP
	b.jLastSend = pkt.Timestamp
	b.jHasLast = true
}

func (b *Buffer) flush(now time.Time, out chan<- pipeline.MediaPacket) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.started || len(b.h) == 0 {
		return
	}

	bufDur := time.Duration(b.cfg.BufferMs) * time.Millisecond

	// Accumulation window: chờ BufferMs từ packet đầu để gom reorder.
	// Sau đó lấy seq nhỏ nhất trong heap làm điểm xuất phát.
	if !b.nextSet {
		if now.Before(b.firstAt.Add(bufDur)) {
			return
		}
		b.nextExpected = b.h[0].pkt.Sequence // heap[0] là min seq
		b.nextSet = true
		b.holdUntil = now.Add(bufDur)
	}

	// Release loop — dùng if-else để break thoát được for, không dùng switch.
	for len(b.h) > 0 {
		top := b.h[0]
		diff := SeqDiff(top.pkt.Sequence, b.nextExpected)

		if diff == 0 {
			// Đúng packet cần release.
			heap.Pop(&b.h)
			select {
			case out <- top.pkt:
				b.released.Add(1)
			default:
				// Output channel đầy — drop để không block flush loop.
				b.dropped.Add(1)
			}
			b.lastProcessed = b.nextExpected
			b.hasProcessed = true
			b.nextExpected++
			b.holdUntil = now.Add(bufDur)

		} else if diff < 0 {
			// Packet cũ hơn nextExpected (duplicate muộn) — discard.
			heap.Pop(&b.h)
			b.dropped.Add(1)

		} else {
			// diff > 0: heap có packet ở phía trước, đang thiếu nextExpected.
			if now.After(b.holdUntil) {
				// Đã chờ đủ lâu → declare lost, tiến tới seq tiếp theo.
				b.lastProcessed = b.nextExpected
				b.hasProcessed = true
				b.lost.Add(1)
				b.nextExpected++
				b.holdUntil = now.Add(bufDur)
				// Tiếp tục vòng lặp để thử release seq mới.
			} else {
				// Vẫn trong cửa sổ chờ — dừng flush lần này.
				break
			}
		}
	}
}

// --- min-heap theo sequence number (xử lý wraparound qua SeqLess) ---

type entry struct {
	pkt       pipeline.MediaPacket
	arrivedAt time.Time
}

type packetHeap []entry

func (h packetHeap) Len() int           { return len(h) }
func (h packetHeap) Less(i, j int) bool { return SeqLess(h[i].pkt.Sequence, h[j].pkt.Sequence) }
func (h packetHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *packetHeap) Push(x any) {
	*h = append(*h, x.(entry))
}

func (h *packetHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}
