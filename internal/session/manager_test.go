package session

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// --- helpers ---

func smallCfg() ManagerConfig {
	return ManagerConfig{
		MaxSessions:     5,
		IdleTimeout:     30 * time.Second,
		PacketQueueSize: 8,
		AudioQueueSize:  8,
		ResultQueueSize: 8,
		GCInterval:      time.Second,
	}
}

func mkSession(id string, ssrc uint32) SessionConfig {
	return SessionConfig{
		ID:         id,
		SourceType: "raw_rtp",
		SSRC:       ssrc,
		Codec:      "PCMU",
		SampleRate: 8000,
		Channels:   1,
		Language:   "vi",
		Task:       "transcribe",
	}
}

// --- Create ---

func TestManager_Create_Fields(t *testing.T) {
	m := NewManager(smallCfg())
	s, err := m.Create(mkSession("s1", 1111))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.ID != "s1" {
		t.Errorf("ID = %q, want %q", s.ID, "s1")
	}
	if s.SSRC != 1111 {
		t.Errorf("SSRC = %d, want 1111", s.SSRC)
	}
	if s.Codec != "PCMU" {
		t.Errorf("Codec = %q, want PCMU", s.Codec)
	}
	if s.Status() != StatusCreated {
		t.Errorf("Status = %v, want CREATED", s.Status())
	}
	if s.Ctx == nil {
		t.Error("Ctx is nil")
	}
	if cap(s.PacketQueue) != 8 {
		t.Errorf("PacketQueue cap = %d, want 8", cap(s.PacketQueue))
	}
	if cap(s.ResultQueue) != 8 {
		t.Errorf("ResultQueue cap = %d, want 8", cap(s.ResultQueue))
	}
	if s.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
	if m.Count() != 1 {
		t.Errorf("Count = %d, want 1", m.Count())
	}
}

func TestManager_Create_MaxSessions(t *testing.T) {
	m := NewManager(smallCfg()) // max 5
	for i := 0; i < 5; i++ {
		if _, err := m.Create(mkSession(fmt.Sprintf("s%d", i), uint32(i+1))); err != nil {
			t.Fatalf("Create s%d: %v", i, err)
		}
	}
	_, err := m.Create(mkSession("overflow", 99))
	if err != ErrMaxSessions {
		t.Errorf("got %v, want ErrMaxSessions", err)
	}
}

func TestManager_Create_DuplicateID(t *testing.T) {
	m := NewManager(smallCfg())
	m.Create(mkSession("s1", 1))
	_, err := m.Create(mkSession("s1", 2))
	if err != ErrDuplicateID {
		t.Errorf("got %v, want ErrDuplicateID", err)
	}
	// Count must not increase on duplicate.
	if m.Count() != 1 {
		t.Errorf("Count = %d after duplicate, want 1", m.Count())
	}
}

func TestManager_Create_ZeroSSRC_NoIndex(t *testing.T) {
	m := NewManager(smallCfg())
	m.Create(mkSession("s1", 0)) // SSRC 0 must not enter ssrcIndex
	_, ok := m.GetBySSRC(0)
	if ok {
		t.Error("SSRC 0 should not be in the index")
	}
}

// --- Get ---

func TestManager_Get_Found(t *testing.T) {
	m := NewManager(smallCfg())
	m.Create(mkSession("s1", 1))
	s, ok := m.Get("s1")
	if !ok || s == nil || s.ID != "s1" {
		t.Errorf("Get: ok=%v s=%v", ok, s)
	}
}

func TestManager_Get_NotFound(t *testing.T) {
	m := NewManager(smallCfg())
	_, ok := m.Get("ghost")
	if ok {
		t.Error("Get missing: expected false")
	}
}

// --- GetByPeerID / GetByTrackID ---

func mkWebRTCSession(id, peerID, trackID string) SessionConfig {
	return SessionConfig{
		ID:         id,
		SourceType: "webrtc",
		PeerID:     peerID,
		TrackID:    trackID,
		Codec:      "opus",
		SampleRate: 48000,
		Channels:   2,
	}
}

