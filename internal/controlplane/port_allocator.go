package controlplane

import (
	"errors"
	"fmt"
	"sync"
)

// ErrNoPortsAvailable is returned when the port pool is exhausted.
var ErrNoPortsAvailable = errors.New("controlplane: no UDP ports available")

// PortAllocator manages a pool of UDP ports for per-session Raw RTP listeners (§5.4).
//
// Ports are handed out from a free-list (stack) for O(1) Acquire and Release.
// Goroutine-safe. Implements rawrtp.PortReleaser.
type PortAllocator struct {
	mu    sync.Mutex
	free  []int
	total int
}

// NewPortAllocator creates a PortAllocator covering [portStart, portEnd] inclusive.
func NewPortAllocator(portStart, portEnd int) (*PortAllocator, error) {
	if portStart <= 0 || portEnd < portStart {
		return nil, fmt.Errorf("controlplane: invalid port range [%d, %d]", portStart, portEnd)
	}
	free := make([]int, 0, portEnd-portStart+1)
	for p := portStart; p <= portEnd; p++ {
		free = append(free, p)
	}
	return &PortAllocator{free: free, total: len(free)}, nil
}

// NewEvenPortAllocator creates a PortAllocator covering only even ports in [portStart, portEnd]
// (theo media_ai). RFC 3550 §11: RTP dùng cổng chẵn, RTCP = RTP+1, nên cổng lẻ kế tiếp
// không bao giờ được cấp cho cuộc gọi khác. portStart phải chẵn.
func NewEvenPortAllocator(portStart, portEnd int) (*PortAllocator, error) {
	if portStart <= 0 || portEnd < portStart {
		return nil, fmt.Errorf("controlplane: invalid port range [%d, %d]", portStart, portEnd)
	}
	if portStart%2 != 0 {
		return nil, fmt.Errorf("controlplane: portStart %d must be even (RFC 3550 RTP convention)", portStart)
	}
	free := make([]int, 0, (portEnd-portStart)/2+1)
	for p := portStart; p <= portEnd; p += 2 {
		free = append(free, p)
	}
	return &PortAllocator{free: free, total: len(free)}, nil
}

// Acquire takes a port from the pool. Returns ErrNoPortsAvailable if exhausted.
func (a *PortAllocator) Acquire() (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.free) == 0 {
		return 0, ErrNoPortsAvailable
	}
	n := len(a.free) - 1
	port := a.free[n]
	a.free = a.free[:n]
	return port, nil
}

// Release returns a port to the pool. Satisfies rawrtp.PortReleaser.
func (a *PortAllocator) Release(port int) {
	a.mu.Lock()
	a.free = append(a.free, port)
	a.mu.Unlock()
}

// Available returns the number of free ports.
func (a *PortAllocator) Available() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.free)
}

// Total returns the total number of ports in the range (free + in-use).
func (a *PortAllocator) Total() int { return a.total }

// UsagePct returns the fraction of ports in use (0.0–1.0).
func (a *PortAllocator) UsagePct() float64 {
	if a.total == 0 {
		return 0
	}
	a.mu.Lock()
	used := a.total - len(a.free)
	a.mu.Unlock()
	return float64(used) / float64(a.total)
}
