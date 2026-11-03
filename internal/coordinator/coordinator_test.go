package coordinator

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"media-ai-gateway/internal/ai"
	"media-ai-gateway/internal/jitter"
	"media-ai-gateway/internal/pipeline"
	"media-ai-gateway/internal/result"
	"media-ai-gateway/internal/session"
)

// ---------- fakes ----------

type fakeClient struct {
	ctx     context.Context
	chunks  chan pipeline.AudioChunk
	results chan pipeline.RecognitionResult
}

func (c *fakeClient) Send(chunk pipeline.AudioChunk) error {
	select {
	case c.chunks <- chunk:
		return nil
	case <-c.ctx.Done():
		return c.ctx.Err()
	}
}

func (c *fakeClient) Recv() (pipeline.RecognitionResult, error) {
	select {
	case r := <-c.results:
		return r, nil
	case <-c.ctx.Done():
		return pipeline.RecognitionResult{}, io.EOF
	}
}

func (c *fakeClient) CloseSend() error { return nil }

type fakeDialer struct {
	mu      sync.Mutex
	clients map[string]*fakeClient
	dialErr error
}

func newFakeDialer() *fakeDialer {
	return &fakeDialer{clients: make(map[string]*fakeClient)}
}

func (d *fakeDialer) Dial(ctx context.Context, sessionID, _, _, _ string) (ai.StreamClient, error) {
	if d.dialErr != nil {
		return nil, d.dialErr
	}
	c := &fakeClient{
		ctx:     ctx,
		chunks:  make(chan pipeline.AudioChunk, 32),
		results: make(chan pipeline.RecognitionResult, 16),
	}
	d.mu.Lock()
	d.clients[sessionID] = c
	d.mu.Unlock()
	return c, nil
}

func (d *fakeDialer) client(sessionID string) *fakeClient {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.clients[sessionID]
}

// ---------- helpers ----------

// fastCfg: jitter buffer với flush 5ms + 20ms chunks để test chạy nhanh.
func fastCfg() Config {
	return Config{
		JitterConfig: jitter.Config{
			BufferMs:     0,
			MaxLateMs:    10_000,
			PacketTimeMs: 5,
		},
		OutSampleRate:   16000,
		OutChannels:     1,
		ChunkMs:         20, // 20ms @ 16kHz = 320 samples = 1 PCMU packet
		CallbackTimeout: time.Second,
		CallbackRetry:   0,
	}
}

func newStack(t *testing.T) (*Coordinator, *pipeline.WorkerPool, *ai.Manager, *result.Dispatcher, *fakeDialer) {
	t.Helper()
	pool := pipeline.NewWorkerPool(pipeline.WorkerPoolConfig{Workers: 4, QueueSize: 256})
	dialer := newFakeDialer()
	aiMgr := ai.NewManager(ai.Config{MaxActiveStreams: 100, StreamTimeout: 30 * time.Second}, dialer)
	disp := result.NewDispatcher(result.Config{Workers: 4, QueueSize: 256, DropPartialWhenFull: true, SendTimeout: time.Second})
	coord := New(fastCfg(), pool, aiMgr, disp)
	return coord, pool, aiMgr, disp, dialer
}

func runStack(ctx context.Context, pool *pipeline.WorkerPool, disp *result.Dispatcher) {
	go pool.Run(ctx)
	go disp.Run(ctx)
}

func newMgr() *session.Manager {
	return session.NewManager(session.ManagerConfig{
		MaxSessions:     100,
		PacketQueueSize: 64,
		AudioQueueSize:  64,
		ResultQueueSize: 32,
		IdleTimeout:     30 * time.Second,
		GCInterval:      time.Minute,
	})
}

func createSession(t *testing.T, mgr *session.Manager, id string) *session.Session {
	t.Helper()
	sess, err := mgr.Create(session.SessionConfig{
		ID: id, SourceType: "raw_rtp",
		Codec: "PCMU", SampleRate: 8000, Channels: 1,
		Language: "vi", Task: "transcribe",
	})
	if err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
	return sess
}

// pcmuPacket tạo 20ms PCMU MediaPacket.
func pcmuPacket(sessID string, seq uint16) pipeline.MediaPacket {
	payload := make([]byte, 160) // 20ms @ 8kHz
	for i := range payload {
		payload[i] = 0xFF
	}
	return pipeline.MediaPacket{
		SessionID:    sessID,
		Codec:        "PCMU",
		SampleRate:   8000,
		Channels:     1,
		Sequence:     seq,
		Payload:      payload,
		ReceivedAtMs: time.Now().UnixMilli(),
	}
}

