package controlplane

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// ErrNoPortsAvailable is returned when the port pool is exhausted.
var ErrNoPortsAvailable = errors.New("controlplane: no UDP ports available")

// PortAllocator manages a pool of UDP ports for per-session Raw RTP listeners (§5.4).
//
// Cổng trống là hàng đợi FIFO: Acquire lấy cổng trả về lâu nhất, Release xếp cổng vào cuối.
// Cổng vừa trả chỉ được cấp lại sau reuseDelay (SetReuseDelay), để RTP còn đến của cuộc gọi cũ
// (MF chưa gỡ context) không lọt vào cuộc gọi mới. Trước đây là stack: cổng vừa trả được cấp lại
// ngay cho cuộc gọi kế tiếp.
// Goroutine-safe. Implements rawrtp.PortReleaser.
type PortAllocator struct {
	mu         sync.Mutex
	free       []freePort // FIFO; readyAt không giảm dần theo thứ tự (reuseDelay cố định)
	total      int
	reuseDelay time.Duration
	now        func() time.Time
}

type freePort struct {
	port    int
	readyAt time.Time // zero = cấp được ngay
}

func newPortAllocator(ports []int) *PortAllocator {
	free := make([]freePort, len(ports))
	for i, p := range ports {
		free[i] = freePort{port: p}
	}
	return &PortAllocator{free: free, total: len(free), now: time.Now}
}

// NewPortAllocator creates a PortAllocator covering [portStart, portEnd] inclusive.
func NewPortAllocator(portStart, portEnd int) (*PortAllocator, error) {
	if portStart <= 0 || portEnd < portStart {
		return nil, fmt.Errorf("controlplane: invalid port range [%d, %d]", portStart, portEnd)
	}
	ports := make([]int, 0, portEnd-portStart+1)
	for p := portStart; p <= portEnd; p++ {
		ports = append(ports, p)
	}
	return newPortAllocator(ports), nil
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
	ports := make([]int, 0, (portEnd-portStart)/2+1)
	for p := portStart; p <= portEnd; p += 2 {
		ports = append(ports, p)
	}
	return newPortAllocator(ports), nil
}

// SetReuseDelay đặt thời gian tối thiểu từ lúc trả cổng tới lúc cổng được cấp lại (rtp.port_reuse_delay_ms).
// 0 = cấp lại ngay khi tới lượt (vẫn theo thứ tự FIFO). Gọi khi khởi tạo, trước khi phục vụ.
func (a *PortAllocator) SetReuseDelay(d time.Duration) {
	a.mu.Lock()
	a.reuseDelay = max(d, 0)
	a.mu.Unlock()
}

// Acquire lấy cổng trả về lâu nhất đã hết thời gian chờ. Trả ErrNoPortsAvailable khi không còn
// cổng trống hoặc mọi cổng trống đều đang chờ.
func (a *PortAllocator) Acquire() (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.free) == 0 || a.free[0].readyAt.After(a.now()) {
		return 0, ErrNoPortsAvailable
	}
	port := a.free[0].port
	a.free = a.free[1:]
	return port, nil
}

// Release returns a port to the pool (cuối hàng đợi, cấp lại sau reuseDelay). Satisfies rawrtp.PortReleaser.
func (a *PortAllocator) Release(port int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	fp := freePort{port: port}
	if a.reuseDelay > 0 {
		fp.readyAt = a.now().Add(a.reuseDelay)
	}
	a.free = append(a.free, fp)
}

// availableLocked: số cổng trống cấp được ngay. readyAt không giảm theo thứ tự hàng đợi nên tìm
// nhị phân cổng đầu tiên còn đang chờ. Gọi dưới a.mu.
func (a *PortAllocator) availableLocked() int {
	now := a.now()
	return sort.Search(len(a.free), func(i int) bool { return a.free[i].readyAt.After(now) })
}

// Available returns the number of free ports that can be acquired now (không tính cổng đang chờ).
func (a *PortAllocator) Available() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.availableLocked()
}

// Cooling trả số cổng đã trả nhưng chưa hết thời gian chờ cấp lại.
func (a *PortAllocator) Cooling() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.free) - a.availableLocked()
}

// InUse trả số cổng đang được cấp cho session (không tính cổng đang chờ).
func (a *PortAllocator) InUse() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.total - len(a.free)
}

// Total returns the total number of ports in the range (free + in-use).
func (a *PortAllocator) Total() int { return a.total }

// UsagePct returns the fraction of ports in use (0.0–1.0).
func (a *PortAllocator) UsagePct() float64 {
	if a.total == 0 {
		return 0
	}
	return float64(a.InUse()) / float64(a.total)
}
