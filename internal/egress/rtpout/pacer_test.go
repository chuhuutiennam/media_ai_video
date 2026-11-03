package rtpout

import (
	"context"
	"testing"
	"time"
)

func newTestPacer(fc *fakeConn, ptimeMs, queueFrames int) *EgressPacer {
	s := newTestSender(fc, 8000, ptimeMs)
	return NewPacer(s, "test-session", ptimeMs, queueFrames)
}

func TestPush_AllOrNothingDrop(t *testing.T) {
	fc := &fakeConn{}
	p := newTestPacer(fc, 20, 3) // capacity = 3 frames

	// 3 frames fit exactly.
	if n := p.Push(make([]byte, 3*160)); n != 0 {
		t.Fatalf("expected 0 drops for exact capacity, got %d", n)
	}
	if len(p.frameQ) != 3 {
		t.Fatalf("expected 3 queued, got %d", len(p.frameQ))
	}

	// Queue full — all-or-nothing: 1 frame rejected, queue unchanged.
	if n := p.Push(make([]byte, 160)); n != 1 {
		t.Fatalf("expected 1 drop (queue full), got %d", n)
	}
	if len(p.frameQ) != 3 {
		t.Fatal("queue should still be 3 after all-or-nothing drop")
	}
}

func TestPush_PartialFitDropsAll(t *testing.T) {
	fc := &fakeConn{}
	p := newTestPacer(fc, 20, 2) // capacity = 2 frames

	// 1 frame fits first.
	p.Push(make([]byte, 160))

	// 2 more frames requested but only 1 slot free → all dropped.
	if n := p.Push(make([]byte, 2*160)); n != 2 {
		t.Fatalf("expected 2 drops, got %d", n)
	}
	if len(p.frameQ) != 1 {
		t.Fatalf("expected 1 queued (unchanged), got %d", len(p.frameQ))
	}
}

func TestPush_PadsRemainder(t *testing.T) {
	fc := &fakeConn{}
	p := newTestPacer(fc, 20, 10)

	// 160 + 80 bytes → 1 full frame + 1 padded frame.
	payload := make([]byte, 240)
	for i := range payload {
		payload[i] = 0xAB
	}
	if n := p.Push(payload); n != 0 {
		t.Fatalf("unexpected drop: %d", n)
	}
	if len(p.frameQ) != 2 {
		t.Fatalf("expected 2 frames, got %d", len(p.frameQ))
	}

	frame1 := <-p.frameQ
	frame2 := <-p.frameQ

	for i, b := range frame1 {
		if b != 0xAB {
			t.Errorf("frame1[%d] = 0x%02X, want 0xAB", i, b)
			break
		}
	}
	// frame2: first 80 bytes = 0xAB, remaining 80 bytes = 0xFF (µ-law silence)
	for i := 0; i < 80; i++ {
		if frame2[i] != 0xAB {
			t.Errorf("frame2[%d] = 0x%02X, want 0xAB", i, frame2[i])
			break
		}
	}
	for i := 80; i < 160; i++ {
		if frame2[i] != 0xFF {
			t.Errorf("frame2[%d] = 0x%02X, want 0xFF (silence pad)", i, frame2[i])
			break
		}
	}
}

func TestPush_EmptyPayload(t *testing.T) {
	fc := &fakeConn{}
	p := newTestPacer(fc, 20, 10)
	if n := p.Push(nil); n != 0 {
		t.Fatalf("expected 0 drops for nil payload, got %d", n)
	}
	if len(p.frameQ) != 0 {
		t.Fatal("expected empty queue")
	}
}

func TestPush_ShorterThanFrame(t *testing.T) {
	fc := &fakeConn{}
	p := newTestPacer(fc, 20, 10)
	// 80 bytes < 160 frame size → 1 padded frame
	if n := p.Push(make([]byte, 80)); n != 0 {
		t.Fatalf("expected 0 drops, got %d", n)
	}
	if len(p.frameQ) != 1 {
		t.Fatalf("expected 1 padded frame, got %d", len(p.frameQ))
	}
}

func TestPacer_Run_MarkerOnFirstPacket(t *testing.T) {
	fc := &fakeConn{}
	p := newTestPacer(fc, 20, 10)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Push(make([]byte, 160))
	time.Sleep(60 * time.Millisecond) // 3 ticks at 20ms
	cancel()

	if len(fc.packets) == 0 {
		t.Fatal("expected at least 1 packet")
	}
	pkt := decodeRTP(t, fc.packets[0])
	if !pkt.Marker {
		t.Error("first packet: Marker should be true")
	}
}

func TestPacer_Run_NoMarkerOnConsecutiveFrames(t *testing.T) {
	fc := &fakeConn{}
	p := newTestPacer(fc, 20, 20)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	// Push 3 consecutive frames — only first should have Marker.
	p.Push(make([]byte, 3*160))
	time.Sleep(100 * time.Millisecond) // 5 ticks — all 3 frames sent
	cancel()

	if len(fc.packets) < 3 {
		t.Fatalf("expected at least 3 packets, got %d", len(fc.packets))
	}
	for i, raw := range fc.packets[:3] {
		pkt := decodeRTP(t, raw)
		wantMarker := i == 0
		if pkt.Marker != wantMarker {
			t.Errorf("packet %d: Marker = %v, want %v", i, pkt.Marker, wantMarker)
		}
	}
}

func TestPacer_Run_MarkerAfterSilenceGap(t *testing.T) {
	fc := &fakeConn{}
	p := newTestPacer(fc, 20, 20)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	// First frame (Marker=true — first ever).
	p.Push(make([]byte, 160))
	time.Sleep(30 * time.Millisecond) // sent in 1 tick

	// Silence for ~2 ticks (40ms) → missingFrames ≥ 1 → next frame gets Marker.
	time.Sleep(45 * time.Millisecond)

	// Second frame — should get Marker=true after the gap.
	p.Push(make([]byte, 160))
	time.Sleep(30 * time.Millisecond)
	cancel()

	if len(fc.packets) < 2 {
		t.Fatalf("expected at least 2 packets, got %d", len(fc.packets))
	}
	first := decodeRTP(t, fc.packets[0])
	last := decodeRTP(t, fc.packets[len(fc.packets)-1])
	if !first.Marker {
		t.Error("first packet: Marker should be true")
	}
	if !last.Marker {
		t.Error("post-silence packet: Marker should be true")
	}
}

func TestPacer_Run_TimestampAdvancedOnSilence(t *testing.T) {
	fc := &fakeConn{}
	p := newTestPacer(fc, 20, 20)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	// First frame: ts = 0.
	p.Push(make([]byte, 160))
	time.Sleep(30 * time.Millisecond)

	// Silence ~2 ticks (40ms) → AdvanceFrames(~1) called.
	time.Sleep(45 * time.Millisecond)

	// Second frame: ts should be ≥ 320 (0 + 160 sent + ≥1×160 advanced + 160 this tick).
	p.Push(make([]byte, 160))
	time.Sleep(30 * time.Millisecond)
	cancel()

	if len(fc.packets) < 2 {
		t.Fatalf("expected at least 2 packets, got %d", len(fc.packets))
	}
	pkt0 := decodeRTP(t, fc.packets[0])
	pktN := decodeRTP(t, fc.packets[len(fc.packets)-1])

	// ts of second frame must be strictly greater than ts0 + tsIncr (accounting for advance).
	if pktN.Timestamp <= pkt0.Timestamp+160 {
		t.Errorf("post-silence ts = %d, want > %d (silence not advanced)", pktN.Timestamp, pkt0.Timestamp+160)
	}
}