func TestManager_GetByPeerID_Found(t *testing.T) {
	m := NewManager(smallCfg())
	m.Create(mkWebRTCSession("s1", "peer-abc", "track-xyz"))
	s, ok := m.GetByPeerID("peer-abc")
	if !ok || s == nil || s.ID != "s1" {
		t.Errorf("GetByPeerID: ok=%v s=%v", ok, s)
	}
}

func TestManager_GetByPeerID_NotFound(t *testing.T) {
	m := NewManager(smallCfg())
	_, ok := m.GetByPeerID("no-such-peer")
	if ok {
		t.Error("GetByPeerID missing: expected false")
	}
}

func TestManager_GetByPeerID_EmptyPeerID_NoIndex(t *testing.T) {
	m := NewManager(smallCfg())
	m.Create(mkWebRTCSession("s1", "", "")) // no PeerID → must not index
	_, ok := m.GetByPeerID("")
	if ok {
		t.Error("empty PeerID must not be indexed")
	}
}

func TestManager_GetByTrackID_Found(t *testing.T) {
	m := NewManager(smallCfg())
	m.Create(mkWebRTCSession("s1", "peer-abc", "track-xyz"))
	s, ok := m.GetByTrackID("track-xyz")
	if !ok || s == nil || s.ID != "s1" {
		t.Errorf("GetByTrackID: ok=%v s=%v", ok, s)
	}
}

func TestManager_GetByTrackID_NotFound(t *testing.T) {
	m := NewManager(smallCfg())
	_, ok := m.GetByTrackID("no-such-track")
	if ok {
		t.Error("GetByTrackID missing: expected false")
	}
}

func TestManager_GetByTrackID_EmptyTrackID_NoIndex(t *testing.T) {
	m := NewManager(smallCfg())
	m.Create(mkWebRTCSession("s1", "", "")) // no TrackID → must not index
	_, ok := m.GetByTrackID("")
	if ok {
		t.Error("empty TrackID must not be indexed")
	}
}

func TestManager_Close_RemovesPeerIndex(t *testing.T) {
	m := NewManager(smallCfg())
	m.Create(mkWebRTCSession("s1", "peer-abc", "track-xyz"))
	m.Close("s1")
	if _, ok := m.GetByPeerID("peer-abc"); ok {
		t.Error("peerIndex not removed after Close")
	}
}

func TestManager_Close_RemovesTrackIndex(t *testing.T) {
	m := NewManager(smallCfg())
	m.Create(mkWebRTCSession("s1", "peer-abc", "track-xyz"))
	m.Close("s1")
	if _, ok := m.GetByTrackID("track-xyz"); ok {
		t.Error("trackIndex not removed after Close")
	}
}

func TestManager_GC_RemovesPeerTrackIndex(t *testing.T) {
	cfg := smallCfg()
	cfg.IdleTimeout = 10 * time.Millisecond
	m := NewManager(cfg)
	m.Create(mkWebRTCSession("s1", "peer-gc", "track-gc"))

	// Advance time past idle timeout so GC evicts the session.
	m.gc(time.Now().Add(time.Second))

	if _, ok := m.GetByPeerID("peer-gc"); ok {
		t.Error("peerIndex not removed after GC eviction")
	}
	if _, ok := m.GetByTrackID("track-gc"); ok {
		t.Error("trackIndex not removed after GC eviction")
	}
}

// --- GetByAddr ---

func mkSessionWithAddr(id string, ssrc uint32, addr string) SessionConfig {
	cfg := mkSession(id, ssrc)
	cfg.RemoteAddr = addr
	return cfg
}

func TestManager_GetByAddr_Found(t *testing.T) {
	m := NewManager(smallCfg())
	m.Create(mkSessionWithAddr("s1", 1, "10.0.0.1:5004"))
	s, ok := m.GetByAddr("10.0.0.1:5004")
	if !ok || s == nil || s.ID != "s1" {
		t.Errorf("GetByAddr: ok=%v s=%v", ok, s)
	}
}

func TestManager_GetByAddr_NotFound(t *testing.T) {
	m := NewManager(smallCfg())
	_, ok := m.GetByAddr("1.2.3.4:9999")
	if ok {
		t.Error("GetByAddr missing: expected false")
	}
}

