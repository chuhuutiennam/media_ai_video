package controlplane

import (
	"context"
	"fmt"
	"testing"
	"time"

	"media-ai-gateway/internal/ai"
	"media-ai-gateway/internal/pipeline"
	"media-ai-gateway/internal/session"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func newSessMgr(maxSessions int) *session.Manager {
	return session.NewManager(session.ManagerConfig{
		MaxSessions:     maxSessions,
		PacketQueueSize: 4,
		AudioQueueSize:  4,
		ResultQueueSize: 4,
		IdleTimeout:     time.Minute,
		GCInterval:      time.Minute,
	})
}

// stubPool là poolQueue stub dùng để inject giá trị queue metrics trực tiếp trong tests.
// Thay thế real WorkerPool để tránh race condition với per-session goroutine.
type stubPool struct{ len, cap int }

func (s *stubPool) QueueLen() int { return s.len }
func (s *stubPool) QueueCap() int { return s.cap }

// newPool tạo stubPool với capacity cho trước và queue rỗng.
func newPool(queueSize int) *stubPool {
	return &stubPool{cap: queueSize}
}

// fillQueue đặt số lượng items "đang chờ" trong stub pool.
func fillQueue(pool *stubPool, n int) {
	pool.len = n
}

func newAIMgr(maxStreams int) *ai.Manager {
	return ai.NewManager(ai.Config{MaxActiveStreams: maxStreams, StreamTimeout: time.Minute}, ai.NullDialer{})
}

// fillSessions tạo n session trong mgr với ID "s0", "s1", ...
func fillSessions(t *testing.T, mgr *session.Manager, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		_, err := mgr.Create(session.SessionConfig{
			ID: fmt.Sprintf("s%d", i), Codec: "PCMU", SampleRate: 8000, Channels: 1,
		})
		if err != nil {
			t.Fatalf("fillSessions %d: %v", i, err)
		}
	}
}

// openStreams mở n AI streams; trả về cancel để dọn goroutine sau test.
func openStreams(t *testing.T, mgr *ai.Manager, n int) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("ai%d", i)
		_, err := mgr.Open(ctx, id, id, "vi", "transcribe",
			make(chan pipeline.AudioChunk, 1),
			make(chan pipeline.RecognitionResult, 1),
		)
		if err != nil {
			cancel()
			t.Fatalf("openStreams %d: %v", i, err)
		}
	}
	return cancel
}

// newAC tạo AdmissionController với deps sạch.
func newAC(sessMgr *session.Manager, pool poolQueue, aiMgr *ai.Manager, portAlloc *PortAllocator) *AdmissionController {
	return NewAdmissionController(sessMgr, pool, aiMgr, portAlloc)
}

// ── tests ─────────────────────────────────────────────────────────────────────

func TestAdmission_AllResourcesAvailable(t *testing.T) {
	ac := newAC(newSessMgr(100), newPool(100), newAIMgr(100), nil)
	ok, ms := ac.Check()
	if !ok || ms != 0 {
		t.Errorf("want (true, 0), got (%v, %d)", ok, ms)
	}
}

// ── Session threshold (90%) ───────────────────────────────────────────────────

func TestAdmission_Sessions_AtThreshold_Rejected(t *testing.T) {
	mgr := newSessMgr(10)
	fillSessions(t, mgr, 9) // 9/10 = 90%

	ac := newAC(mgr, newPool(100), newAIMgr(100), nil)
	ok, ms := ac.Check()
	if ok {
		t.Fatal("want rejected at 90% sessions")
	}
	if ms != 5000 {
		t.Errorf("retryAfterMs: want 5000, got %d", ms)
	}
}

func TestAdmission_Sessions_BelowThreshold_Accepted(t *testing.T) {
	mgr := newSessMgr(10)
	fillSessions(t, mgr, 8) // 8/10 = 80% < 90%

	ac := newAC(mgr, newPool(100), newAIMgr(100), nil)
	ok, _ := ac.Check()
	if !ok {
		t.Error("want accepted at 80% sessions")
	}
}

func TestAdmission_Sessions_Full_Rejected(t *testing.T) {
	mgr := newSessMgr(5)
	fillSessions(t, mgr, 5) // 100%

	ac := newAC(mgr, newPool(100), newAIMgr(100), nil)
	ok, ms := ac.Check()
	if ok || ms != 5000 {
		t.Errorf("want (false, 5000), got (%v, %d)", ok, ms)
	}
}

