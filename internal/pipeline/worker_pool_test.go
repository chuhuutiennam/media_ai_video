package pipeline

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// pcmuPayload tạo PCMU payload: 0xFF = G.711 µ-law silence (decodes to PCM ≈ 0).
func pcmuPayload(samples int) []byte {
	b := make([]byte, samples)
	for i := range b {
		b[i] = 0xFF
	}
	return b
}

// defaultSessionCfg trả về SessionConfig PCMU 8k→16k, 20ms chunks.
func defaultSessionCfg(id string) SessionConfig {
	return SessionConfig{
		Codec:         "PCMU",
		SampleRate:    8000,
		Channels:      1,
		OutSampleRate: 16000,
		OutChannels:   1,
		ChunkMs:       20,
		SessionID:     id,
		StreamID:      "stream-1",
	}
}

// makeJob tạo AudioJob chứa 20ms PCMU payload (160 bytes @ 8000 Hz).
func makeJob(sessionID string) AudioJob {
	return AudioJob{
		SessionID: sessionID,
		Packet: MediaPacket{
			SessionID:  sessionID,
			Codec:      "PCMU",
			SampleRate: 8000,
			Channels:   1,
			Payload:    pcmuPayload(160), // 20ms @ 8kHz
		},
	}
}

// ---------- RegisterSession ----------

