package jitter

import (
	"context"
	"testing"
	"time"

	"media-ai-gateway/internal/pipeline"
)

func makePkt(seq uint16) pipeline.MediaPacket {
	return pipeline.MediaPacket{
		SessionID:    "test-session",
		Sequence:     seq,
		ReceivedAtMs: time.Now().UnixMilli(),
		Payload:      []byte{0x01, 0x02},
	}
}

// collect đọc packet từ out channel cho đến khi hết timeout.
func collect(out <-chan pipeline.MediaPacket, timeout time.Duration) []uint16 {
	var seqs []uint16
	deadline := time.After(timeout)
	for {
		select {
		case pkt := <-out:
			seqs = append(seqs, pkt.Sequence)
		case <-deadline:
			return seqs
		}
	}
}

// newBuf tạo buffer test với BufferMs nhỏ để test nhanh.
func newBuf() *Buffer {
	return New(Config{BufferMs: 40, MaxLateMs: 120, PacketTimeMs: 10})
}

// TestInOrder: packet đến đúng thứ tự phải ra đúng thứ tự.
func TestInOrder(t *testing.T) {
	b := newBuf()
	out := make(chan pipeline.MediaPacket, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx, out)

	for seq := uint16(100); seq < 105; seq++ {
		b.Push(makePkt(seq))
	}

	// Chờ accumulation window (40ms) + vài tick flush.
	time.Sleep(150 * time.Millisecond)
	seqs := collect(out, 30*time.Millisecond)

	if len(seqs) != 5 {
		t.Fatalf("expected 5 packets, got %d: %v", len(seqs), seqs)
	}
	for i, seq := range seqs {
		if seq != uint16(100+i) {
			t.Errorf("pos %d: expected seq %d, got %d", i, 100+i, seq)
		}
	}
}

// TestReorder: packet đến đảo thứ tự phải ra đúng thứ tự.
func TestReorder(t *testing.T) {
	b := newBuf()
	out := make(chan pipeline.MediaPacket, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx, out)

	// Gửi đảo thứ tự: 102 trước, sau đó 100, 101.
	b.Push(makePkt(102))
	b.Push(makePkt(100))
	b.Push(makePkt(101))

	time.Sleep(150 * time.Millisecond)
	seqs := collect(out, 30*time.Millisecond)

	if len(seqs) != 3 {
		t.Fatalf("expected 3 packets, got %d: %v", len(seqs), seqs)
	}
	for i, seq := range seqs {
		if seq != uint16(100+i) {
			t.Errorf("pos %d: expected seq %d, got %d", i, 100+i, seq)
		}
	}
}

// TestPacketLoss: packet bị mất phải được khai báo lost sau BufferMs.
func TestPacketLoss(t *testing.T) {
	b := newBuf() // BufferMs=40
	out := make(chan pipeline.MediaPacket, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx, out)

	// seq 100 và 102 có mặt, seq 101 bị mất.
	b.Push(makePkt(100))
	b.Push(makePkt(102))

	// Chờ: 40ms accumulation + 40ms holdUntil cho 100 + 40ms holdUntil cho 101 (lost).
	time.Sleep(200 * time.Millisecond)
	seqs := collect(out, 30*time.Millisecond)

	if len(seqs) != 2 {
		t.Fatalf("expected 2 packets [100 102], got %d: %v", len(seqs), seqs)
	}
	if seqs[0] != 100 || seqs[1] != 102 {
		t.Errorf("expected [100 102], got %v", seqs)
	}
	if b.Stats().Lost == 0 {
		t.Error("expected lost counter > 0")
	}
}

// TestLateDrop: packet đến sau MaxLateMs phải bị drop, bất kể trạng thái buffer.
func TestLateDrop(t *testing.T) {
	b := New(Config{BufferMs: 40, MaxLateMs: 50, PacketTimeMs: 10})
	out := make(chan pipeline.MediaPacket, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx, out)

	// Packet hợp lệ.
	b.Push(makePkt(100))
	time.Sleep(200 * time.Millisecond)

	// Packet seq 101 với ReceivedAtMs 200ms về trước → age=200ms > MaxLateMs=50ms.
	latePkt := makePkt(101)
	latePkt.ReceivedAtMs = time.Now().Add(-200 * time.Millisecond).UnixMilli()
	if b.Push(latePkt) {
		t.Error("late packet should have been dropped")
	}
	if b.Stats().Dropped == 0 {
		t.Error("expected dropped counter > 0")
	}
}

