package webrtc

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
)

// mockDCRelayer implements dcRelayer for unit tests.
type mockDCRelayer struct {
	mu      sync.Mutex
	state   pion.DataChannelState
	sent    [][]byte
	sendErr error
	onMsg   func(pion.DataChannelMessage)
}

func (m *mockDCRelayer) ReadyState() pion.DataChannelState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}
func (m *mockDCRelayer) Send(data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sendErr != nil {
		return m.sendErr
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	m.sent = append(m.sent, cp)
	return nil
}
func (m *mockDCRelayer) OnMessage(f func(pion.DataChannelMessage)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onMsg = f
}
func (m *mockDCRelayer) trigger(data []byte) {
	m.mu.Lock()
	f := m.onMsg
	m.mu.Unlock()
	if f != nil {
		f(pion.DataChannelMessage{Data: data})
	}
}
func (m *mockDCRelayer) receivedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}
func (m *mockDCRelayer) lastReceived() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sent) == 0 {
		return nil
	}
	return m.sent[len(m.sent)-1]
}

// startUDPServer starts a test UDP server on a random port and returns
// the server connection, its address, and received datagrams via a channel.
func startUDPServer(t *testing.T) (*net.UDPConn, *net.UDPAddr, <-chan []byte) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("startUDPServer: %v", err)
	}
	addr := conn.LocalAddr().(*net.UDPAddr)
	ch := make(chan []byte, 16)
	go func() {
		buf := make([]byte, 65535)
		for {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			data := make([]byte, n)
			copy(data, buf[:n])
			ch <- data
		}
	}()
	return conn, addr, ch
}

// ── LocalPort ─────────────────────────────────────────────────────────────────

func TestUDPProxy_LocalPort_NonZero(t *testing.T) {
	srv, srvAddr, _ := startUDPServer(t)
	defer srv.Close()

	p, err := newUDPProxy("sess-1", srvAddr.String())
	if err != nil {
		t.Fatalf("newUDPProxy: %v", err)
	}
	defer p.conn.Close()

	if p.LocalPort() == 0 {
		t.Error("LocalPort should be non-zero after successful creation")
	}
}

// ── UE → App Server (wire / OnMessage) ────────────────────────────────────────

func TestUDPProxy_Forward_UE_To_AppServer(t *testing.T) {
	srv, srvAddr, received := startUDPServer(t)
	defer srv.Close()

	p, err := newUDPProxy("sess-2", srvAddr.String())
	if err != nil {
		t.Fatalf("newUDPProxy: %v", err)
	}
	defer p.conn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dc := &mockDCRelayer{state: pion.DataChannelStateOpen}
	p.wire(ctx, dc)

	payload := []byte("hello from UE")
	dc.trigger(payload)

	select {
	case got := <-received:
		if string(got) != string(payload) {
			t.Errorf("app server got %q, want %q", got, payload)
		}
	case <-time.After(2 * time.Second):
		t.Error("app server did not receive the forwarded datagram")
	}
}

func TestUDPProxy_Forward_CtxCancel_StopsForwarding(t *testing.T) {
	srv, srvAddr, received := startUDPServer(t)
	defer srv.Close()

	p, err := newUDPProxy("sess-3", srvAddr.String())
	if err != nil {
		t.Fatalf("newUDPProxy: %v", err)
	}
	defer p.conn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	dc := &mockDCRelayer{state: pion.DataChannelStateOpen}
	p.wire(ctx, dc)

	cancel() // cancel before triggering

	dc.trigger([]byte("should be dropped"))

	select {
	case <-received:
		t.Error("datagram should not have been forwarded after ctx cancel")
	case <-time.After(100 * time.Millisecond):
		// expected: nothing received
	}
}

// ── App Server → UE (run) ─────────────────────────────────────────────────────

func TestUDPProxy_Relay_AppServer_To_UE(t *testing.T) {
	srv, srvAddr, _ := startUDPServer(t)
	defer srv.Close()

	p, err := newUDPProxy("sess-4", srvAddr.String())
	if err != nil {
		t.Fatalf("newUDPProxy: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dc := &mockDCRelayer{state: pion.DataChannelStateOpen}
	done := make(chan struct{})
	go func() {
		p.run(ctx, dc)
		close(done)
	}()

	// Send a UDP datagram from "app server" to the proxy's local port.
	proxyAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: p.LocalPort()}
	sender, err := net.DialUDP("udp", nil, proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer sender.Close()

	want := []byte("transcript from AI")
	if _, err := sender.Write(want); err != nil {
		t.Fatalf("send to proxy: %v", err)
	}

	// Wait until DC.Send is called.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if dc.receivedCount() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	got := dc.lastReceived()
	if string(got) != string(want) {
		t.Errorf("DC.Send got %q, want %q", got, want)
	}
}

func TestUDPProxy_Relay_DropWhenDCNotOpen(t *testing.T) {
	srv, srvAddr, _ := startUDPServer(t)
	defer srv.Close()

	p, err := newUDPProxy("sess-5", srvAddr.String())
	if err != nil {
		t.Fatalf("newUDPProxy: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dc := &mockDCRelayer{state: pion.DataChannelStateConnecting} // NOT open
	go p.run(ctx, dc)

	proxyAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: p.LocalPort()}
	sender, err := net.DialUDP("udp", nil, proxyAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer sender.Close()
	_, _ = sender.Write([]byte("should be dropped"))

	time.Sleep(100 * time.Millisecond)
	if n := dc.receivedCount(); n != 0 {
		t.Errorf("expected 0 DC.Send calls when DC not open, got %d", n)
	}
}

func TestUDPProxy_Run_ExitsOnCtxCancel(t *testing.T) {
	srv, srvAddr, _ := startUDPServer(t)
	defer srv.Close()

	p, err := newUDPProxy("sess-6", srvAddr.String())
	if err != nil {
		t.Fatalf("newUDPProxy: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	dc := &mockDCRelayer{state: pion.DataChannelStateOpen}

	done := make(chan struct{})
	go func() {
		p.run(ctx, dc)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// expected
	case <-time.After(2 * time.Second):
		t.Error("run goroutine did not exit after ctx cancel")
	}
}

// ── Binary payload integrity ───────────────────────────────────────────────────

func TestUDPProxy_BinaryPayload_RoundTrip(t *testing.T) {
	srv, srvAddr, received := startUDPServer(t)
	defer srv.Close()

	p, err := newUDPProxy("sess-7", srvAddr.String())
	if err != nil {
		t.Fatalf("newUDPProxy: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dc := &mockDCRelayer{state: pion.DataChannelStateOpen}
	p.wire(ctx, dc)

	// Binary payload including null bytes.
	payload := []byte{0x00, 0x01, 0x02, 0xFF, 0xFE, 0x00}
	dc.trigger(payload)

	select {
	case got := <-received:
		if len(got) != len(payload) {
			t.Errorf("len mismatch: got %d want %d", len(got), len(payload))
		}
		for i := range payload {
			if got[i] != payload[i] {
				t.Errorf("byte %d: got 0x%02X want 0x%02X", i, got[i], payload[i])
			}
		}
	case <-time.After(2 * time.Second):
		t.Error("app server did not receive binary datagram")
	}
}
