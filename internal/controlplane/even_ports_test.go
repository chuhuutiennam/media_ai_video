package controlplane

import (
	"testing"
)

// Chỉ cổng chẵn được cấp; cổng lẻ (RTCP = RTP+1) không bao giờ thuộc cuộc gọi nào.
func TestNewEvenPortAllocator(t *testing.T) {
	if _, err := NewEvenPortAllocator(40001, 40010); err == nil {
		t.Fatal("odd portStart must be rejected")
	}
	if _, err := NewEvenPortAllocator(40010, 40000); err == nil {
		t.Fatal("inverted range must be rejected")
	}
	a, err := NewEvenPortAllocator(40000, 40009)
	if err != nil {
		t.Fatal(err)
	}
	if a.Total() != 5 {
		t.Fatalf("Total: want 5 even ports in 40000-40009, got %d", a.Total())
	}
	seen := map[int]bool{}
	for i := 0; i < 5; i++ {
		p, err := a.Acquire()
		if err != nil || p%2 != 0 || p < 40000 || p > 40008 || seen[p] {
			t.Fatalf("acquire %d: port %d, %v", i, p, err)
		}
		seen[p] = true
	}
	if _, err := a.Acquire(); err != ErrNoPortsAvailable {
		t.Fatalf("want ErrNoPortsAvailable, got %v", err)
	}
}

func TestNewServer_RTPEvenPorts(t *testing.T) {
	for _, tc := range []struct {
		even bool
		want int
	}{{false, 10}, {true, 5}} {
		cfg := DefaultServerConfig()
		cfg.RTPPortStart, cfg.RTPPortEnd, cfg.RTPEvenPorts = 40000, 40009, tc.even
		s := newTestServer(t, &fakeStarter{})
		s = NewServer(cfg, s.sessionMgr, s.coord, s.pool, s.aiMgr, s.dispatcher)
		if got := s.portAlloc.Total(); got != tc.want {
			t.Errorf("even=%v: ports %d, want %d", tc.even, got, tc.want)
		}
	}
}
