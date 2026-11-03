package rawrtp

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"media-ai-gateway/internal/pipeline"
)

type fakeRouter struct {
	mu        sync.RWMutex
	routes    map[uint32]fakeRoute
	addrRoutes map[string]fakeRoute
}

type fakeRoute struct {
	queue chan pipeline.MediaPacket
	info  CodecInfo
}

func newFakeRouter() *fakeRouter {
	return &fakeRouter{
		routes:     make(map[uint32]fakeRoute),
		addrRoutes: make(map[string]fakeRoute),
	}
}

func (r *fakeRouter) register(ssrc uint32, info CodecInfo, bufSize int) chan pipeline.MediaPacket {
	ch := make(chan pipeline.MediaPacket, bufSize)
	r.mu.Lock()
	r.routes[ssrc] = fakeRoute{queue: ch, info: info}
	r.mu.Unlock()
	return ch
}

func (r *fakeRouter) registerAddr(addr string, info CodecInfo, bufSize int) chan pipeline.MediaPacket {
	ch := make(chan pipeline.MediaPacket, bufSize)
	r.mu.Lock()
	r.addrRoutes[addr] = fakeRoute{queue: ch, info: info}
	r.mu.Unlock()
	return ch
}

func (r *fakeRouter) RouteBySSRC(ssrc uint32) (chan<- pipeline.MediaPacket, CodecInfo, bool) {
	r.mu.RLock()
	route, ok := r.routes[ssrc]
	r.mu.RUnlock()
	if !ok {
		return nil, CodecInfo{}, false
	}
	return route.queue, route.info, true
}

func (r *fakeRouter) RouteByAddr(addr string) (chan<- pipeline.MediaPacket, CodecInfo, bool) {
	r.mu.RLock()
	route, ok := r.addrRoutes[addr]
	r.mu.RUnlock()
	if !ok {
		return nil, CodecInfo{}, false
	}
	return route.queue, route.info, true
}

func startIngress(t *testing.T, router SessionRouter) (*Ingress, *net.UDPAddr, context.CancelFunc) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := conn.LocalAddr().(*net.UDPAddr)

	ingress := NewIngress(DefaultIngressConfig(), router)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := ingress.serve(ctx, conn); err != nil {
			t.Logf("serve error: %v", err)
		}
	}()
	return ingress, addr, cancel
}