// ── Queue threshold (80%) ─────────────────────────────────────────────────────

func TestAdmission_Queue_AtThreshold_Rejected(t *testing.T) {
	pool := newPool(10)
	fillQueue(pool, 8) // 8/10 = 80%

	ac := newAC(newSessMgr(100), pool, newAIMgr(100), nil)
	ok, ms := ac.Check()
	if ok {
		t.Fatal("want rejected at 80% queue")
	}
	if ms != 1000 {
		t.Errorf("retryAfterMs: want 1000, got %d", ms)
	}
}

func TestAdmission_Queue_BelowThreshold_Accepted(t *testing.T) {
	pool := newPool(10)
	fillQueue(pool, 7) // 7/10 = 70% < 80%

	ac := newAC(newSessMgr(100), pool, newAIMgr(100), nil)
	ok, _ := ac.Check()
	if !ok {
		t.Error("want accepted at 70% queue")
	}
}

func TestAdmission_Queue_ZeroCap_CheckSkipped(t *testing.T) {
	// QueueSize=0 → QueueCap()=0 → division guard skips queue check entirely.
	pool := newPool(0)

	ac := newAC(newSessMgr(100), pool, newAIMgr(100), nil)
	ok, _ := ac.Check()
	if !ok {
		t.Error("QueueCap=0 should skip queue check and accept")
	}
}

// ── AI stream threshold (95%) ─────────────────────────────────────────────────

func TestAdmission_AIStreams_AtThreshold_Rejected(t *testing.T) {
	aiMgr := newAIMgr(20)
	cancel := openStreams(t, aiMgr, 19) // 19/20 = 95%
	t.Cleanup(cancel)

	ac := newAC(newSessMgr(100), newPool(100), aiMgr, nil)
	ok, ms := ac.Check()
	if ok {
		t.Fatal("want rejected at 95% AI streams")
	}
	if ms != 2000 {
		t.Errorf("retryAfterMs: want 2000, got %d", ms)
	}
}

func TestAdmission_AIStreams_BelowThreshold_Accepted(t *testing.T) {
	aiMgr := newAIMgr(20)
	cancel := openStreams(t, aiMgr, 18) // 18/20 = 90% < 95%
	t.Cleanup(cancel)

	ac := newAC(newSessMgr(100), newPool(100), aiMgr, nil)
	ok, _ := ac.Check()
	if !ok {
		t.Error("want accepted at 90% AI streams")
	}
}

// ── Port pool threshold ────────────────────────────────────────────────────────

func TestAdmission_Ports_Exhausted_Rejected(t *testing.T) {
	alloc, err := NewPortAllocator(20000, 20002) // 3 ports
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := alloc.Acquire(); err != nil {
			t.Fatalf("acquire port %d: %v", i, err)
		}
	}

	ac := newAC(newSessMgr(100), newPool(100), newAIMgr(100), alloc)
	ok, ms := ac.Check()
	if ok {
		t.Fatal("want rejected when port pool exhausted")
	}
	if ms != 10000 {
		t.Errorf("retryAfterMs: want 10000, got %d", ms)
	}
}

func TestAdmission_Ports_Available_Accepted(t *testing.T) {
	alloc, err := NewPortAllocator(20100, 20102) // 3 ports, none acquired
	if err != nil {
		t.Fatal(err)
	}

	ac := newAC(newSessMgr(100), newPool(100), newAIMgr(100), alloc)
	ok, _ := ac.Check()
	if !ok {
		t.Error("want accepted when ports available")
	}
}

func TestAdmission_NilPortAlloc_Accepted(t *testing.T) {
	ac := newAC(newSessMgr(100), newPool(100), newAIMgr(100), nil)
	ok, _ := ac.Check()
	if !ok {
		t.Error("nil portAlloc should skip port check and accept")
	}
}

// ── Evaluation order ──────────────────────────────────────────────────────────

// TestAdmission_Order_Sessions_BeforeQueue: sessions check thất bại trước queue check.
func TestAdmission_Order_Sessions_BeforeQueue(t *testing.T) {
	mgr := newSessMgr(10)
	fillSessions(t, mgr, 9) // sessions = 90% → sẽ reject

	pool := newPool(10)
	fillQueue(pool, 8) // queue = 80% → cũng sẽ reject, nhưng sessions được kiểm tra trước

	ac := newAC(mgr, pool, newAIMgr(100), nil)
	_, ms := ac.Check()
	if ms != 5000 {
		t.Errorf("sessions check should fire first (retryMs=5000), got %d", ms)
	}
}