func TestManager_GetByAddr_EmptyAddr_NoIndex(t *testing.T) {
	m := NewManager(smallCfg())
	m.Create(mkSession("s1", 1)) // RemoteAddr="" → must not index
	_, ok := m.GetByAddr("")
	if ok {
		t.Error("empty RemoteAddr must not be indexed")
	}
}

func TestManager_Close_RemovesAddrIndex(t *testing.T) {
	m := NewManager(smallCfg())
	m.Create(mkSessionWithAddr("s1", 1, "192.168.1.1:5060"))
	m.Close("s1")
	if _, ok := m.GetByAddr("192.168.1.1:5060"); ok {
		t.Error("addrIndex not removed after Close")
	}
}

func TestManager_GC_RemovesAddrIndex(t *testing.T) {
	cfg := smallCfg()
	cfg.IdleTimeout = 10 * time.Millisecond
	m := NewManager(cfg)
	m.Create(mkSessionWithAddr("s1", 1, "10.1.1.1:5004"))

	m.gc(time.Now().Add(time.Second))

	if _, ok := m.GetByAddr("10.1.1.1:5004"); ok {
		t.Error("addrIndex not removed after GC eviction")
	}
}

// --- GetBySSRC ---

func TestManager_GetBySSRC_Found(t *testing.T) {
	m := NewManager(smallCfg())
	m.Create(mkSession("s1", 9999))
	s, ok := m.GetBySSRC(9999)
	if !ok || s == nil || s.ID != "s1" {
		t.Errorf("GetBySSRC: ok=%v s=%v", ok, s)
	}
}

func TestManager_GetBySSRC_NotFound(t *testing.T) {
	m := NewManager(smallCfg())
	_, ok := m.GetBySSRC(42)
	if ok {
		t.Error("GetBySSRC missing: expected false")
	}
}

// --- Close ---

func TestManager_Close_RemovesFromMap(t *testing.T) {
	m := NewManager(smallCfg())
	m.Create(mkSession("s1", 1))
	ok := m.Close("s1")
	if !ok {
		t.Error("Close: expected true")
	}
	if m.Count() != 0 {
		t.Errorf("Count = %d after close, want 0", m.Count())
	}
	if _, found := m.Get("s1"); found {
		t.Error("session still in map after Close")
	}
}

func TestManager_Close_RemovesSSRCIndex(t *testing.T) {
	m := NewManager(smallCfg())
	m.Create(mkSession("s1", 7777))
	m.Close("s1")
	if _, ok := m.GetBySSRC(7777); ok {
		t.Error("SSRC index not removed after Close")
	}
}

func TestManager_Close_CancelsCtx(t *testing.T) {
	m := NewManager(smallCfg())
	s, _ := m.Create(mkSession("s1", 1))
	m.Close("s1")
	select {
	case <-s.Ctx.Done():
		// expected
	default:
		t.Error("Ctx should be cancelled after Close")
	}
}

func TestManager_Close_StatusClosed(t *testing.T) {
	m := NewManager(smallCfg())
	s, _ := m.Create(mkSession("s1", 1))
	m.Close("s1")
	if s.Status() != StatusClosed {
		t.Errorf("Status = %v, want CLOSED", s.Status())
	}
}

func TestManager_Close_NotFound(t *testing.T) {
	m := NewManager(smallCfg())
	if m.Close("ghost") {
		t.Error("Close non-existent: expected false")
	}
}

func TestManager_Close_Idempotent(t *testing.T) {
	m := NewManager(smallCfg())
	m.Create(mkSession("s1", 1))
	m.Close("s1")
	// Second close must return false, not panic.
	if m.Close("s1") {
		t.Error("second Close should return false")
	}
}

// --- Session Touch / status transitions ---

func TestSession_Touch_CreatedToActive(t *testing.T) {
	m := NewManager(smallCfg())
	s, _ := m.Create(mkSession("s1", 1))
	if s.Status() != StatusCreated {
		t.Fatalf("initial status = %v, want CREATED", s.Status())
	}
	s.Touch()
	if s.Status() != StatusActive {
		t.Errorf("status after Touch = %v, want ACTIVE", s.Status())
	}
}