// waitForClient polls đến khi fakeDialer có client cho sessionID.
func waitForClient(t *testing.T, d *fakeDialer, sessionID string) *fakeClient {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if c := d.client(sessionID); c != nil {
			return c
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no fake client for %s after 500ms", sessionID)
	return nil
}

// ---------- tests ----------

func TestStart_RegistersSessionWithPool(t *testing.T) {
	coord, pool, _, disp, _ := newStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runStack(ctx, pool, disp)

	mgr := newMgr()
	sess := createSession(t, mgr, "s1")

	if _, err := coord.Start(sess); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if pool.SessionCount() != 1 {
		t.Errorf("pool.SessionCount: want 1, got %d", pool.SessionCount())
	}
}

func TestStart_OpensAIStream(t *testing.T) {
	coord, pool, aiMgr, disp, _ := newStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runStack(ctx, pool, disp)

	mgr := newMgr()
	sess := createSession(t, mgr, "s1")

	if _, err := coord.Start(sess); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if aiMgr.Count() != 1 {
		t.Errorf("aiMgr.Count: want 1, got %d", aiMgr.Count())
	}
}

func TestStart_DuplicateSession_ReturnsError(t *testing.T) {
	coord, pool, _, disp, _ := newStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runStack(ctx, pool, disp)

	mgr := newMgr()
	sess := createSession(t, mgr, "s1")

	if _, err := coord.Start(sess); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if _, err := coord.Start(sess); err == nil {
		t.Fatal("second Start: want error, got nil")
	}
}

func TestStart_AIDialFail_RollsBackPool(t *testing.T) {
	coord, pool, _, disp, dialer := newStack(t)
	dialer.dialErr = io.ErrUnexpectedEOF
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runStack(ctx, pool, disp)

	mgr := newMgr()
	sess := createSession(t, mgr, "s1")

	if _, err := coord.Start(sess); err == nil {
		t.Fatal("want error from failed Dial, got nil")
	}
	if pool.SessionCount() != 0 {
		t.Errorf("pool should be empty after rollback, got %d", pool.SessionCount())
	}
}

func TestStart_InvalidCodec_RollsBack(t *testing.T) {
	coord, pool, _, disp, _ := newStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runStack(ctx, pool, disp)

	mgr := newMgr()
	sess, _ := newMgr().Create(session.SessionConfig{
		ID: "bad", Codec: "G729", SampleRate: 8000, Channels: 1,
	})
	_ = mgr
	if _, err := coord.Start(sess); err == nil {
		t.Fatal("want error for unsupported codec")
	}
	if pool.SessionCount() != 0 {
		t.Errorf("pool should be empty after rollback, got %d", pool.SessionCount())
	}
}

func TestStart_PacketFlowsToAIClient(t *testing.T) {
	coord, pool, _, disp, dialer := newStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runStack(ctx, pool, disp)

	mgr := newMgr()
	sess := createSession(t, mgr, "s1")

	if _, err := coord.Start(sess); err != nil {
		t.Fatalf("Start: %v", err)
	}

	sess.PacketQueue <- pcmuPacket(sess.ID, 1)

	client := waitForClient(t, dialer, sess.ID)

	select {
	case chunk := <-client.chunks:
		if chunk.SessionID != sess.ID {
			t.Errorf("chunk.SessionID: want %s, got %s", sess.ID, chunk.SessionID)
		}
		if chunk.SampleRate != 16000 {
			t.Errorf("chunk.SampleRate: want 16000, got %d", chunk.SampleRate)
		}
		if chunk.DurationMs != 20 {
			t.Errorf("chunk.DurationMs: want 20ms, got %d", chunk.DurationMs)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: no AudioChunk arrived at AI client")
	}
}

func TestStart_ResultFlowsToSink(t *testing.T) {
	coord, pool, _, disp, dialer := newStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runStack(ctx, pool, disp)

	mgr := newMgr()
	sess := createSession(t, mgr, "s1")

	received := make(chan pipeline.RecognitionResult, 4)
	disp.RegisterSink(sess.ID, result.NewSinkFunc("test", func(_ context.Context, r pipeline.RecognitionResult) error {
		received <- r
		return nil
	}))

	if _, err := coord.Start(sess); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Send a packet so the AI stream goroutines are live.
	sess.PacketQueue <- pcmuPacket(sess.ID, 1)
	client := waitForClient(t, dialer, sess.ID)

	// Wait until the chunk arrives at the client (stream goroutines confirmed live).
	select {
	case <-client.chunks:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for chunk at AI client")
	}

	// Inject a recognition result.
	want := pipeline.RecognitionResult{SessionID: sess.ID, Text: "xin chào", IsFinal: true}
	client.results <- want

	select {
	case got := <-received:
		if got.Text != want.Text {
			t.Errorf("result.Text: want %q, got %q", want.Text, got.Text)
		}
		if got.SessionID != want.SessionID {
			t.Errorf("result.SessionID mismatch")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout: result not delivered to sink")
	}
}

func TestStart_SessionCtxCancel_UnregistersPool(t *testing.T) {
	coord, pool, _, disp, _ := newStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runStack(ctx, pool, disp)

	mgr := newMgr()
	sess1 := createSession(t, mgr, "s1")
	sess2 := createSession(t, mgr, "s2")

	if _, err := coord.Start(sess1); err != nil {
		t.Fatalf("Start s1: %v", err)
	}
	if _, err := coord.Start(sess2); err != nil {
		t.Fatalf("Start s2: %v", err)
	}
	if pool.SessionCount() != 2 {
		t.Fatalf("want 2 sessions before close, got %d", pool.SessionCount())
	}

	mgr.Close("s2")

	deadline := time.Now().Add(500 * time.Millisecond)
	for pool.SessionCount() > 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if pool.SessionCount() != 1 {
		t.Errorf("pool.SessionCount after close: want 1, got %d", pool.SessionCount())
	}
}

func TestStart_SessionCtxCancel_UnregistersSinks(t *testing.T) {
	coord, pool, _, disp, dialer := newStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runStack(ctx, pool, disp)

	mgr := newMgr()
	sess := createSession(t, mgr, "sink-sess")

	delivered := make(chan struct{}, 8)
	disp.RegisterSink(sess.ID, result.NewSinkFunc("test", func(_ context.Context, _ pipeline.RecognitionResult) error {
		delivered <- struct{}{}
		return nil
	}))

	if _, err := coord.Start(sess); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Send packet so AI stream is live, then inject one result before close.
	sess.PacketQueue <- pcmuPacket(sess.ID, 1)
	client := waitForClient(t, dialer, sess.ID)
	select {
	case <-client.chunks:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for chunk")
	}
	client.results <- pipeline.RecognitionResult{SessionID: sess.ID, Text: "before", IsFinal: true}
	select {
	case <-delivered:
	case <-time.After(time.Second):
		t.Fatal("result before close not delivered")
	}

	// Close session → cleanup goroutine calls UnregisterSinks.
	mgr.Close(sess.ID)
	time.Sleep(50 * time.Millisecond)

	// Drain any extra deliveries during shutdown.
	for len(delivered) > 0 {
		<-delivered
	}

	// Push result directly to dispatcher — sink should not fire.
	_ = disp.Push(context.Background(), pipeline.RecognitionResult{SessionID: sess.ID, Text: "after"})
	time.Sleep(50 * time.Millisecond)

	if len(delivered) != 0 {
		t.Error("sink received result after UnregisterSinks")
	}
}

func TestStart_MultipleSessions(t *testing.T) {
	coord, pool, aiMgr, disp, _ := newStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runStack(ctx, pool, disp)

	mgr := newMgr()
	const n = 5
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("ms-%d", i)
		sess := createSession(t, mgr, id)
		if _, err := coord.Start(sess); err != nil {
			t.Fatalf("Start %s: %v", id, err)
		}
	}

	if pool.SessionCount() != n {
		t.Errorf("pool.SessionCount: want %d, got %d", n, pool.SessionCount())
	}
	if aiMgr.Count() != n {
		t.Errorf("aiMgr.Count: want %d, got %d", n, aiMgr.Count())
	}
}

func TestStart_TouchCalledOnPacketArrival(t *testing.T) {
	coord, pool, _, disp, _ := newStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runStack(ctx, pool, disp)

	mgr := newMgr()
	sess := createSession(t, mgr, "s1")

	if _, err := coord.Start(sess); err != nil {
		t.Fatalf("Start: %v", err)
	}

	before := sess.LastPacketAt()
	time.Sleep(5 * time.Millisecond) // ensure measurable gap
	sess.PacketQueue <- pcmuPacket(sess.ID, 1)
	time.Sleep(20 * time.Millisecond) // let packet pump goroutine run

	if after := sess.LastPacketAt(); !after.After(before) {
		t.Error("Touch() was not called on packet arrival")
	}
}