// TestAdmission_Order_Queue_BeforeAI: queue check thất bại trước AI check.
func TestAdmission_Order_Queue_BeforeAI(t *testing.T) {
	pool := newPool(10)
	fillQueue(pool, 8) // queue = 80% → reject

	aiMgr := newAIMgr(20)
	cancel := openStreams(t, aiMgr, 19) // AI = 95% → cũng reject, nhưng queue được kiểm tra trước
	t.Cleanup(cancel)

	ac := newAC(newSessMgr(100), pool, aiMgr, nil)
	_, ms := ac.Check()
	if ms != 1000 {
		t.Errorf("queue check should fire before AI check (retryMs=1000), got %d", ms)
	}
}

// TestAdmission_Order_AI_BeforePorts: AI check thất bại trước port check.
func TestAdmission_Order_AI_BeforePorts(t *testing.T) {
	aiMgr := newAIMgr(20)
	cancel := openStreams(t, aiMgr, 19) // AI = 95% → reject
	t.Cleanup(cancel)

	alloc, _ := NewPortAllocator(20200, 20200)
	if _, err := alloc.Acquire(); err != nil {
		t.Fatal(err)
	} // port exhausted → cũng reject

	ac := newAC(newSessMgr(100), newPool(100), aiMgr, alloc)
	_, ms := ac.Check()
	if ms != 2000 {
		t.Errorf("AI check should fire before port check (retryMs=2000), got %d", ms)
	}
}

// ── CheckWithReason ───────────────────────────────────────────────────────────

func TestAdmission_CheckWithReason_OkHasEmptyReason(t *testing.T) {
	ac := newAC(newSessMgr(100), newPool(100), newAIMgr(100), nil)
	ok, ms, reason := ac.CheckWithReason()
	if !ok || ms != 0 || reason != "" {
		t.Errorf("want (true, 0, \"\"), got (%v, %d, %q)", ok, ms, reason)
	}
}

func TestAdmission_CheckWithReason_SessionCapacity(t *testing.T) {
	mgr := newSessMgr(10)
	fillSessions(t, mgr, 9)
	ac := newAC(mgr, newPool(100), newAIMgr(100), nil)
	_, _, reason := ac.CheckWithReason()
	if reason != "session_capacity" {
		t.Errorf("reason = %q, want %q", reason, "session_capacity")
	}
}

func TestAdmission_CheckWithReason_QueuePressure(t *testing.T) {
	pool := newPool(10)
	fillQueue(pool, 8)
	ac := newAC(newSessMgr(100), pool, newAIMgr(100), nil)
	_, _, reason := ac.CheckWithReason()
	if reason != "queue_pressure" {
		t.Errorf("reason = %q, want %q", reason, "queue_pressure")
	}
}

func TestAdmission_CheckWithReason_PortExhausted(t *testing.T) {
	alloc, _ := NewPortAllocator(20300, 20300)
	_, _ = alloc.Acquire()
	ac := newAC(newSessMgr(100), newPool(100), newAIMgr(100), alloc)
	_, _, reason := ac.CheckWithReason()
	if reason != "port_exhausted" {
		t.Errorf("reason = %q, want %q", reason, "port_exhausted")
	}
}

// ── AI Worker Registry check ──────────────────────────────────────────────────

func TestAdmission_WorkerRegistry_NoWorkers_Rejected(t *testing.T) {
	reg := ai.NewWorkerRegistry(0) // empty — no fresh workers
	ac := newAC(newSessMgr(100), newPool(100), newAIMgr(100), nil)
	ac.SetWorkerRegistry(reg)

	ok, ms, reason := ac.CheckWithReason()
	if ok {
		t.Fatal("want rejected when worker registry is empty")
	}
	if ms != 3000 {
		t.Errorf("retryAfterMs = %d, want 3000", ms)
	}
	if reason != "no_ai_worker" {
		t.Errorf("reason = %q, want %q", reason, "no_ai_worker")
	}
}

