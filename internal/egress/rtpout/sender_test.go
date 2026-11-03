package rtpout

import (
	"net"
	"testing"

	pionrtp "github.com/pion/rtp"
)

// fakeConn captures WriteTo calls for inspection.
type fakeConn struct {
	net.PacketConn
	packets [][]byte
}

func (f *fakeConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	cp := make([]byte, len(p))
	copy(cp, p)
	f.packets = append(f.packets, cp)
	return len(p), nil
}

func newTestSender(fc *fakeConn, sampleRate, ptimeMs int) *Sender {
	return New(fc, &net.UDPAddr{}, 0xDEADBEEF, 0, sampleRate, ptimeMs)
}

func decodeRTP(t *testing.T, raw []byte) pionrtp.Packet {
	t.Helper()
	var pkt pionrtp.Packet
	if err := pkt.Unmarshal(raw); err != nil {
		t.Fatalf("unmarshal RTP: %v", err)
	}
	return pkt
}

func TestSend_NoMarkerBit(t *testing.T) {
	fc := &fakeConn{}
	s := newTestSender(fc, 8000, 20)
	if err := s.Send(make([]byte, 160)); err != nil {
		t.Fatal(err)
	}
	pkt := decodeRTP(t, fc.packets[0])
	if pkt.Marker {
		t.Error("Send: Marker bit should be false")
	}
}

func TestSendMarked_SetsBit(t *testing.T) {
	fc := &fakeConn{}
	s := newTestSender(fc, 8000, 20)
	if err := s.SendMarked(make([]byte, 160)); err != nil {
		t.Fatal(err)
	}
	pkt := decodeRTP(t, fc.packets[0])
	if !pkt.Marker {
		t.Error("SendMarked: Marker bit should be true")
	}
}

func TestSendMarked_DoesNotPersistMarker(t *testing.T) {
	fc := &fakeConn{}
	s := newTestSender(fc, 8000, 20)
	_ = s.SendMarked(make([]byte, 160))
	_ = s.Send(make([]byte, 160))
	pkt := decodeRTP(t, fc.packets[1])
	if pkt.Marker {
		t.Error("second packet should not have Marker bit")
	}
}

func TestAdvanceFrames_TimestampJump(t *testing.T) {
	fc := &fakeConn{}
	s := newTestSender(fc, 8000, 20) // tsIncr = 160

	_ = s.Send(make([]byte, 160)) // ts=0, after send ts=160
	s.AdvanceFrames(5)            // ts=160 + 5*160 = 960
	_ = s.Send(make([]byte, 160)) // ts=960, after send ts=1120

	pkt0 := decodeRTP(t, fc.packets[0])
	pkt1 := decodeRTP(t, fc.packets[1])
	if pkt0.Timestamp != 0 {
		t.Errorf("packet 0: ts = %d, want 0", pkt0.Timestamp)
	}
	if pkt1.Timestamp != 960 {
		t.Errorf("packet 1: ts = %d, want 960", pkt1.Timestamp)
	}
}

func TestAdvanceFrames_Zero(t *testing.T) {
	fc := &fakeConn{}
	s := newTestSender(fc, 8000, 20)
	_ = s.Send(make([]byte, 160)) // ts=0 → ts=160
	s.AdvanceFrames(0)
	s.AdvanceFrames(-1)
	_ = s.Send(make([]byte, 160)) // ts should still be 160
	pkt := decodeRTP(t, fc.packets[1])
	if pkt.Timestamp != 160 {
		t.Errorf("ts = %d, want 160 (AdvanceFrames(0) should be no-op)", pkt.Timestamp)
	}
}
