package pipeline

import (
	"context"
	"testing"
	"time"
)

func TestVideoDecodeErrorsSurviveSessionClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := NewVideoWorkerPool(VideoWorkerPoolConfig{Workers: 1, QueueSize: 8})
	if err := pool.RegisterSession(ctx, VideoSessionConfig{SessionID: "broken"}, make(chan VideoFrame, 1)); err != nil {
		t.Fatal(err)
	}
	defer pool.UnregisterSession("broken")
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()
	// A malformed STAP-A packet must be counted, even after its decoder closes.
	err := pool.Submit(ctx, VideoJob{SessionID: "broken", Packet: MediaPacket{
		Sequence: 1, Timestamp: 9000, Marker: true, Payload: []byte{24, 0, 20, 0x65},
	}})
	if err != nil {
		t.Fatal(err)
	}
	// An IDR without SPS/PPS reaches libavcodec, which records a decode error
	// while the RTP API itself returns success. That error must also be visible.
	if err := pool.Submit(ctx, VideoJob{SessionID: "broken", Packet: MediaPacket{
		Sequence: 2, Timestamp: 18000, Marker: true, Payload: []byte{0x65, 0x88, 0x84, 0x00},
	}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for pool.Stats().Processed != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	before := pool.Stats()
	if before.Processed != 2 || before.DecodeErrors < 2 {
		t.Fatalf("malformed H264 not accounted for: %+v", before)
	}
	pool.UnregisterSession("broken")
	if after := pool.Stats(); after != before {
		t.Fatalf("counters changed after close: %+v -> %+v", before, after)
	}
	cancel()
	<-done
}
