package ai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"media-ai-gateway/internal/pipeline"
)

// --- fakeClient ---
//
// ctx được truyền từ Dialer.Dial. Khi ctx bị cancel:
//   - Recv() trả về io.EOF (giống gRPC real).
//   - Send() trả về ctx.Err().

type fakeClient struct {
	ctx      context.Context
	sendCh   chan pipeline.AudioChunk
	recvCh   chan pipeline.RecognitionResult
	sendErr  error // nếu khác nil, Send trả về lỗi này thay vì gửi
	recvDone chan struct{}
	recvOnce sync.Once
}

func newFakeClient(ctx context.Context) *fakeClient {
	return &fakeClient{
		ctx:      ctx,
		sendCh:   make(chan pipeline.AudioChunk, 16),
		recvCh:   make(chan pipeline.RecognitionResult, 16),
		recvDone: make(chan struct{}),
	}
}

func (f *fakeClient) Send(chunk pipeline.AudioChunk) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	select {
	case f.sendCh <- chunk:
		return nil
	case <-f.ctx.Done():
		return f.ctx.Err()
	}
}

func (f *fakeClient) Recv() (pipeline.RecognitionResult, error) {
	select {
	case r, ok := <-f.recvCh:
		if !ok {
			return pipeline.RecognitionResult{}, io.EOF
		}
		return r, nil
	case <-f.ctx.Done():
		return pipeline.RecognitionResult{}, io.EOF
	case <-f.recvDone:
		return pipeline.RecognitionResult{}, io.EOF
	}
}

// CloseSend unblocks any pending Recv() call, mirroring real gRPC half-close behaviour.
func (f *fakeClient) CloseSend() error {
	f.recvOnce.Do(func() {
		if f.recvDone != nil {
			close(f.recvDone)
		}
	})
	return nil
}

// --- fakeDialer ---

type fakeDialer struct {
	mu      sync.Mutex
	clients map[string]*fakeClient
	dialErr error
}

func (d *fakeDialer) Dial(ctx context.Context, sessionID, streamID, language, task string) (StreamClient, error) {
	if d.dialErr != nil {
		return nil, d.dialErr
	}
	c := newFakeClient(ctx)
	d.mu.Lock()
	if d.clients == nil {
		d.clients = make(map[string]*fakeClient)
	}
	d.clients[sessionID] = c
	d.mu.Unlock()
	return c, nil
}

func (d *fakeDialer) client(sessionID string) *fakeClient {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.clients[sessionID]
}

// --- helpers ---

func testCfg() Config {
	return Config{
		MaxActiveStreams: 5,
		SendTimeout:     100 * time.Millisecond,
		StreamTimeout:   30 * time.Second,
	}
}

// openStream opens a stream and registers cleanup via t.Cleanup.
func openStream(t *testing.T, m *Manager, d *fakeDialer, sessionID string) (
	audioIn chan pipeline.AudioChunk,
	resultOut chan pipeline.RecognitionResult,
	s *Stream,
) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	audioIn = make(chan pipeline.AudioChunk, 8)
	resultOut = make(chan pipeline.RecognitionResult, 8)
	var err error
	s, err = m.Open(ctx, sessionID, sessionID, "vi", "transcribe", audioIn, resultOut)
	if err != nil {
		t.Fatalf("Open %s: %v", sessionID, err)
	}
	return
}

// waitForCount polls until m.Count() == want or 1s elapses.
func waitForCount(t *testing.T, m *Manager, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if m.Count() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("timeout: Count = %d, want %d", m.Count(), want)
}

// --- Manager tests ---

func TestManager_Open_Count(t *testing.T) {
	m := NewManager(testCfg(), &fakeDialer{})
	openStream(t, m, nil, "s1")
	if m.Count() != 1 {
		t.Errorf("Count = %d, want 1", m.Count())
	}
}

func TestManager_Open_Fields(t *testing.T) {
	m := NewManager(testCfg(), &fakeDialer{})
	_, _, s := openStream(t, m, nil, "sess-abc")
	if s.SessionID != "sess-abc" {
		t.Errorf("SessionID = %q, want %q", s.SessionID, "sess-abc")
	}
	if s.StreamID != "sess-abc" {
		t.Errorf("StreamID = %q, want %q", s.StreamID, "sess-abc")
	}
	if s.OpenedAt.IsZero() {
		t.Error("OpenedAt is zero")
	}
}

