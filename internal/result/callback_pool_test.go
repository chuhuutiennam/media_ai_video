package result

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"media-ai-gateway/internal/pipeline"
)

func smallPoolCfg() CallbackPoolConfig {
	return CallbackPoolConfig{Workers: 2, QueueSize: 8, SendTimeout: time.Second}
}

func TestCallbackPool_Type(t *testing.T) {
	p := NewCallbackPool(smallPoolCfg(), NewSinkFunc("noop", func(_ context.Context, _ pipeline.RecognitionResult) error { return nil }))
	if p.Type() != "callback_pool" {
		t.Errorf("Type = %q, want %q", p.Type(), "callback_pool")
	}
}

func TestCallbackPool_Send_DeliveredToSink(t *testing.T) {
	delivered := make(chan pipeline.RecognitionResult, 1)
	sink := NewSinkFunc("capture", func(_ context.Context, r pipeline.RecognitionResult) error {
		delivered <- r
		return nil
	})

	p := NewCallbackPool(smallPoolCfg(), sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	r := pipeline.RecognitionResult{SessionID: "s1", Text: "hello"}
	if err := p.Send(context.Background(), r); err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case got := <-delivered:
		if got.Text != "hello" {
			t.Errorf("got %q, want 'hello'", got.Text)
		}
	case <-time.After(time.Second):
		t.Error("timeout: result not delivered to sink")
	}
}

