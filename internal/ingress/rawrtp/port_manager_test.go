package rawrtp

import (
	"net"
	"sync"
	"testing"
	"time"

	"media-ai-gateway/internal/pipeline"
	"media-ai-gateway/internal/session"
)

// fakeReleaser tracks Release calls for testing.
type fakeReleaser struct {
	mu       sync.Mutex
	released []int
}

func (f *fakeReleaser) Release(port int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, port)
}

func (f *fakeReleaser) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.released)
}

func makeTestSession(t *testing.T) (*session.Session, func()) {
	t.Helper()
	mgr := session.NewManager(session.ManagerConfig{
		MaxSessions:     10,
		PacketQueueSize: 32,
		AudioQueueSize:  8,
		ResultQueueSize: 8,
		IdleTimeout:     time.Minute,
		GCInterval:      time.Minute,
	})
	sess, err := mgr.Create(session.SessionConfig{
		ID:         "test-sess",
		SourceType: "raw_rtp",
		Codec:      "PCMU",
		SampleRate: 8000,
		Channels:   1,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return sess, func() { mgr.Close("test-sess") }
}

func buildRTPPacket(ssrc uint32, seq uint16, ts uint32, pt uint8, payload []byte) []byte {
	return buildRTP(seq, ts, ssrc, pt, false, payload)
}

func TestPortManager_ReceivesPacket(t *testing.T) {
	releaser := &fakeReleaser{}
	sess, cancel := makeTestSession(t)
	defer cancel()

	ln, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pre-listen: %v", err)
	}
	port := ln.LocalAddr().(*net.UDPAddr).Port
	ln.Close()

	if err := StartSessionListener(sess, "127.0.0.1", port, releaser, 10); err != nil {
		t.Fatalf("StartSessionListener: %v", err)
	}

	conn, err := net.Dial("udp", "127.0.0.1:"+itoa(port))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	pkt := buildRTPPacket(0xDEADBEEF, 1, 100, 0, []byte{0xAB, 0xCD})
	conn.Write(pkt)

	select {
	case got := <-sess.PacketQueue:
		if got.SSRC != 0xDEADBEEF {
			t.Errorf("SSRC: want 0xDEADBEEF, got 0x%08X", got.SSRC)
		}
		if got.Sequence != 1 {
			t.Errorf("Sequence: want 1, got %d", got.Sequence)
		}
		if got.SessionID != "test-sess" {
			t.Errorf("SessionID: want test-sess, got %s", got.SessionID)
		}
		if got.Codec != "PCMU" {
			t.Errorf("Codec: want PCMU, got %s", got.Codec)
		}
		wantPayload := []byte{0xAB, 0xCD}
		if len(got.Payload) != 2 || got.Payload[0] != wantPayload[0] {
			t.Errorf("Payload mismatch: got %v", got.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for packet")
	}
}

func TestPortManager_StopsOnContextCancel(t *testing.T) {
	releaser := &fakeReleaser{}
	sess, cancel := makeTestSession(t)

	ln, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pre-listen: %v", err)
	}
	port := ln.LocalAddr().(*net.UDPAddr).Port
	ln.Close()

	if err := StartSessionListener(sess, "127.0.0.1", port, releaser, 10); err != nil {
		t.Fatalf("StartSessionListener: %v", err)
	}

	cancel()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if releaser.count() > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("port not released after context cancel")
}

func TestPortManager_BindError_ReturnsError(t *testing.T) {
	ln, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pre-bind: %v", err)
	}
	defer ln.Close()
	port := ln.LocalAddr().(*net.UDPAddr).Port

	releaser := &fakeReleaser{}
	sess, cancel := makeTestSession(t)
	defer cancel()

	err = StartSessionListener(sess, "127.0.0.1", port, releaser, 10)
	if err == nil {
		t.Fatal("expected bind error, got nil")
	}
}

func TestPortManager_MalformedPackets_Skipped(t *testing.T) {
	releaser := &fakeReleaser{}
	sess, cancel := makeTestSession(t)
	defer cancel()

	ln, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pre-listen: %v", err)
	}
	port := ln.LocalAddr().(*net.UDPAddr).Port
	ln.Close()

	if err := StartSessionListener(sess, "127.0.0.1", port, releaser, 10); err != nil {
		t.Fatalf("StartSessionListener: %v", err)
	}

	conn, _ := net.Dial("udp", "127.0.0.1:"+itoa(port))
	defer conn.Close()

	conn.Write([]byte{0x80, 0x00}) // too short

	validPkt := buildRTPPacket(0x1234, 7, 200, 0, []byte{0xFF})
	conn.Write(validPkt)

	select {
	case got := <-sess.PacketQueue:
		if got.SSRC != 0x1234 {
			t.Errorf("SSRC: want 0x1234, got 0x%08X", got.SSRC)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: valid packet not received after malformed one")
	}
}

func TestPortManager_PopulatesReceivedAtMs(t *testing.T) {
	releaser := &fakeReleaser{}
	sess, cancel := makeTestSession(t)
	defer cancel()

	ln, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pre-listen: %v", err)
	}
	port := ln.LocalAddr().(*net.UDPAddr).Port
	ln.Close()

	if err := StartSessionListener(sess, "127.0.0.1", port, releaser, 10); err != nil {
		t.Fatalf("StartSessionListener: %v", err)
	}

	before := time.Now().UnixMilli()
	conn, _ := net.Dial("udp", "127.0.0.1:"+itoa(port))
	defer conn.Close()
	conn.Write(buildRTPPacket(1, 1, 1, 0, []byte{0x00}))

	var got pipeline.MediaPacket
	select {
	case got = <-sess.PacketQueue:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
	after := time.Now().UnixMilli()

	if got.ReceivedAtMs < before || got.ReceivedAtMs > after {
		t.Errorf("ReceivedAtMs %d out of range [%d, %d]", got.ReceivedAtMs, before, after)
	}
}

func TestPortManager_QueueFull_Drops(t *testing.T) {
	releaser := &fakeReleaser{}
	// PacketQueueSize=1 so the second packet finds the queue full.
	mgr := session.NewManager(session.ManagerConfig{
		MaxSessions:     10,
		PacketQueueSize: 1,
		AudioQueueSize:  8,
		ResultQueueSize: 8,
		IdleTimeout:     time.Minute,
		GCInterval:      time.Minute,
	})
	sess, err := mgr.Create(session.SessionConfig{
		ID:         "q-sess",
		SourceType: "raw_rtp",
		Codec:      "PCMU",
		SampleRate: 8000,
		Channels:   1,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer mgr.Close("q-sess")

	ln, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pre-listen: %v", err)
	}
	port := ln.LocalAddr().(*net.UDPAddr).Port
	ln.Close()

	if err := StartSessionListener(sess, "127.0.0.1", port, releaser, 10); err != nil {
		t.Fatalf("StartSessionListener: %v", err)
	}

	conn, _ := net.Dial("udp", "127.0.0.1:"+itoa(port))
	defer conn.Close()

	pkt := buildRTPPacket(0xABCD, 1, 100, 0, []byte{0x01})
	conn.Write(pkt)
	conn.Write(pkt) // second packet should be dropped (queue size=1)
	time.Sleep(50 * time.Millisecond)

	// Queue should hold exactly one packet.
	if len(sess.PacketQueue) != 1 {
		t.Errorf("queue len: want 1, got %d", len(sess.PacketQueue))
	}
}

// TestManagerRouter_RoutesPacket verifies ManagerRouter integrates with a real session.Manager.
func TestManagerRouter_RoutesPacket(t *testing.T) {
	mgr := session.NewManager(session.ManagerConfig{
		MaxSessions:     10,
		PacketQueueSize: 8,
		AudioQueueSize:  8,
		ResultQueueSize: 8,
		IdleTimeout:     time.Minute,
		GCInterval:      time.Minute,
	})
	sess, err := mgr.Create(session.SessionConfig{
		ID:         "router-sess",
		SourceType: "raw_rtp",
		SSRC:       0xCAFEBABE,
		Codec:      "PCMA",
		SampleRate: 8000,
		Channels:   1,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer mgr.Close("router-sess")

	router := NewManagerRouter(mgr)

	// Known SSRC should route to the session's queue.
	queue, info, ok := router.RouteBySSRC(0xCAFEBABE)
	if !ok {
		t.Fatal("RouteBySSRC: want ok=true, got false")
	}
	if info.SessionID != "router-sess" {
		t.Errorf("SessionID: want router-sess, got %s", info.SessionID)
	}
	if info.Codec != "PCMA" {
		t.Errorf("Codec: want PCMA, got %s", info.Codec)
	}
	if info.SampleRate != 8000 {
		t.Errorf("SampleRate: want 8000, got %d", info.SampleRate)
	}

	// Verify queue is the session's actual PacketQueue.
	pkt := pipeline.MediaPacket{SessionID: "router-sess", SSRC: 0xCAFEBABE}
	queue <- pkt
	select {
	case got := <-sess.PacketQueue:
		if got.SSRC != 0xCAFEBABE {
			t.Errorf("SSRC mismatch in queue")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("packet not received from queue")
	}

	// Unknown SSRC should not route.
	_, _, ok = router.RouteBySSRC(0x00000001)
	if ok {
		t.Error("unknown SSRC: want ok=false, got true")
	}
}

// TestManagerRouter_RouteByAddr verifies addr-based routing in ManagerRouter.
func TestManagerRouter_RouteByAddr(t *testing.T) {
	mgr := session.NewManager(session.ManagerConfig{
		MaxSessions:     10,
		PacketQueueSize: 8,
		AudioQueueSize:  8,
		ResultQueueSize: 8,
		IdleTimeout:     time.Minute,
		GCInterval:      time.Minute,
	})
	_, err := mgr.Create(session.SessionConfig{
		ID:         "addr-sess",
		SourceType: "raw_rtp",
		Codec:      "PCMU",
		SampleRate: 8000,
		Channels:   1,
		RemoteAddr: "10.0.0.5:5004",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer mgr.Close("addr-sess")

	router := NewManagerRouter(mgr)

	_, info, ok := router.RouteByAddr("10.0.0.5:5004")
	if !ok {
		t.Fatal("RouteByAddr: want ok=true, got false")
	}
	if info.SessionID != "addr-sess" {
		t.Errorf("SessionID: want addr-sess, got %s", info.SessionID)
	}

	_, _, ok = router.RouteByAddr("0.0.0.0:1234")
	if ok {
		t.Error("unknown addr: want ok=false, got true")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := [20]byte{}
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}