func TestManager_Open_MaxStreams(t *testing.T) {
	m := NewManager(testCfg(), &fakeDialer{}) // max 5
	for i := 0; i < 5; i++ {
		openStream(t, m, nil, fmt.Sprintf("s%d", i))
	}
	ctx := context.Background()
	_, err := m.Open(ctx, "overflow", "overflow", "vi", "transcribe",
		make(chan pipeline.AudioChunk), make(chan pipeline.RecognitionResult))
	if !errors.Is(err, ErrMaxStreams) {
		t.Errorf("got %v, want ErrMaxStreams", err)
	}
}

func TestManager_Open_DuplicateSession(t *testing.T) {
	m := NewManager(testCfg(), &fakeDialer{})
	audioIn, resultOut, _ := openStream(t, m, nil, "s1")
	ctx := context.Background()
	_, err := m.Open(ctx, "s1", "s1", "vi", "transcribe", audioIn, resultOut)
	if !errors.Is(err, ErrDuplicateStream) {
		t.Errorf("got %v, want ErrDuplicateStream", err)
	}
}

func TestManager_Open_DialError(t *testing.T) {
	dialErr := errors.New("connection refused")
	m := NewManager(testCfg(), &fakeDialer{dialErr: dialErr})
	ctx := context.Background()
	_, err := m.Open(ctx, "s1", "s1", "vi", "transcribe",
		make(chan pipeline.AudioChunk), make(chan pipeline.RecognitionResult))
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, dialErr) {
		t.Errorf("got %v, want to wrap %v", err, dialErr)
	}
	if m.Count() != 0 {
		t.Errorf("Count = %d after dial error, want 0", m.Count())
	}
}

func TestManager_Close_ReturnsTrue(t *testing.T) {
	m := NewManager(testCfg(), &fakeDialer{})
	_, _, s := openStream(t, m, nil, "s1")
	if !m.Close("s1") {
		t.Error("Close: expected true")
	}
	if m.Count() != 0 {
		t.Errorf("Count = %d after Close, want 0", m.Count())
	}
	// goroutines must exit cleanly
	done := make(chan struct{})
	go func() { s.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("goroutines did not exit after Close")
	}
}

func TestManager_Close_ReturnsFalse_WhenNotFound(t *testing.T) {
	m := NewManager(testCfg(), &fakeDialer{})
	if m.Close("ghost") {
		t.Error("Close non-existent: expected false")
	}
}

func TestManager_Close_Idempotent(t *testing.T) {
	m := NewManager(testCfg(), &fakeDialer{})
	openStream(t, m, nil, "s1")
	m.Close("s1")
	if m.Close("s1") {
		t.Error("second Close: expected false")
	}
}

// --- Data-flow tests ---

func TestManager_Send_ChunkReachesClient(t *testing.T) {
	d := &fakeDialer{}
	m := NewManager(testCfg(), d)
	audioIn, _, _ := openStream(t, m, d, "s1")

	chunk := pipeline.AudioChunk{SessionID: "s1", PCM: []byte{1, 2, 3}, DurationMs: 20}
	audioIn <- chunk

	fc := d.client("s1")
	select {
	case got := <-fc.sendCh:
		if got.SessionID != "s1" || got.DurationMs != 20 {
			t.Errorf("unexpected chunk: %+v", got)
		}
	case <-time.After(time.Second):
		t.Error("timeout: chunk did not reach client.Send")
	}
}

func TestManager_Recv_ResultReachesResultOut(t *testing.T) {
	d := &fakeDialer{}
	m := NewManager(testCfg(), d)
	_, resultOut, _ := openStream(t, m, d, "s1")

	// Wait for Dial to complete and client to be stored.
	var fc *fakeClient
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if fc = d.client("s1"); fc != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if fc == nil {
		t.Fatal("fakeClient never created")
	}

	result := pipeline.RecognitionResult{SessionID: "s1", Text: "xin chào", IsFinal: true}
	fc.recvCh <- result

	select {
	case got := <-resultOut:
		if got.Text != "xin chào" || !got.IsFinal {
			t.Errorf("unexpected result: %+v", got)
		}
	case <-time.After(time.Second):
		t.Error("timeout: result did not reach resultOut")
	}
}

func TestManager_ParentCtxCancel_StopsGoroutines(t *testing.T) {
	m := NewManager(testCfg(), &fakeDialer{})
	ctx, cancel := context.WithCancel(context.Background())
	audioIn := make(chan pipeline.AudioChunk, 4)
	resultOut := make(chan pipeline.RecognitionResult, 4)
	s, err := m.Open(ctx, "s1", "s1", "vi", "transcribe", audioIn, resultOut)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	cancel()

	done := make(chan struct{})
	go func() { s.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("goroutines did not exit after parent ctx cancel")
	}
}