func sendUDP(t *testing.T, addr *net.UDPAddr, b []byte) {
	t.Helper()
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write(b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestIngress_RoutesPacketToQueue(t *testing.T) {
	router := newFakeRouter()
	info := CodecInfo{SessionID: "s1", SourceType: "raw_rtp", Codec: "PCMU", SampleRate: 8000, Channels: 1}
	out := router.register(0x11223344, info, 8)

	ingress, addr, cancel := startIngress(t, router)
	defer cancel()

	pkt := buildRTP(42, 1000, 0x11223344, 0, false, []byte{0xFF, 0xFE})
	sendUDP(t, addr, pkt)

	select {
	case p := <-out:
		if p.SessionID != "s1" {
			t.Errorf("SessionID: want s1, got %s", p.SessionID)
		}
		if p.SSRC != 0x11223344 {
			t.Errorf("SSRC mismatch")
		}
		if p.Sequence != 42 {
			t.Errorf("Sequence: want 42, got %d", p.Sequence)
		}
		if p.Codec != "PCMU" {
			t.Errorf("Codec: want PCMU, got %s", p.Codec)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout: no packet in queue")
	}

	stats := ingress.Stats()
	if stats.Received != 1 || stats.Routed != 1 || stats.TotalDropped() != 0 {
		t.Errorf("stats: %+v", stats)
	}
}

func TestIngress_PayloadExtracted(t *testing.T) {
	router := newFakeRouter()
	info := CodecInfo{SessionID: "s1", SourceType: "raw_rtp", Codec: "PCMU", SampleRate: 8000, Channels: 1}
	out := router.register(0xAABBCCDD, info, 4)

	_, addr, cancel := startIngress(t, router)
	defer cancel()

	payload := []byte{0x01, 0x02, 0x03, 0x04}
	sendUDP(t, addr, buildRTP(1, 0, 0xAABBCCDD, 0, false, payload))

	select {
	case p := <-out:
		if string(p.Payload) != string(payload) {
			t.Errorf("payload mismatch: got %v, want %v", p.Payload, payload)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout")
	}
}

func TestIngress_MarkerBitForwarded(t *testing.T) {
	router := newFakeRouter()
	info := CodecInfo{SessionID: "s1", SourceType: "raw_rtp", Codec: "PCMU", SampleRate: 8000, Channels: 1}
	out := router.register(0x10, info, 4)

	_, addr, cancel := startIngress(t, router)
	defer cancel()

	sendUDP(t, addr, buildRTP(1, 0, 0x10, 0, true, []byte{0xFF}))

	select {
	case p := <-out:
		if !p.Marker {
			t.Error("Marker should be true")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout")
	}
}

func TestIngress_UnknownSSRC_Dropped(t *testing.T) {
	router := newFakeRouter()
	ingress, addr, cancel := startIngress(t, router)
	defer cancel()

	sendUDP(t, addr, buildRTP(1, 0, 0x99, 0, false, []byte{0xFF}))
	time.Sleep(30 * time.Millisecond)

	stats := ingress.Stats()
	if stats.DroppedUnknownSSRC != 1 || stats.Routed != 0 {
		t.Errorf("stats: %+v", stats)
	}
}

func TestIngress_ParseError_Dropped(t *testing.T) {
	router := newFakeRouter()
	ingress, addr, cancel := startIngress(t, router)
	defer cancel()

	sendUDP(t, addr, make([]byte, 10))
	time.Sleep(30 * time.Millisecond)

	stats := ingress.Stats()
	if stats.Received != 1 || stats.DroppedParseError != 1 {
		t.Errorf("stats: %+v", stats)
	}
}

func TestIngress_WrongVersion_Dropped(t *testing.T) {
	router := newFakeRouter()
	ingress, addr, cancel := startIngress(t, router)
	defer cancel()

	sendUDP(t, addr, make([]byte, 12))
	time.Sleep(30 * time.Millisecond)

	if ingress.Stats().DroppedParseError != 1 {
		t.Errorf("want DroppedParseError=1, got %d", ingress.Stats().DroppedParseError)
	}
}

func TestIngress_QueueFull_Dropped(t *testing.T) {
	router := newFakeRouter()
	info := CodecInfo{SessionID: "s1", SourceType: "raw_rtp", Codec: "PCMU", SampleRate: 8000, Channels: 1}
	router.register(0x20, info, 1)

	ingress, addr, cancel := startIngress(t, router)
	defer cancel()

	pkt := buildRTP(1, 0, 0x20, 0, false, []byte{0xFF})
	sendUDP(t, addr, pkt)
	sendUDP(t, addr, pkt)
	time.Sleep(50 * time.Millisecond)

	stats := ingress.Stats()
	if stats.Received != 2 || stats.Routed != 1 || stats.DroppedQueueFull != 1 {
		t.Errorf("stats: %+v", stats)
	}
}

func TestIngress_MultipleSSRCs_RoutedCorrectly(t *testing.T) {
	router := newFakeRouter()
	out1 := router.register(0x0001, CodecInfo{SessionID: "s1", SourceType: "raw_rtp", Codec: "PCMU", SampleRate: 8000, Channels: 1}, 8)
	out2 := router.register(0x0002, CodecInfo{SessionID: "s2", SourceType: "raw_rtp", Codec: "PCMA", SampleRate: 8000, Channels: 1}, 8)

	_, addr, cancel := startIngress(t, router)
	defer cancel()

	sendUDP(t, addr, buildRTP(1, 0, 0x0001, 0, false, []byte{0xAA}))
	sendUDP(t, addr, buildRTP(2, 0, 0x0002, 0, false, []byte{0xBB}))

	recv := func(ch chan pipeline.MediaPacket) pipeline.MediaPacket {
		select {
		case p := <-ch:
			return p
		case <-time.After(500 * time.Millisecond):
			t.Fatal("timeout")
			return pipeline.MediaPacket{}
		}
	}

	p1 := recv(out1)
	p2 := recv(out2)

	if p1.SessionID != "s1" || p2.SessionID != "s2" {
		t.Errorf("routing error: p1=%s p2=%s", p1.SessionID, p2.SessionID)
	}
}

func TestIngress_ReceivedAtMs_Set(t *testing.T) {
	router := newFakeRouter()
	info := CodecInfo{SessionID: "s1", SourceType: "raw_rtp", Codec: "PCMU", SampleRate: 8000, Channels: 1}
	out := router.register(0x30, info, 4)

	_, addr, cancel := startIngress(t, router)
	defer cancel()

	before := time.Now().UnixMilli()
	sendUDP(t, addr, buildRTP(1, 0, 0x30, 0, false, []byte{0xFF}))

	select {
	case p := <-out:
		after := time.Now().UnixMilli()
		if p.ReceivedAtMs < before || p.ReceivedAtMs > after+5 {
			t.Errorf("ReceivedAtMs=%d out of range [%d, %d]", p.ReceivedAtMs, before, after)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout")
	}
}

func TestIngress_Run_ExitsOnCtxCancel(t *testing.T) {
	router := newFakeRouter()
	ingress := NewIngress(IngressConfig{Addr: "127.0.0.1:0", MaxPacket: 1500}, router)

	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ingress.serve(ctx, conn) }()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve returned error: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("serve did not exit after ctx cancel")
	}
}

// TestIngress_AddrRouting_FallbackWhenSSRCUnknown verifies that when SSRC is not
// registered the ingress falls back to routing by source addr.
func TestIngress_AddrRouting_FallbackWhenSSRCUnknown(t *testing.T) {
	router := newFakeRouter()
	info := CodecInfo{SessionID: "addr-sess", SourceType: "raw_rtp", Codec: "PCMU", SampleRate: 8000, Channels: 1}

	ingress, srvAddr, cancel := startIngress(t, router)
	defer cancel()

	// Dial from a fixed local addr so we know the source port.
	localAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}
	conn, err := net.DialUDP("udp", localAddr, srvAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Register the route by the actual source addr the OS assigned.
	srcAddr := conn.LocalAddr().String()
	out := router.registerAddr(srcAddr, info, 8)

	// Send packet with an SSRC that is NOT in the SSRC index.
	pkt := buildRTP(1, 0, 0xDEAD, 0, false, []byte{0xFF})
	if _, err := conn.Write(pkt); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case p := <-out:
		if p.SessionID != "addr-sess" {
			t.Errorf("SessionID: want addr-sess, got %s", p.SessionID)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout: addr-based routing did not deliver packet")
	}

	if ingress.Stats().Routed != 1 {
		t.Errorf("want Routed=1, got %d", ingress.Stats().Routed)
	}
}

// TestIngress_AddrRouting_ZeroSSRC verifies that a packet with SSRC=0 skips the
// SSRC lookup and is routed by source addr.
func TestIngress_AddrRouting_ZeroSSRC(t *testing.T) {
	router := newFakeRouter()
	info := CodecInfo{SessionID: "zero-ssrc-sess", SourceType: "raw_rtp", Codec: "PCMU", SampleRate: 8000, Channels: 1}

	ingress, srvAddr, cancel := startIngress(t, router)
	defer cancel()

	localAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}
	conn, err := net.DialUDP("udp", localAddr, srvAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	srcAddr := conn.LocalAddr().String()
	out := router.registerAddr(srcAddr, info, 8)

	pkt := buildRTP(1, 0, 0, 0, false, []byte{0xFF}) // SSRC=0
	if _, err := conn.Write(pkt); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case p := <-out:
		if p.SessionID != "zero-ssrc-sess" {
			t.Errorf("SessionID: want zero-ssrc-sess, got %s", p.SessionID)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout: addr-based routing for SSRC=0 did not deliver packet")
	}

	_ = ingress.Stats()
}

func TestIngress_Stats_Accurate(t *testing.T) {
	router := newFakeRouter()
	info := CodecInfo{SessionID: "s1", SourceType: "raw_rtp", Codec: "PCMU", SampleRate: 8000, Channels: 1}
	out := router.register(0x40, info, 16)

	ingress, addr, cancel := startIngress(t, router)
	defer cancel()

	for i := 0; i < 3; i++ {
		sendUDP(t, addr, buildRTP(uint16(i), 0, 0x40, 0, false, []byte{0xFF}))
	}
	for i := 0; i < 2; i++ {
		sendUDP(t, addr, buildRTP(uint16(i), 0, 0xFF, 0, false, []byte{0xFF}))
	}
	sendUDP(t, addr, make([]byte, 5))

	deadline := time.Now().Add(500 * time.Millisecond)
	got := 0
	for got < 3 && time.Now().Before(deadline) {
		select {
		case <-out:
			got++
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	time.Sleep(20 * time.Millisecond)

	stats := ingress.Stats()
	if stats.Received != 6 || stats.Routed != 3 ||
		stats.DroppedUnknownSSRC != 2 || stats.DroppedParseError != 1 || stats.DroppedQueueFull != 0 {
		t.Errorf("stats: received=%d routed=%d parse=%d unknownSSRC=%d queueFull=%d",
			stats.Received, stats.Routed,
			stats.DroppedParseError, stats.DroppedUnknownSSRC, stats.DroppedQueueFull)
	}
}
