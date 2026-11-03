package controlplane

import (
	"sync"
	"testing"
)

func TestNewPortAllocator_ValidRange(t *testing.T) {
	a, err := NewPortAllocator(40000, 40009)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Total() != 10 {
		t.Errorf("Total: want 10, got %d", a.Total())
	}
	if a.Available() != 10 {
		t.Errorf("Available: want 10, got %d", a.Available())
	}
}

func TestNewPortAllocator_InvalidRange(t *testing.T) {
	cases := [][2]int{{0, 100}, {40000, 39999}, {-1, 100}}
	for _, c := range cases {
		if _, err := NewPortAllocator(c[0], c[1]); err == nil {
			t.Errorf("want error for range [%d,%d], got nil", c[0], c[1])
		}
	}
}

func TestPortAllocator_AcquireRelease(t *testing.T) {
	a, _ := NewPortAllocator(40000, 40002)

	p1, err := a.Acquire()
	if err != nil {
		t.Fatalf("Acquire 1: %v", err)
	}
	p2, _ := a.Acquire()
	p3, _ := a.Acquire()

	if a.Available() != 0 {
		t.Errorf("Available after 3 acquires: want 0, got %d", a.Available())
	}

	_, err = a.Acquire()
	if err != ErrNoPortsAvailable {
		t.Errorf("want ErrNoPortsAvailable, got %v", err)
	}

	a.Release(p2)
	if a.Available() != 1 {
		t.Errorf("Available after release: want 1, got %d", a.Available())
	}
	p4, err := a.Acquire()
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	if p4 != p2 {
		t.Errorf("expected re-acquired port %d, got %d", p2, p4)
	}

	_ = p1
	_ = p3
}

func TestPortAllocator_UsagePct(t *testing.T) {
	a, _ := NewPortAllocator(40000, 40003) // 4 ports

	if pct := a.UsagePct(); pct != 0.0 {
		t.Errorf("initial UsagePct: want 0.0, got %f", pct)
	}

	a.Acquire()
	a.Acquire()
	if pct := a.UsagePct(); pct != 0.5 {
		t.Errorf("after 2/4 acquire: want 0.5, got %f", pct)
	}

	a.Acquire()
	a.Acquire()
	if pct := a.UsagePct(); pct != 1.0 {
		t.Errorf("fully used: want 1.0, got %f", pct)
	}
}

func TestPortAllocator_Concurrency(t *testing.T) {
	const total = 200
	a, _ := NewPortAllocator(40000, 40000+total-1)

	var wg sync.WaitGroup
	ports := make(chan int, total)

	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := a.Acquire()
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			ports <- p
		}()
	}
	wg.Wait()
	close(ports)

	if a.Available() != 0 {
		t.Errorf("Available after all acquires: want 0, got %d", a.Available())
	}

	for p := range ports {
		wg.Add(1)
		go func(port int) {
			defer wg.Done()
			a.Release(port)
		}(p)
	}
	wg.Wait()

	if a.Available() != total {
		t.Errorf("Available after all releases: want %d, got %d", total, a.Available())
	}
}

func TestPortAllocator_NoDuplicatePorts(t *testing.T) {
	const total = 50
	a, _ := NewPortAllocator(50000, 50000+total-1)

	seen := make(map[int]bool, total)
	for i := 0; i < total; i++ {
		p, err := a.Acquire()
		if err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
		if seen[p] {
			t.Errorf("duplicate port: %d", p)
		}
		seen[p] = true
	}
}
