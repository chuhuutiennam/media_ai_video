package jitter

import (
	"testing"
	"time"

	"media-ai-gateway/internal/pipeline"
)

// Cấu hình như gateway-video-ims.yaml; video bật SkipGaps.
var (
	legacyCfg = Config{BufferMs: 80, MaxLateMs: 200, PacketTimeMs: 20}
	videoCfg  = Config{BufferMs: 80, MaxLateMs: 200, PacketTimeMs: 20, SkipGaps: true, MaxPackets: 1024}
)

func pktAt(seq uint16, at time.Time) pipeline.MediaPacket {
	return pipeline.MediaPacket{Sequence: seq, ReceivedAtMs: at.UnixMilli(), Payload: []byte{1}}
}

// clock mô phỏng ticker flush: mỗi tick 20ms, gom packet ra.
type clock struct {
	b   *Buffer
	now time.Time
	out chan pipeline.MediaPacket
	got []uint16
}

func newClock(cfg Config) *clock {
	return &clock{b: New(cfg), now: time.Unix(1_700_000_000, 0), out: make(chan pipeline.MediaPacket, 4096)}
}

func (c *clock) push(seqs ...uint16) {
	for _, s := range seqs {
		c.b.pushAt(pktAt(s, c.now), c.now)
	}
}

func (c *clock) advance(d time.Duration) { c.now = c.now.Add(d) }

// tickUntil chạy tick 20ms tới khi seq xuất hiện; trả về thời gian đã trôi (-1 nếu quá maxTicks).
func (c *clock) tickUntil(seq uint16, maxTicks int) time.Duration {
	start := c.now
	for i := 0; i < maxTicks; i++ {
		c.now = c.now.Add(20 * time.Millisecond)
		c.b.flush(c.now, c.out)
		for len(c.out) > 0 {
			p := <-c.out
			c.got = append(c.got, p.Sequence)
			if p.Sequence == seq {
				return c.now.Sub(start)
			}
		}
	}
	return -1
}

func seqRange(from, n int) []uint16 {
	out := make([]uint16, n)
	for i := range out {
		out[i] = uint16(from + i)
	}
	return out
}

// Mất 20 gói liên tiếp: video bỏ qua cả khoảng trống sau một cửa sổ; audio (cũ) chờ từng seq.
func TestSkipGaps_BurstLossSkippedOnce(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     Config
		atLeast time.Duration
		atMost  time.Duration
	}{
		{"video", videoCfg, 80 * time.Millisecond, 100 * time.Millisecond},
		{"legacy", legacyCfg, 19 * 80 * time.Millisecond, 5 * time.Second},
	} {
		c := newClock(tc.cfg)
		c.push(seqRange(0, 10)...)
		if c.tickUntil(9, 50) < 0 {
			t.Fatalf("%s: frame đầu không được nhả", tc.name)
		}
		c.advance(30 * time.Millisecond)
		c.push(seqRange(30, 30)...) // seq 10..29 mất
		took := c.tickUntil(30, 500)
		if took < tc.atLeast || took > tc.atMost {
			t.Fatalf("%s: seq 30 nhả sau %v, muốn trong [%v, %v]", tc.name, took, tc.atLeast, tc.atMost)
		}
		if st := c.b.Stats(); st.Lost != 20 {
			t.Fatalf("%s: lost=%d, muốn 20", tc.name, st.Lost)
		}
	}
}

// Nghỉ giữa 2 frame dài hơn BufferMs, gói đầu frame mới đến trễ 5ms: video vẫn nhận đúng thứ tự.
// Hành vi cũ khai báo mất ngay (gói đến muộn bị coi là trùng) — giữ để chứng minh audio không đổi.
func TestSkipGaps_ReorderAfterInterFramePause(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cfg      Config
		admitted bool
		want     []uint16
		lost     uint64
	}{
		{"video", videoCfg, true, []uint16{10, 11, 12}, 0},
		{"legacy", legacyCfg, false, []uint16{11, 12}, 1},
	} {
		c := newClock(tc.cfg)
		c.push(seqRange(0, 10)...)
		c.tickUntil(9, 50)
		c.got = nil
		c.advance(100 * time.Millisecond)
		c.push(11, 12)
		c.advance(2 * time.Millisecond)
		c.b.flush(c.now, c.out) // tick rơi giữa gói 11 và gói 10
		for len(c.out) > 0 {
			c.got = append(c.got, (<-c.out).Sequence)
		}
		c.advance(3 * time.Millisecond)
		admitted := c.b.pushAt(pktAt(10, c.now), c.now)
		c.tickUntil(12, 20)
		if admitted != tc.admitted || !equalSeqs(c.got, tc.want) || c.b.Stats().Lost != tc.lost {
			t.Fatalf("%s: admitted=%v got=%v lost=%d; muốn admitted=%v got=%v lost=%d",
				tc.name, admitted, c.got, c.b.Stats().Lost, tc.admitted, tc.want, tc.lost)
		}
	}
}

// Seq nhảy tiến 1000 (vd nguồn đổi SSRC): video qua được sau một cửa sổ, Lost đếm đúng số seq bỏ qua.
func TestSkipGaps_SequenceJumpAhead(t *testing.T) {
	c := newClock(videoCfg)
	c.push(seqRange(30000, 10)...)
	c.tickUntil(30009, 50)
	c.advance(40 * time.Millisecond)
	c.push(seqRange(31010, 10)...)
	if took := c.tickUntil(31019, 10); took < 0 || took > 100*time.Millisecond {
		t.Fatalf("seq mới nhả sau %v, muốn ≤ 100ms", took)
	}
	if st := c.b.Stats(); st.Lost != 1000 || st.Released != 20 {
		t.Fatalf("stats=%+v, muốn lost=1000 released=20", st)
	}
}

// MaxPackets giới hạn số gói chờ; gói vượt trần bị bỏ và đếm riêng (cả aggregate).
func TestMaxPackets_Overflow(t *testing.T) {
	var totals Counters
	cfg := videoCfg
	cfg.MaxPackets = 4
	b := NewWithCounters(cfg, &totals)
	now := time.Unix(1_700_000_000, 0)
	accepted := 0
	for s := uint16(0); s < 6; s++ {
		if b.pushAt(pktAt(s, now), now) {
			accepted++
		}
	}
	st := b.Stats()
	if accepted != 4 || st.OverflowDropped != 2 || st.Dropped != 2 || st.Received != 4 {
		t.Fatalf("accepted=%d stats=%+v", accepted, st)
	}
	if agg := totals.Stats(); agg.OverflowDropped != 2 || agg.Dropped != 2 {
		t.Fatalf("aggregate=%+v", agg)
	}
	// Không giới hạn khi MaxPackets = 0 (audio).
	unlimited := New(legacyCfg)
	for s := uint16(0); s < 2000; s++ {
		if !unlimited.pushAt(pktAt(s, now), now) {
			t.Fatalf("legacy từ chối gói %d", s)
		}
	}
}

func equalSeqs(a, b []uint16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
