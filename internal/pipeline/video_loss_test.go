package pipeline

import (
	"context"
	"testing"
	"time"

	"media-ai-gateway/internal/codec/video"
)

// trackLoss: lần chờ IDR đầu stream không tính; một đợt chờ tính từ gói làm lộ khoảng trống
// tới gói hoàn tất IDR, theo giờ nhận gói; vào và ra trong cùng một gói tính 0 ms.
func TestTrackLoss_Episodes(t *testing.T) {
	sp := &sessionVideoPipeline{cfg: VideoSessionConfig{SessionID: "s1"}}
	steps := []struct {
		st      video.Stats
		atMs    int64
		waiting bool
		waitMs  uint64
	}{
		{video.Stats{WaitingForIDR: true}, 0, false, 0}, // đầu stream
		{video.Stats{}, 100, false, 0},                  // IDR đầu
		{video.Stats{PacketGaps: 1, WaitIDREpisodes: 1, WaitingForIDR: true}, 1000, true, 0},
		{video.Stats{PacketGaps: 1, WaitIDREpisodes: 1, WaitIDRDrops: 3, WaitingForIDR: true}, 1300, true, 0},
		{video.Stats{PacketGaps: 1, WaitIDREpisodes: 1, WaitIDRDrops: 7}, 1900, false, 900},
		{video.Stats{PacketGaps: 2, WaitIDREpisodes: 2, WaitIDRDrops: 7}, 2500, false, 900}, // gap + IDR cùng gói
	}
	for i, s := range steps {
		sp.trackLoss(s.st, s.atMs)
		if sp.waitingIDR.Load() != s.waiting || sp.waitMs.Load() != s.waitMs {
			t.Fatalf("bước %d: waiting=%v waitMs=%d, muốn %v/%d", i, sp.waitingIDR.Load(), sp.waitMs.Load(), s.waiting, s.waitMs)
		}
	}
	if sp.packetGaps.Load() != 2 || sp.waitEpisodes.Load() != 2 || sp.waitDrops.Load() != 7 {
		t.Fatalf("gaps=%d episodes=%d drops=%d, muốn 2/2/7", sp.packetGaps.Load(), sp.waitEpisodes.Load(), sp.waitDrops.Load())
	}
}

// Qua pool với decoder thật: IDR → mất seq 2 → P-slice bị bỏ → IDR. Số liệu C++ lên tới pool.
func TestVideoWorkerPool_PacketLossMetrics(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := NewVideoWorkerPool(VideoWorkerPoolConfig{Workers: 1, QueueSize: 8})
	if err := pool.RegisterSession(ctx, VideoSessionConfig{SessionID: "loss"}, make(chan VideoFrame, 1)); err != nil {
		t.Fatal(err)
	}
	defer pool.UnregisterSession("loss")
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()

	idr, slice := []byte{0x65, 0x88, 0x84, 0x00}, []byte{0x41, 0x9a, 0x00, 0x00}
	submitAndWait := func(n uint64, seq uint16, ts uint32, payload []byte, atMs int64) VideoStats {
		t.Helper()
		if err := pool.Submit(ctx, VideoJob{SessionID: "loss", Packet: MediaPacket{
			Sequence: seq, Timestamp: ts, Marker: true, Payload: payload, ReceivedAtMs: atMs,
		}}); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(time.Second)
		for pool.Stats().Processed != n && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		return pool.Stats()
	}
	submitAndWait(1, 1, 9000, idr, 1000)
	st := submitAndWait(2, 3, 18000, slice, 1100) // seq 2 mất
	if st.PacketGaps != 1 || st.WaitIDREpisodes != 1 || st.WaitIDRDroppedFrames != 1 || pool.WaitingIDRSessions() != 1 {
		t.Fatalf("sau khi mất gói: %+v waiting=%d", st, pool.WaitingIDRSessions())
	}
	st = submitAndWait(3, 4, 27000, idr, 1350)
	if st.WaitIDRMsTotal != 250 || pool.WaitingIDRSessions() != 0 || st.WaitIDREpisodes != 1 {
		t.Fatalf("sau IDR: %+v waiting=%d", st, pool.WaitingIDRSessions())
	}
	cancel()
	<-done
}