func TestCallbackPool_Send_NonBlocking(t *testing.T) {
	// Sink blocks indefinitely — Send must still return immediately.
	block := make(chan struct{})
	sink := NewSinkFunc("slow", func(ctx context.Context, _ pipeline.RecognitionResult) error {
		<-block
		return nil
	})
	p := NewCallbackPool(CallbackPoolConfig{Workers: 1, QueueSize: 4, SendTimeout: time.Second}, sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	start := time.Now()
	for i := 0; i < 4; i++ {
		p.Send(context.Background(), pipeline.RecognitionResult{SessionID: "s1"})
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("Send blocked for %v, want <50ms", elapsed)
	}
	close(block)
}

func TestCallbackPool_Send_DropWhenFull(t *testing.T) {
	block := make(chan struct{})
	sink := NewSinkFunc("block", func(ctx context.Context, _ pipeline.RecognitionResult) error {
		<-block
		return nil
	})
	cfg := CallbackPoolConfig{Workers: 1, QueueSize: 2, SendTimeout: time.Second}
	p := NewCallbackPool(cfg, sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	// Fill queue: 1 in-flight (worker picks up first), 2 in queue.
	time.Sleep(10 * time.Millisecond) // let worker start and block on first
	p.Send(context.Background(), pipeline.RecognitionResult{SessionID: "a"}) // queue slot 1
	p.Send(context.Background(), pipeline.RecognitionResult{SessionID: "b"}) // queue slot 2
	// Wait for worker to dequeue one, then fill to capacity again
	time.Sleep(10 * time.Millisecond)

	// This send should be dropped (queue full).
	for i := 0; i < 5; i++ {
		p.Send(context.Background(), pipeline.RecognitionResult{SessionID: "overflow"})
	}

	st := p.Stats()
	if st.Dropped == 0 {
		t.Error("expected at least one drop when queue full")
	}
	close(block)
}

func TestCallbackPool_Send_IgnoresCallerCtx(t *testing.T) {
	// Cancelled caller ctx must not prevent enqueue (pool has its own lifecycle).
	delivered := make(chan struct{}, 1)
	sink := NewSinkFunc("capture", func(_ context.Context, _ pipeline.RecognitionResult) error {
		delivered <- struct{}{}
		return nil
	})
	p := NewCallbackPool(smallPoolCfg(), sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	cancelledCtx, c := context.WithCancel(context.Background())
	c() // already cancelled

	if err := p.Send(cancelledCtx, pipeline.RecognitionResult{SessionID: "s1"}); err != nil {
		t.Fatalf("Send with cancelled caller ctx: %v", err)
	}
	select {
	case <-delivered:
	case <-time.After(time.Second):
		t.Error("timeout: result not delivered despite enqueue succeeding")
	}
}

func TestCallbackPool_Run_ExitsOnCtxCancel(t *testing.T) {
	sink := NewSinkFunc("noop", func(_ context.Context, _ pipeline.RecognitionResult) error { return nil })
	p := NewCallbackPool(smallPoolCfg(), sink)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("Run did not exit after ctx cancel")
	}
}

func TestCallbackPool_Stats_Pushed_Sent(t *testing.T) {
	var count atomic.Uint64
	sink := NewSinkFunc("counter", func(_ context.Context, _ pipeline.RecognitionResult) error {
		count.Add(1)
		return nil
	})
	const n = 10
	// QueueSize must be >= n so no drops occur before workers drain the queue.
	p := NewCallbackPool(CallbackPoolConfig{Workers: 2, QueueSize: n + 4, SendTimeout: time.Second}, sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	for i := 0; i < n; i++ {
		p.Send(context.Background(), pipeline.RecognitionResult{SessionID: "s1"})
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.Stats().Sent == n {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	st := p.Stats()
	if st.Pushed != n {
		t.Errorf("Pushed = %d, want %d", st.Pushed, n)
	}
	if st.Sent != n {
		t.Errorf("Sent = %d, want %d", st.Sent, n)
	}
	if st.Dropped != 0 {
		t.Errorf("Dropped = %d, want 0", st.Dropped)
	}
	if st.SendErrors != 0 {
		t.Errorf("SendErrors = %d, want 0", st.SendErrors)
	}
}

func TestCallbackPool_Stats_SendErrors(t *testing.T) {
	sinkErr := errors.New("sink failure")
	sink := NewSinkFunc("fail", func(_ context.Context, _ pipeline.RecognitionResult) error {
		return sinkErr
	})
	p := NewCallbackPool(smallPoolCfg(), sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Send(context.Background(), pipeline.RecognitionResult{SessionID: "s1"})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		st := p.Stats()
		if st.Sent+st.SendErrors >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	st := p.Stats()
	if st.SendErrors != 1 {
		t.Errorf("SendErrors = %d, want 1", st.SendErrors)
	}
	if st.Sent != 0 {
		t.Errorf("Sent = %d, want 0", st.Sent)
	}
}

func TestCallbackPool_SendTimeout_Applied(t *testing.T) {
	// Sink blocks longer than SendTimeout → error counted.
	sink := NewSinkFunc("slow", func(ctx context.Context, _ pipeline.RecognitionResult) error {
		<-ctx.Done()
		return ctx.Err()
	})
	cfg := CallbackPoolConfig{Workers: 1, QueueSize: 4, SendTimeout: 30 * time.Millisecond}
	p := NewCallbackPool(cfg, sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Send(context.Background(), pipeline.RecognitionResult{SessionID: "s1"})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if p.Stats().SendErrors >= 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("SendErrors = %d, want ≥1 after timeout", p.Stats().SendErrors)
}

func TestCallbackPool_QueueLen_QueueCap(t *testing.T) {
	sink := NewSinkFunc("noop", func(_ context.Context, _ pipeline.RecognitionResult) error { return nil })
	cfg := CallbackPoolConfig{Workers: 0, QueueSize: 16, SendTimeout: time.Second} // Workers=0: no drain
	p := NewCallbackPool(cfg, sink)

	if p.QueueCap() != 16 {
		t.Errorf("QueueCap = %d, want 16", p.QueueCap())
	}
	p.Send(context.Background(), pipeline.RecognitionResult{})
	p.Send(context.Background(), pipeline.RecognitionResult{})
	if p.QueueLen() != 2 {
		t.Errorf("QueueLen = %d, want 2", p.QueueLen())
	}
}

func TestCallbackPool_ImplementsSink(t *testing.T) {
	// Compile-time check that CallbackPool implements Sink.
	var _ Sink = (*CallbackPool)(nil)
}