func TestSession_Touch_IdleToActive(t *testing.T) {
	m := NewManager(smallCfg())
	s, _ := m.Create(mkSession("s1", 1))
	s.setStatus(StatusIdle)
	s.Touch()
	if s.Status() != StatusActive {
		t.Errorf("status after Touch from IDLE = %v, want ACTIVE", s.Status())
	}
}

func TestSession_Touch_UpdatesLastPacketAt(t *testing.T) {
	m := NewManager(smallCfg())
	s, _ := m.Create(mkSession("s1", 1))
	before := s.LastPacketAt()
	time.Sleep(time.Millisecond) // ensure clock advances
	s.Touch()
	after := s.LastPacketAt()
	if !after.After(before) {
		t.Error("LastPacketAt should advance after Touch")
	}
}

// --- GC ---

func TestManager_GC_EvictsIdle(t *testing.T) {
	m := NewManager(smallCfg()) // IdleTimeout = 30s
	m.Create(mkSession("s1", 1))
	// advance now by 31s — session was created just now, so it's idle
	m.gc(time.Now().Add(31 * time.Second))
	if m.Count() != 0 {
		t.Errorf("GC: Count = %d, want 0 after eviction", m.Count())
	}
}

func TestManager_GC_SkipsActive(t *testing.T) {
	m := NewManager(smallCfg()) // IdleTimeout = 30s
	s, _ := m.Create(mkSession("s1", 1))
	s.Touch() // resets lastPacketAt to now
	// advance only 5s — still within timeout
	m.gc(time.Now().Add(5 * time.Second))
	if m.Count() != 1 {
		t.Error("GC: should not evict session that is still active")
	}
}

func TestManager_GC_EvictedCtxCancelled(t *testing.T) {
	m := NewManager(smallCfg())
	s, _ := m.Create(mkSession("s1", 1))
	m.gc(time.Now().Add(31 * time.Second))
	select {
	case <-s.Ctx.Done():
		// expected
	default:
		t.Error("GC-evicted session Ctx should be cancelled")
	}
}

func TestManager_GC_RemovesSSRCIndex(t *testing.T) {
	m := NewManager(smallCfg())
	m.Create(mkSession("s1", 5555))
	m.gc(time.Now().Add(31 * time.Second))
	if _, ok := m.GetBySSRC(5555); ok {
		t.Error("SSRC index should be cleaned up by GC")
	}
}

func TestManager_GC_SkipsClosed(t *testing.T) {
	m := NewManager(smallCfg())
	s, _ := m.Create(mkSession("s1", 1))
	s.setStatus(StatusClosed)
	// Even if time is past, a CLOSED session should not be re-evicted.
	m.gc(time.Now().Add(31 * time.Second))
	// The session was not in the map anyway (setStatus doesn't remove it),
	// so GC sees it as idle — let's verify GC doesn't panic on a CLOSED session.
	// The real check: setStatus CLOSED causes isIdle to return false.
	if s.isIdle(time.Now().Add(31*time.Second), 30*time.Second) {
		t.Error("CLOSED session should not be considered idle")
	}
}

// --- Run lifecycle ---

func TestManager_Run_ExitsOnCtxCancel(t *testing.T) {
	m := NewManager(smallCfg())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
		// expected
	case <-time.After(time.Second):
		t.Error("Run did not exit after ctx cancel")
	}
}

// --- Concurrency ---

func TestManager_Concurrency(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxSessions = 200
	m := NewManager(cfg)

	const n = 100
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("sess-%d", i)
			m.Create(mkSession(id, uint32(i+1)))
			m.Get(id)
			m.GetBySSRC(uint32(i + 1))
			m.Close(id)
		}(i)
	}
	wg.Wait()

	if m.Count() != 0 {
		t.Errorf("Count = %d after concurrent create+close, want 0", m.Count())
	}
}

func TestManager_Concurrency_GCAndCreate(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxSessions = 200
	m := NewManager(cfg)

	var wg sync.WaitGroup
	// Half goroutines create; the other half run GC.
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			m.Create(mkSession(fmt.Sprintf("c%d", i), uint32(i+1)))
		}(i)
		go func() {
			defer wg.Done()
			m.gc(time.Now().Add(31 * time.Second))
		}()
	}
	wg.Wait()
	// No panic = pass.
}
