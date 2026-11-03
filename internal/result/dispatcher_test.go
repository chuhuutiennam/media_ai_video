package result

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"media-ai-gateway/internal/pipeline"
)

// newH2CServer wraps handler với H2C support, cho phép HTTPCallbackSink (HTTP/2) kết nối.
func newH2CServer(handler http.Handler) *httptest.Server {
	return httptest.NewServer(h2c.NewHandler(handler, &http2.Server{}))
}

// --- captureSink ---

type captureSink struct {
	mu  sync.Mutex
	got []pipeline.RecognitionResult
	ch  chan pipeline.RecognitionResult
	err error // nếu khác nil, Send trả về lỗi này
}

func newCaptureSink(bufSize int) *captureSink {
	return &captureSink{ch: make(chan pipeline.RecognitionResult, bufSize)}
}

func (s *captureSink) Send(_ context.Context, r pipeline.RecognitionResult) error {
	if s.err != nil {
		return s.err
	}
	s.mu.Lock()
	s.got = append(s.got, r)
	s.mu.Unlock()
	s.ch <- r
	return nil
}

func (s *captureSink) Type() string { return "capture" }

func (s *captureSink) results() []pipeline.RecognitionResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]pipeline.RecognitionResult, len(s.got))
	copy(cp, s.got)
	return cp
}

// --- helpers ---

func smallCfg() Config {
	return Config{
		Workers:             2,
		QueueSize:           8,
		DropPartialWhenFull: true,
		SendTimeout:         200 * time.Millisecond,
	}
}

// waitStats polls until condition(Stats) is true or timeout.
func waitStats(t *testing.T, d *Dispatcher, cond func(Stats) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond(d.Stats()) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("timeout waiting for stats condition; current: %+v", d.Stats())
}

// --- Dispatcher tests ---

func TestDispatcher_Push_DeliveredToSink(t *testing.T) {
	d := NewDispatcher(smallCfg())
	sink := newCaptureSink(4)
	d.RegisterSink("s1", sink)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	r := pipeline.RecognitionResult{SessionID: "s1", Text: "hello", IsFinal: true}
	if err := d.Push(context.Background(), r); err != nil {
		t.Fatalf("Push: %v", err)
	}

	select {
	case got := <-sink.ch:
		if got.Text != "hello" || !got.IsFinal {
			t.Errorf("unexpected result: %+v", got)
		}
	case <-time.After(time.Second):
		t.Error("timeout: result not delivered to sink")
	}
}

func TestDispatcher_Push_DropPartial_WhenQueueFull(t *testing.T) {
	cfg := Config{QueueSize: 1, DropPartialWhenFull: true, SendTimeout: time.Second}
	d := NewDispatcher(cfg)
	// Fill queue without starting workers.
	d.queue <- pipeline.RecognitionResult{Text: "filler"}

	r := pipeline.RecognitionResult{SessionID: "s1", Text: "partial", IsFinal: false}
	if err := d.Push(context.Background(), r); err != nil {
		t.Fatalf("Push partial: %v", err)
	}
	st := d.Stats()
	if st.Dropped != 1 {
		t.Errorf("Dropped = %d, want 1", st.Dropped)
	}
	if st.Pushed != 1 {
		t.Errorf("Pushed = %d, want 1", st.Pushed)
	}
}

func TestDispatcher_Push_Final_NotDropped_WhenQueueFull(t *testing.T) {
	cfg := Config{QueueSize: 1, DropPartialWhenFull: true, SendTimeout: time.Second}
	d := NewDispatcher(cfg)
	// Fill queue (no workers).
	d.queue <- pipeline.RecognitionResult{Text: "filler"}

	enqueued := make(chan struct{})
	go func() {
		d.Push(context.Background(), pipeline.RecognitionResult{SessionID: "s1", IsFinal: true})
		close(enqueued)
	}()

	// Give Push time to block.
	time.Sleep(20 * time.Millisecond)
	// Drain filler to create space.
	<-d.queue

	select {
	case <-enqueued:
		// Final was enqueued successfully.
	case <-time.After(time.Second):
		t.Error("timeout: final result not enqueued when space became available")
	}
	if d.Stats().Dropped != 0 {
		t.Errorf("Dropped = %d, want 0 (final must not be dropped)", d.Stats().Dropped)
	}
}