func TestAdmission_WorkerRegistry_HasWorker_Accepted(t *testing.T) {
	reg := ai.NewWorkerRegistry(0)
	reg.Register(ai.WorkerInfo{ID: "w1", Addr: "w1:50051"})
	ac := newAC(newSessMgr(100), newPool(100), newAIMgr(100), nil)
	ac.SetWorkerRegistry(reg)

	ok, _, _ := ac.CheckWithReason()
	if !ok {
		t.Error("want accepted when registry has a fresh worker")
	}
}

func TestAdmission_WorkerRegistry_Nil_Skipped(t *testing.T) {
	// nil workerRegistry → check skipped, should accept.
	ac := newAC(newSessMgr(100), newPool(100), newAIMgr(100), nil)
	// workerRegistry not set
	ok, _, _ := ac.CheckWithReason()
	if !ok {
		t.Error("nil workerRegistry should skip AI reachability check")
	}
}

func TestAdmission_WorkerRegistry_StaleWorker_Rejected(t *testing.T) {
	// TTL=1ms: worker registers then expires → should reject.
	reg := ai.NewWorkerRegistry(1)
	reg.Register(ai.WorkerInfo{ID: "w1", Addr: "w1:50051"})
	time.Sleep(5 * time.Millisecond) // wait for TTL to expire

	ac := newAC(newSessMgr(100), newPool(100), newAIMgr(100), nil)
	ac.SetWorkerRegistry(reg)

	ok, _, reason := ac.CheckWithReason()
	if ok {
		t.Fatal("want rejected when all workers are stale")
	}
	if reason != "no_ai_worker" {
		t.Errorf("reason = %q, want %q", reason, "no_ai_worker")
	}
}

// ── Memory threshold check ────────────────────────────────────────────────────

func TestAdmission_Memory_ZeroThreshold_Skipped(t *testing.T) {
	ac := newAC(newSessMgr(100), newPool(100), newAIMgr(100), nil)
	// memThreshold = 0 (default) → skip
	ok, _, _ := ac.CheckWithReason()
	if !ok {
		t.Error("zero memThreshold should skip memory check")
	}
}

func TestAdmission_Memory_BelowThreshold_Accepted(t *testing.T) {
	ac := newAC(newSessMgr(100), newPool(100), newAIMgr(100), nil)
	ac.SetMemThreshold(^uint64(0)) // max uint64 — never exceeded
	ok, _, _ := ac.CheckWithReason()
	if !ok {
		t.Error("want accepted when heap is below threshold")
	}
}

func TestAdmission_Memory_AboveThreshold_Rejected(t *testing.T) {
	ac := newAC(newSessMgr(100), newPool(100), newAIMgr(100), nil)
	ac.SetMemThreshold(1) // threshold = 1 byte — always exceeded
	ok, ms, reason := ac.CheckWithReason()
	if ok {
		t.Fatal("want rejected when heap exceeds threshold")
	}
	if ms != 5000 {
		t.Errorf("retryAfterMs = %d, want 5000", ms)
	}
	if reason != "memory_pressure" {
		t.Errorf("reason = %q, want %q", reason, "memory_pressure")
	}
}

// ── Order: worker check before memory, memory before ports ───────────────────

func TestAdmission_Order_Worker_BeforeMemory(t *testing.T) {
	reg := ai.NewWorkerRegistry(0) // empty → no_ai_worker (retryMs=3000)
	ac := newAC(newSessMgr(100), newPool(100), newAIMgr(100), nil)
	ac.SetWorkerRegistry(reg)
	ac.SetMemThreshold(1) // also triggers memory_pressure (retryMs=5000)

	_, ms, _ := ac.CheckWithReason()
	if ms != 3000 {
		t.Errorf("worker check should fire before memory check (retryMs=3000), got %d", ms)
	}
}

func TestAdmission_Order_Memory_BeforePorts(t *testing.T) {
	alloc, _ := NewPortAllocator(20400, 20400)
	_, _ = alloc.Acquire() // port_exhausted (retryMs=10000)
	ac := newAC(newSessMgr(100), newPool(100), newAIMgr(100), alloc)
	ac.SetMemThreshold(1) // memory_pressure (retryMs=5000) — fires first

	_, ms, _ := ac.CheckWithReason()
	if ms != 5000 {
		t.Errorf("memory check should fire before port check (retryMs=5000), got %d", ms)
	}
}