func TestRegisterSession_Basic(t *testing.T) {
	wp := NewWorkerPool(DefaultWorkerPoolConfig())
	out := make(chan AudioChunk, 16)
	err := wp.RegisterSession(context.Background(), defaultSessionCfg("s1"), out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wp.SessionCount() != 1 {
		t.Fatalf("want 1, got %d", wp.SessionCount())
	}
}

func TestRegisterSession_DuplicateReturnsError(t *testing.T) {
	wp := NewWorkerPool(DefaultWorkerPoolConfig())
	out := make(chan AudioChunk, 16)
	_ = wp.RegisterSession(context.Background(), defaultSessionCfg("s1"), out)
	err := wp.RegisterSession(context.Background(), defaultSessionCfg("s1"), out)
	if err != ErrSessionExists {
		t.Fatalf("want ErrSessionExists, got %v", err)
	}
}

func TestRegisterSession_InvalidCodec(t *testing.T) {
	wp := NewWorkerPool(DefaultWorkerPoolConfig())
	out := make(chan AudioChunk, 16)
	cfg := defaultSessionCfg("s1")
	cfg.Codec = "UNKNOWN"
	err := wp.RegisterSession(context.Background(), cfg, out)
	if err == nil {
		t.Fatal("want error for invalid codec, got nil")
	}
}

// ---------- UnregisterSession ----------

func TestUnregisterSession_ReturnsTrueOnExist(t *testing.T) {
	wp := NewWorkerPool(DefaultWorkerPoolConfig())
	out := make(chan AudioChunk, 16)
	_ = wp.RegisterSession(context.Background(), defaultSessionCfg("s1"), out)
	if !wp.UnregisterSession("s1") {
		t.Fatal("want true")
	}
	if wp.SessionCount() != 0 {
		t.Fatal("want 0 after unregister")
	}
}

func TestUnregisterSession_ReturnsFalseOnMissing(t *testing.T) {
	wp := NewWorkerPool(DefaultWorkerPoolConfig())
	if wp.UnregisterSession("nonexistent") {
		t.Fatal("want false for unknown session")
	}
}

func TestUnregisterSession_FlushesPartialChunk(t *testing.T) {
	wp := NewWorkerPool(WorkerPoolConfig{Workers: 4, QueueSize: 512})
	out := make(chan AudioChunk, 64)
	cfg := defaultSessionCfg("s1")
	cfg.ChunkMs = 500
	_ = wp.RegisterSession(context.Background(), cfg, out)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go wp.Run(ctx)

	for i := 0; i < 5; i++ {
		if err := wp.Submit(context.Background(), makeJob("s1")); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for wp.QueueLen() > 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}

	wp.UnregisterSession("s1")

	select {
	case chunk := <-out:
		if chunk.DurationMs == 0 {
			t.Fatal("flushed chunk has zero duration")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected partial chunk from flush, got nothing")
	}
}

// ---------- Submit ----------

func TestSubmit_QueueFull(t *testing.T) {
	// Per-session queue capacity = 2. Submit bão hoà để đảm bảo ít nhất một ErrQueueFull
	// bất kể tốc độ goroutine per-session xử lý.
	wp := NewWorkerPool(WorkerPoolConfig{QueueSize: 2})
	out := make(chan AudioChunk, 256)
	_ = wp.RegisterSession(context.Background(), defaultSessionCfg("s1"), out)

	gotQueueFull := false
	for i := 0; i < 50; i++ {
		if err := wp.Submit(context.Background(), makeJob("s1")); err == ErrQueueFull {
			gotQueueFull = true
			break
		}
	}
	if !gotQueueFull {
		t.Fatal("want at least one ErrQueueFull")
	}
	if wp.Stats().Dropped == 0 {
		t.Fatal("dropped should be > 0")
	}
}

func TestSubmit_CtxCancelledDrops(t *testing.T) {
	wp := NewWorkerPool(DefaultWorkerPoolConfig())
	out := make(chan AudioChunk, 16)
	_ = wp.RegisterSession(context.Background(), defaultSessionCfg("s1"), out)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := wp.Submit(ctx, makeJob("s1"))
	if err == nil {
		t.Fatal("want error from cancelled ctx")
	}
	if wp.Stats().Dropped == 0 {
		t.Fatal("dropped should be > 0")
	}
}

func TestSubmit_CountsSubmitted(t *testing.T) {
	wp := NewWorkerPool(WorkerPoolConfig{Workers: 0, QueueSize: 8})
	out := make(chan AudioChunk, 16)
	_ = wp.RegisterSession(context.Background(), defaultSessionCfg("s1"), out)

	for i := 0; i < 5; i++ {
		_ = wp.Submit(context.Background(), makeJob("s1"))
	}
	if wp.Stats().Submitted != 5 {
		t.Fatalf("want 5, got %d", wp.Stats().Submitted)
	}
}

// ---------- Processing pipeline ----------

func TestProcess_PCMU20ms_ProducesChunk(t *testing.T) {
	wp := NewWorkerPool(WorkerPoolConfig{Workers: 4, QueueSize: 128})
	out := make(chan AudioChunk, 16)
	_ = wp.RegisterSession(context.Background(), defaultSessionCfg("s1"), out)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go wp.Run(ctx)

	if err := wp.Submit(context.Background(), makeJob("s1")); err != nil {
		t.Fatalf("submit: %v", err)
	}

	select {
	case chunk := <-out:
		if chunk.SessionID != "s1" {
			t.Errorf("wrong session: %s", chunk.SessionID)
		}
		if chunk.SampleRate != 16000 {
			t.Errorf("want SampleRate=16000, got %d", chunk.SampleRate)
		}
		if chunk.DurationMs != 20 {
			t.Errorf("want DurationMs=20, got %d", chunk.DurationMs)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout: no chunk produced")
	}
}

func TestProcess_MultiplePackets_RTPTimestampPropagated(t *testing.T) {
	wp := NewWorkerPool(WorkerPoolConfig{Workers: 1, QueueSize: 128})
	out := make(chan AudioChunk, 32)
	_ = wp.RegisterSession(context.Background(), defaultSessionCfg("s1"), out)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go wp.Run(ctx)

	// Gửi 5 packet với RTP timestamp tăng dần (160 samples/packet @ 8kHz).
	const n = 5
	for i := 0; i < n; i++ {
		job := AudioJob{
			SessionID: "s1",
			Packet: MediaPacket{
				SessionID:  "s1",
				Codec:      "PCMU",
				SampleRate: 8000,
				Channels:   1,
				Timestamp:  uint32(i * 160),
				Payload:    pcmuPayload(160),
			},
		}
		if err := wp.Submit(context.Background(), job); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}

	chunks := make([]AudioChunk, 0, n)
	deadline := time.Now().Add(time.Second)
	for len(chunks) < n && time.Now().Before(deadline) {
		select {
		case c := <-out:
			chunks = append(chunks, c)
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
	if len(chunks) < n {
		t.Fatalf("want %d chunks, got %d", n, len(chunks))
	}
	// Chunk đầu tiên phải có RTPTimestamp = 0 (packet đầu tiên).
	if chunks[0].RTPTimestamp != 0 {
		t.Errorf("chunk[0].RTPTimestamp = %d, want 0", chunks[0].RTPTimestamp)
	}
}

func TestProcess_DecodeError_Counted(t *testing.T) {
	wp := NewWorkerPool(WorkerPoolConfig{Workers: 2, QueueSize: 16})
	out := make(chan AudioChunk, 16)
	cfg := defaultSessionCfg("s1")
	cfg.Codec = "opus"
	cfg.SampleRate = 48000
	cfg.OutSampleRate = 16000
	_ = wp.RegisterSession(context.Background(), cfg, out)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go wp.Run(ctx)

	for i := 0; i < 3; i++ {
		job := AudioJob{
			SessionID: "s1",
			// {0x01, 0x02}: code-1 Opus packet with odd-length payload — pion/opus rejects this.
			Packet: MediaPacket{SessionID: "s1", Codec: "opus", Payload: []byte{0x01, 0x02}},
		}
		if err := wp.Submit(context.Background(), job); err != nil {
			t.Fatalf("submit: %v", err)
		}
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for wp.Stats().DecodeErrors+wp.Stats().Processed < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	stats := wp.Stats()
	if stats.DecodeErrors != 3 {
		t.Errorf("want DecodeErrors=3, got %d", stats.DecodeErrors)
	}
	if stats.Processed != 0 {
		t.Errorf("want Processed=0, got %d", stats.Processed)
	}
}

func TestProcess_UnknownSession_DropSilently(t *testing.T) {
	wp := NewWorkerPool(WorkerPoolConfig{Workers: 2, QueueSize: 16})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go wp.Run(ctx)

	err := wp.Submit(context.Background(), makeJob("ghost"))
	if err != nil {
		t.Fatalf("submit should succeed: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if wp.Stats().Processed != 0 {
		t.Errorf("processed should be 0 for unknown session, got %d", wp.Stats().Processed)
	}
}

// ---------- Run lifecycle ----------

func TestRun_ExitsOnCtxCancel(t *testing.T) {
	wp := NewWorkerPool(WorkerPoolConfig{Workers: 4, QueueSize: 16})
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		wp.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not exit after ctx cancel")
	}
}

// ---------- Stats ----------

func TestStats_Accurate(t *testing.T) {
	wp := NewWorkerPool(WorkerPoolConfig{Workers: 4, QueueSize: 64})
	out := make(chan AudioChunk, 32)
	_ = wp.RegisterSession(context.Background(), defaultSessionCfg("s1"), out)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go wp.Run(ctx)

	const n = 10
	for i := 0; i < n; i++ {
		_ = wp.Submit(context.Background(), makeJob("s1"))
	}

	deadline := time.Now().Add(time.Second)
	for wp.Stats().Processed < n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	stats := wp.Stats()
	if stats.Submitted != n {
		t.Errorf("want Submitted=%d, got %d", n, stats.Submitted)
	}
	if stats.Processed != n {
		t.Errorf("want Processed=%d, got %d", n, stats.Processed)
	}
	if stats.Dropped != 0 {
		t.Errorf("want Dropped=0, got %d", stats.Dropped)
	}
}

// ---------- SessionCtx cancellation ----------

func TestProcess_SessionCtxCancelled_StopsEmit(t *testing.T) {
	wp := NewWorkerPool(WorkerPoolConfig{Workers: 2, QueueSize: 64})
	out := make(chan AudioChunk, 4)
	sessCtx, sessCancel := context.WithCancel(context.Background())
	_ = wp.RegisterSession(sessCtx, defaultSessionCfg("s1"), out)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go wp.Run(ctx)

	for i := 0; i < 4; i++ {
		out <- AudioChunk{}
	}

	sessCancel()
	_ = wp.Submit(context.Background(), makeJob("s1"))
	time.Sleep(50 * time.Millisecond)
}

// ---------- Concurrency ----------

func TestConcurrency_MultipleSessions(t *testing.T) {
	const sessions = 8
	const jobsPerSession = 20

	wp := NewWorkerPool(WorkerPoolConfig{Workers: 8, QueueSize: 1024})
	outs := make([]chan AudioChunk, sessions)

	for i := range outs {
		outs[i] = make(chan AudioChunk, 64)
		id := fmt.Sprintf("sess-%d", i)
		_ = wp.RegisterSession(context.Background(), defaultSessionCfg(id), outs[i])
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go wp.Run(ctx)

	var wg sync.WaitGroup
	for i := 0; i < sessions; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("sess-%d", i)
			for j := 0; j < jobsPerSession; j++ {
				for {
					err := wp.Submit(context.Background(), makeJob(id))
					if err == nil {
						break
					}
					time.Sleep(time.Millisecond)
				}
			}
		}(i)
	}
	wg.Wait()

	deadline := time.Now().Add(2 * time.Second)
	for wp.Stats().Processed+wp.Stats().DecodeErrors < sessions*jobsPerSession &&
		time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	stats := wp.Stats()
	if stats.Processed != sessions*jobsPerSession {
		t.Errorf("want Processed=%d, got %d", sessions*jobsPerSession, stats.Processed)
	}
}