// TestSeqWraparound: sequence number wrap tại 65535 → 0.
func TestSeqWraparound(t *testing.T) {
	b := newBuf()
	out := make(chan pipeline.MediaPacket, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx, out)

	// Gửi đảo thứ tự xung quanh wraparound.
	for _, s := range []uint16{65534, 0, 65535, 1} {
		b.Push(makePkt(s))
	}

	time.Sleep(150 * time.Millisecond)
	seqs := collect(out, 30*time.Millisecond)

	expected := []uint16{65534, 65535, 0, 1}
	if len(seqs) != len(expected) {
		t.Fatalf("wraparound: expected %v, got %v", expected, seqs)
	}
	for i := range expected {
		if seqs[i] != expected[i] {
			t.Errorf("pos %d: expected %d, got %d", i, expected[i], seqs[i])
		}
	}
}

// --- Jitter metric (RFC 3550 §6.4.1) ---

func TestJitter_InitialZero(t *testing.T) {
	b := newBuf()
	if s := b.Stats(); s.Jitter != 0 {
		t.Errorf("initial jitter = %d, want 0", s.Jitter)
	}
}

// TestJitter_ZeroForConstantArrival: khi inter-arrival = inter-send, D=0 → jitter ≈ 0.
func TestJitter_ZeroForConstantArrival(t *testing.T) {
	b := newBuf()
	const sampleRate = 8000
	const ptimeMs = 20
	const samplesPerPacket = sampleRate * ptimeMs / 1000 // 160
	now := time.Now().UnixMilli()

	for i := 0; i < 5; i++ {
		b.Push(pipeline.MediaPacket{
			Sequence:     uint16(i),
			Timestamp:    uint32(i * samplesPerPacket),
			SampleRate:   sampleRate,
			ReceivedAtMs: now + int64(i*ptimeMs),
		})
	}
	if s := b.Stats(); s.Jitter > 2 {
		t.Errorf("jitter for constant arrival = %d, want ≈ 0", s.Jitter)
	}
}

// TestJitter_IncreasesWithVariance: arrival không đều → jitter > 0.
func TestJitter_IncreasesWithVariance(t *testing.T) {
	b := newBuf()
	const sampleRate = 8000
	now := time.Now().UnixMilli()
	// Packets arrive with irregular spacing (0ms, 30ms, 5ms gaps).
	arrivals := []int64{0, 30, 35}
	for i, arrMs := range arrivals {
		b.Push(pipeline.MediaPacket{
			Sequence:     uint16(i),
			Timestamp:    uint32(i * 160),
			SampleRate:   sampleRate,
			ReceivedAtMs: now + arrMs,
		})
	}
	if s := b.Stats(); s.Jitter == 0 {
		t.Error("jitter should be > 0 after irregular inter-arrival")
	}
}

// TestJitter_NoSampleRate_Skipped: packet thiếu SampleRate không làm panic hay ảnh hưởng jitter.
func TestJitter_NoSampleRate_Skipped(t *testing.T) {
	b := newBuf()
	b.Push(pipeline.MediaPacket{Sequence: 1, Timestamp: 0, SampleRate: 0, ReceivedAtMs: time.Now().UnixMilli()})
	b.Push(pipeline.MediaPacket{Sequence: 2, Timestamp: 160, SampleRate: 0, ReceivedAtMs: time.Now().UnixMilli()})
	// No panic and jitter stays 0 when SampleRate is missing.
	if s := b.Stats(); s.Jitter != 0 {
		t.Errorf("jitter without SampleRate = %d, want 0", s.Jitter)
	}
}

// TestSeqDiff kiểm tra logic wraparound của SeqDiff.
func TestSeqDiff(t *testing.T) {
	cases := []struct {
		a, b uint16
		want int
	}{
		{101, 100, 1},
		{100, 101, -1},
		{0, 65535, 1},  // 0 đến sau 65535
		{65535, 0, -1}, // 65535 đến trước 0
		{100, 100, 0},
	}
	for _, c := range cases {
		got := SeqDiff(c.a, c.b)
		if got != c.want {
			t.Errorf("SeqDiff(%d, %d) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