// TestManager_AutoCleanup verifies that Count() drops after goroutines exit
// without an explicit Close() call (e.g. parent context cancelled externally).
func TestManager_AutoCleanup_CountDecreases(t *testing.T) {
	m := NewManager(testCfg(), &fakeDialer{})
	ctx, cancel := context.WithCancel(context.Background())
	audioIn := make(chan pipeline.AudioChunk, 4)
	resultOut := make(chan pipeline.RecognitionResult, 4)
	s, _ := m.Open(ctx, "s1", "s1", "vi", "transcribe", audioIn, resultOut)

	cancel()
	s.Wait() // goroutines done → cleanup goroutine will remove from map

	waitForCount(t, m, 0)
}

// --- Recv backpressure policy ---

func TestStream_Recv_DropPartial_WhenFull(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fc := newFakeClient(ctx)
	resultOut := make(chan pipeline.RecognitionResult, 1)

	s := &Stream{SessionID: "s1"}
	go s.runRecv(ctx, fc, resultOut)

	// Fill resultOut with a placeholder.
	resultOut <- pipeline.RecognitionResult{Text: "placeholder"}

	// Send partial — should be dropped (resultOut full, IsFinal=false).
	fc.recvCh <- pipeline.RecognitionResult{Text: "dropped", IsFinal: false}

	// Give recv goroutine time to see and drop the partial.
	time.Sleep(20 * time.Millisecond)

	// Drain placeholder, making room.
	<-resultOut

	// Send final — must be delivered even after the drop.
	fc.recvCh <- pipeline.RecognitionResult{Text: "final", IsFinal: true}

	select {
	case got := <-resultOut:
		if got.Text != "final" {
			t.Errorf("got %q, want 'final'", got.Text)
		}
	case <-time.After(time.Second):
		t.Error("timeout: final result not delivered")
	}
}

func TestStream_Recv_AllResultsDelivered_WhenCapacitySufficient(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fc := newFakeClient(ctx)
	resultOut := make(chan pipeline.RecognitionResult, 8)

	s := &Stream{SessionID: "s1"}
	go s.runRecv(ctx, fc, resultOut)

	texts := []string{"a", "b", "c", "d"}
	for i, txt := range texts {
		fc.recvCh <- pipeline.RecognitionResult{Text: txt, IsFinal: i%2 == 1}
	}

	got := make(map[string]bool)
	for range texts {
		select {
		case r := <-resultOut:
			got[r.Text] = true
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for result")
		}
	}
	for _, txt := range texts {
		if !got[txt] {
			t.Errorf("result %q not delivered", txt)
		}
	}
}

func TestStream_Recv_UnexpectedEOF(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fc := newFakeClient(ctx)
	close(fc.recvCh) // EOF immediately, end_of_stream never sent

	s := &Stream{SessionID: "s1"}

	done := make(chan struct{})
	go func() { s.runRecv(ctx, fc, make(chan pipeline.RecognitionResult, 4)); close(done) }()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("runRecv did not exit on EOF")
	}
	if s.Err() != ErrUnexpectedEOF {
		t.Errorf("Err = %v, want ErrUnexpectedEOF", s.Err())
	}
}

func TestStream_Recv_CleanEOF_AfterEndOfStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fc := newFakeClient(ctx)
	close(fc.recvCh) // EOF immediately

	s := &Stream{SessionID: "s1"}
	s.endOfStreamSent.Store(true) // gateway already sent end_of_stream

	done := make(chan struct{})
	go func() { s.runRecv(ctx, fc, make(chan pipeline.RecognitionResult, 4)); close(done) }()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("runRecv did not exit on clean EOF")
	}
	if s.Err() != nil {
		t.Errorf("Err = %v, want nil for clean EOF after end_of_stream", s.Err())
	}
}

func TestStream_Recv_IdleTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fc := newFakeClient(ctx) // recvCh never closed — simulates hung worker

	s := &Stream{
		SessionID: "s1",
		cfg:       Config{RecvIdleTimeout: 50 * time.Millisecond},
	}

	done := make(chan struct{})
	go func() { s.runRecv(ctx, fc, make(chan pipeline.RecognitionResult, 4)); close(done) }()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("runRecv did not exit on idle timeout")
	}
	if s.Err() != ErrRecvIdleTimeout {
		t.Errorf("Err = %v, want ErrRecvIdleTimeout", s.Err())
	}
}

func TestStream_Send_ExitsOnClosedAudioIn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fc := newFakeClient(ctx)
	audioIn := make(chan pipeline.AudioChunk)
	close(audioIn) // signal no more data

	s := &Stream{SessionID: "s1"}

	done := make(chan struct{})
	go func() { s.runSend(ctx, fc, audioIn); close(done) }()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("runSend did not exit on closed audioIn")
	}
}