func TestDispatcher_Push_CtxCancelled_Returns_Error(t *testing.T) {
	cfg := Config{QueueSize: 1, DropPartialWhenFull: false, SendTimeout: time.Second}
	d := NewDispatcher(cfg)
	// Fill queue (no workers).
	d.queue <- pipeline.RecognitionResult{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	err := d.Push(ctx, pipeline.RecognitionResult{SessionID: "s1", IsFinal: true})
	if err == nil {
		t.Error("Push with cancelled ctx: expected error")
	}
	if d.Stats().Dropped != 1 {
		t.Errorf("Dropped = %d, want 1", d.Stats().Dropped)
	}
}

func TestDispatcher_RegisterSink_Delivers(t *testing.T) {
	d := NewDispatcher(smallCfg())
	sink := newCaptureSink(4)
	d.RegisterSink("sess-x", sink)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	d.Push(context.Background(), pipeline.RecognitionResult{SessionID: "sess-x", Text: "delivered"})

	select {
	case got := <-sink.ch:
		if got.Text != "delivered" {
			t.Errorf("got %q, want 'delivered'", got.Text)
		}
	case <-time.After(time.Second):
		t.Error("timeout: sink not triggered")
	}
}

func TestDispatcher_UnregisterSinks_StopsDelivery(t *testing.T) {
	d := NewDispatcher(smallCfg())
	sink := newCaptureSink(4)
	d.RegisterSink("s1", sink)
	d.UnregisterSinks("s1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	d.Push(context.Background(), pipeline.RecognitionResult{SessionID: "s1", Text: "nope"})

	// Give dispatcher time to process.
	time.Sleep(60 * time.Millisecond)
	if n := len(sink.results()); n != 0 {
		t.Errorf("sink received %d results after unregister, want 0", n)
	}
}

func TestDispatcher_MultipleSinks_AllReceive(t *testing.T) {
	d := NewDispatcher(smallCfg())
	sink1 := newCaptureSink(4)
	sink2 := newCaptureSink(4)
	d.RegisterSink("s1", sink1)
	d.RegisterSink("s1", sink2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	d.Push(context.Background(), pipeline.RecognitionResult{SessionID: "s1", Text: "multi"})

	for i, sink := range []*captureSink{sink1, sink2} {
		select {
		case got := <-sink.ch:
			if got.Text != "multi" {
				t.Errorf("sink%d got %q, want 'multi'", i+1, got.Text)
			}
		case <-time.After(time.Second):
			t.Errorf("timeout: sink%d not triggered", i+1)
		}
	}
}

func TestDispatcher_NoSink_NoPanic(t *testing.T) {
	d := NewDispatcher(smallCfg())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	// No sink registered: worker should process and discard silently.
	if err := d.Push(context.Background(), pipeline.RecognitionResult{SessionID: "ghost"}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // let worker process
}

func TestDispatcher_Run_ExitsOnCtxCancel(t *testing.T) {
	d := NewDispatcher(smallCfg())
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("Run did not exit after ctx cancel")
	}
}

func TestDispatcher_Stats_Accurate(t *testing.T) {
	d := NewDispatcher(smallCfg())

	// errSink: always fails.
	errCapture := newCaptureSink(4)
	errCapture.err = fmt.Errorf("sink error")
	// okSink: always succeeds.
	okCapture := newCaptureSink(4)

	d.RegisterSink("s-err", errCapture)
	d.RegisterSink("s-ok", okCapture)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	d.Push(context.Background(), pipeline.RecognitionResult{SessionID: "s-err"}) // → error
	d.Push(context.Background(), pipeline.RecognitionResult{SessionID: "s-ok"})  // → ok

	// Wait until both are processed.
	waitStats(t, d, func(st Stats) bool { return st.Sent+st.SendErrors >= 2 })

	st := d.Stats()
	if st.Pushed != 2 {
		t.Errorf("Pushed = %d, want 2", st.Pushed)
	}
	if st.Sent != 1 {
		t.Errorf("Sent = %d, want 1", st.Sent)
	}
	if st.SendErrors != 1 {
		t.Errorf("SendErrors = %d, want 1", st.SendErrors)
	}
	if st.Dropped != 0 {
		t.Errorf("Dropped = %d, want 0", st.Dropped)
	}
}

func TestDispatcher_Stats_DropCounted(t *testing.T) {
	cfg := Config{QueueSize: 1, DropPartialWhenFull: true, SendTimeout: time.Second}
	d := NewDispatcher(cfg)
	d.queue <- pipeline.RecognitionResult{} // fill queue

	// Push 3 partial results — all should be dropped.
	for i := 0; i < 3; i++ {
		d.Push(context.Background(), pipeline.RecognitionResult{IsFinal: false})
	}
	st := d.Stats()
	if st.Dropped != 3 {
		t.Errorf("Dropped = %d, want 3", st.Dropped)
	}
	if st.Pushed != 3 {
		t.Errorf("Pushed = %d, want 3", st.Pushed)
	}
}

func TestDispatcher_QueueLen(t *testing.T) {
	cfg := Config{QueueSize: 8, Workers: 0, SendTimeout: time.Second}
	d := NewDispatcher(cfg)
	d.Push(context.Background(), pipeline.RecognitionResult{})
	d.Push(context.Background(), pipeline.RecognitionResult{})
	if d.QueueLen() != 2 {
		t.Errorf("QueueLen = %d, want 2", d.QueueLen())
	}
}

func TestDispatcher_WorkerPool_ConcurrentDelivery(t *testing.T) {
	cfg := Config{Workers: 4, QueueSize: 256, DropPartialWhenFull: true, SendTimeout: time.Second}
	d := NewDispatcher(cfg)

	var received atomic.Uint64
	sink := NewSinkFunc("counter", func(_ context.Context, r pipeline.RecognitionResult) error {
		received.Add(1)
		return nil
	})
	d.RegisterSink("s1", sink)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	const n = 100
	for i := 0; i < n; i++ {
		d.Push(context.Background(), pipeline.RecognitionResult{SessionID: "s1", IsFinal: true})
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if received.Load() == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("received %d results, want %d", received.Load(), n)
}

func TestDispatcher_SinkFunc(t *testing.T) {
	var got pipeline.RecognitionResult
	sink := NewSinkFunc("test_sink", func(_ context.Context, r pipeline.RecognitionResult) error {
		got = r
		return nil
	})
	if sink.Type() != "test_sink" {
		t.Errorf("Type = %q, want %q", sink.Type(), "test_sink")
	}
	r := pipeline.RecognitionResult{Text: "fn"}
	sink.Send(context.Background(), r)
	if got.Text != "fn" {
		t.Errorf("got %q, want 'fn'", got.Text)
	}
}

// --- HTTPCallbackSink tests ---

func TestHTTPCallbackSink_Success(t *testing.T) {
	var received pipeline.RecognitionResult
	srv := newH2CServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := NewHTTPCallbackSink(srv.URL, time.Second, 0)
	result := pipeline.RecognitionResult{SessionID: "s1", Text: "xin chào", IsFinal: true}

	if err := sink.Send(context.Background(), result); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if received.Text != "xin chào" {
		t.Errorf("received.Text = %q, want %q", received.Text, "xin chào")
	}
	if !received.IsFinal {
		t.Error("received.IsFinal = false, want true")
	}
}

func TestHTTPCallbackSink_ContentTypeJSON(t *testing.T) {
	var gotContentType string
	srv := newH2CServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := NewHTTPCallbackSink(srv.URL, time.Second, 0)
	sink.Send(context.Background(), pipeline.RecognitionResult{SessionID: "s1"})

	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want %q", gotContentType, "application/json")
	}
}

func TestHTTPCallbackSink_RetryOn5xx(t *testing.T) {
	var attempts atomic.Int32
	srv := newH2CServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	sink := NewHTTPCallbackSink(srv.URL, time.Second, 3)
	sink.RetryBackoff = time.Millisecond

	if err := sink.Send(context.Background(), pipeline.RecognitionResult{SessionID: "s1"}); err != nil {
		t.Fatalf("Send with retry: %v", err)
	}
	if attempts.Load() != 3 {
		t.Errorf("attempts = %d, want 3 (2 failures + 1 success)", attempts.Load())
	}
}

func TestHTTPCallbackSink_NoRetryOn4xx(t *testing.T) {
	var attempts atomic.Int32
	srv := newH2CServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	sink := NewHTTPCallbackSink(srv.URL, time.Second, 3)
	sink.RetryBackoff = time.Millisecond

	if err := sink.Send(context.Background(), pipeline.RecognitionResult{SessionID: "s1"}); err == nil {
		t.Error("expected error on 4xx")
	}
	if attempts.Load() != 1 {
		t.Errorf("attempts = %d, want 1 (no retry on 4xx)", attempts.Load())
	}
}

func TestHTTPCallbackSink_MaxRetryExceeded(t *testing.T) {
	var attempts atomic.Int32
	srv := newH2CServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	sink := NewHTTPCallbackSink(srv.URL, time.Second, 2)
	sink.RetryBackoff = time.Millisecond

	if err := sink.Send(context.Background(), pipeline.RecognitionResult{SessionID: "s1"}); err == nil {
		t.Error("expected error after max retry exceeded")
	}
	// MaxRetry=2: attempts 0, 1, 2 → total 3.
	if attempts.Load() != 3 {
		t.Errorf("attempts = %d, want 3", attempts.Load())
	}
}

func TestHTTPCallbackSink_CtxCancel_StopsRetry(t *testing.T) {
	var attempts atomic.Int32
	srv := newH2CServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	sink := NewHTTPCallbackSink(srv.URL, time.Second, 10)
	sink.RetryBackoff = 100 * time.Millisecond

	// Context times out after 150ms → should cancel mid-retry.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	if err := sink.Send(ctx, pipeline.RecognitionResult{SessionID: "s1"}); err == nil {
		t.Error("expected error when ctx cancelled during retry")
	}
	// With backoff=100ms and timeout=150ms, we expect ≤2 attempts.
	if n := attempts.Load(); n > 2 {
		t.Errorf("attempts = %d, expected ≤2 before ctx expired", n)
	}
}

func TestHTTPCallbackSink_UsesHTTP2(t *testing.T) {
	var gotProto string
	srv := newH2CServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotProto = r.Proto
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := NewHTTPCallbackSink(srv.URL, time.Second, 0)
	if err := sink.Send(context.Background(), pipeline.RecognitionResult{SessionID: "s1"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotProto != "HTTP/2.0" {
		t.Errorf("protocol = %q, want HTTP/2.0", gotProto)
	}
}

// --- event_type field tests ---

func TestHTTPCallbackSink_EventType_Final(t *testing.T) {
	var payload struct {
		EventType string `json:"event_type"`
		pipeline.RecognitionResult
	}
	srv := newH2CServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&payload)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := NewHTTPCallbackSink(srv.URL, time.Second, 0)
	sink.Send(context.Background(), pipeline.RecognitionResult{SessionID: "s1", IsFinal: true})

	if payload.EventType != "asr.transcript.final" {
		t.Errorf("event_type = %q, want %q", payload.EventType, "asr.transcript.final")
	}
	if payload.SessionID != "s1" {
		t.Errorf("session_id = %q, want %q", payload.SessionID, "s1")
	}
}

func TestHTTPCallbackSink_EventType_Partial(t *testing.T) {
	var payload struct {
		EventType string `json:"event_type"`
	}
	srv := newH2CServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&payload)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := NewHTTPCallbackSink(srv.URL, time.Second, 0)
	sink.Send(context.Background(), pipeline.RecognitionResult{SessionID: "s1", IsFinal: false})

	if payload.EventType != "asr.transcript.partial" {
		t.Errorf("event_type = %q, want %q", payload.EventType, "asr.transcript.partial")
	}
}

// --- Dead-letter tests ---

func TestHTTPCallbackSink_DeadLetter_CalledWhenRetriesExhausted(t *testing.T) {
	srv := newH2CServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	var dlGot pipeline.RecognitionResult
	dl := NewSinkFunc("dead_letter", func(_ context.Context, r pipeline.RecognitionResult) error {
		dlGot = r
		return nil
	})

	sink := NewHTTPCallbackSink(srv.URL, time.Second, 1) // 1 retry → 2 total attempts
	sink.RetryBackoff = time.Millisecond
	sink.DeadLetter = dl

	result := pipeline.RecognitionResult{SessionID: "s1", Text: "important", IsFinal: true}
	if err := sink.Send(context.Background(), result); err != nil {
		t.Fatalf("Send with dead-letter: %v", err)
	}
	if dlGot.SessionID != "s1" || dlGot.Text != "important" {
		t.Errorf("dead-letter got %+v, want session s1 text 'important'", dlGot)
	}
}

func TestHTTPCallbackSink_DeadLetter_NotCalledOnSuccess(t *testing.T) {
	srv := newH2CServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dlCalled := false
	dl := NewSinkFunc("dead_letter", func(_ context.Context, _ pipeline.RecognitionResult) error {
		dlCalled = true
		return nil
	})

	sink := NewHTTPCallbackSink(srv.URL, time.Second, 0)
	sink.DeadLetter = dl
	sink.Send(context.Background(), pipeline.RecognitionResult{SessionID: "s1"})

	if dlCalled {
		t.Error("dead-letter should not be called on successful delivery")
	}
}

func TestHTTPCallbackSink_DeadLetter_NilSafe(t *testing.T) {
	srv := newH2CServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	sink := NewHTTPCallbackSink(srv.URL, time.Second, 0)
	sink.RetryBackoff = time.Millisecond
	// DeadLetter is nil — should return error, not panic.
	err := sink.Send(context.Background(), pipeline.RecognitionResult{SessionID: "s1"})
	if err == nil {
		t.Error("expected error when retries exhausted and DeadLetter is nil")
	}
}

func TestHTTPCallbackSink_Type(t *testing.T) {
	sink := NewHTTPCallbackSink("http://x", time.Second, 0)
	if sink.Type() != "http_callback" {
		t.Errorf("Type = %q, want %q", sink.Type(), "http_callback")
	}
}

// --- Concurrency ---

func TestDispatcher_Concurrency(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDispatcher(cfg)

	sink := newCaptureSink(512)
	d.RegisterSink("shared", sink)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d.Push(context.Background(), pipeline.RecognitionResult{
				SessionID: "shared",
				Text:      fmt.Sprintf("result-%d", i),
				IsFinal:   i%2 == 0,
			})
		}(i)
	}
	wg.Wait()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st := d.Stats()
		if st.Sent+st.Dropped == n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	st := d.Stats()
	t.Errorf("after 2s: Sent=%d Dropped=%d total=%d, want %d", st.Sent, st.Dropped, st.Sent+st.Dropped, n)
}