func TestStream_Send_Error_SetsErr(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fc := newFakeClient(ctx)
	fc.sendErr = errors.New("network error")

	audioIn := make(chan pipeline.AudioChunk, 4)
	audioIn <- pipeline.AudioChunk{SessionID: "s1"}

	s := &Stream{SessionID: "s1"}

	done := make(chan struct{})
	go func() { s.runSend(ctx, fc, audioIn); close(done) }()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runSend did not exit on send error")
	}
	if s.Err() == nil {
		t.Error("Err should be set after send error")
	}
}

// --- Concurrency ---

func TestManager_Concurrency(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxActiveStreams = 200
	m := NewManager(cfg, &fakeDialer{})

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("sess-%d", i)
			ctx, cancel := context.WithCancel(context.Background())
			audioIn := make(chan pipeline.AudioChunk, 4)
			resultOut := make(chan pipeline.RecognitionResult, 4)
			m.Open(ctx, id, id, "vi", "transcribe", audioIn, resultOut)
			cancel()
			m.Close(id)
		}(i)
	}
	wg.Wait()
	// no panic = pass; Count may be non-zero briefly while cleanup goroutines run
}

// --- seqDialer: returns clients from a predefined sequence ---

type seqDialer struct {
	mu  sync.Mutex
	seq []func(context.Context) *fakeClient
	idx int
}

func (d *seqDialer) Dial(ctx context.Context, sessionID, streamID, language, task string) (StreamClient, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.idx >= len(d.seq) {
		return newFakeClient(ctx), nil
	}
	c := d.seq[d.idx](ctx)
	d.idx++
	return c, nil
}

// --- Reconnect tests ---

func TestStream_Reconnect_IncrementsRetries(t *testing.T) {
	// First client fails on send; second is normal.
	d := &seqDialer{
		seq: []func(context.Context) *fakeClient{
			func(ctx context.Context) *fakeClient {
				fc := newFakeClient(ctx)
				fc.sendErr = errors.New("transport fail")
				return fc
			},
			newFakeClient,
		},
	}

	cfg := testCfg()
	cfg.MaxRetries = 2
	cfg.RetryBackoff = time.Millisecond
	m := NewManager(cfg, d)

	audioIn := make(chan pipeline.AudioChunk, 4)
	resultOut := make(chan pipeline.RecognitionResult, 4)
	audioIn <- pipeline.AudioChunk{SessionID: "s1"} // triggers send error on first client

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, err := m.Open(ctx, "s1", "s1", "vi", "transcribe", audioIn, resultOut)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if s.Retries() > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("expected at least one reconnect attempt")
}

// --- SendTimeout test ---

func TestStream_SendTimeout_CausesExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Unbuffered sendCh: fc.Send blocks until ctx is done — simulates slow AI service.
	fc := &fakeClient{
		ctx:    ctx,
		sendCh: make(chan pipeline.AudioChunk),
		recvCh: make(chan pipeline.RecognitionResult, 4),
	}

	s := &Stream{
		SessionID: "s1",
		cfg:       Config{SendTimeout: 20 * time.Millisecond},
	}

	audioIn := make(chan pipeline.AudioChunk, 4)
	audioIn <- pipeline.AudioChunk{SessionID: "s1"}

	done := make(chan struct{})
	go func() { s.runSend(ctx, fc, audioIn); close(done) }()

	select {
	case <-done:
		// sendWithTimeout fired → runSend exited cleanly
	case <-time.After(500 * time.Millisecond):
		t.Error("runSend did not exit after SendTimeout")
	}
}

// --- Bounded queue test ---

func TestManager_BoundedQueue_NoDeadlock(t *testing.T) {
	d := &fakeDialer{}
	cfg := testCfg()
	cfg.QueueSize = 2

	m := NewManager(cfg, d)
	ctx, cancel := context.WithCancel(context.Background())

	audioIn := make(chan pipeline.AudioChunk, 20)
	resultOut := make(chan pipeline.RecognitionResult, 8)
	s, err := m.Open(ctx, "s1", "s1", "vi", "transcribe", audioIn, resultOut)
	if err != nil {
		cancel()
		t.Fatalf("Open: %v", err)
	}

	// Flood audioIn; bounded queue should drop excess without blocking.
	for i := 0; i < 20; i++ {
		audioIn <- pipeline.AudioChunk{SessionID: "s1"}
	}

	time.Sleep(50 * time.Millisecond)
	cancel()

	done := make(chan struct{})
	go func() { s.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("stream did not exit cleanly — possible deadlock in bounded queue")
	}
}
