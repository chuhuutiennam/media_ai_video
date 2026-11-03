package rawrtp

import (
	"context"
	"net"
	"sync/atomic"
)

// IngressConfig cấu hình UDP listener cho Raw RTP ingress.
type IngressConfig struct {
	Addr            string // UDP bind address, e.g. ":5004"
	MaxPacket       int    // max datagram size; default 1500
	ReadBufferBytes int    // SO_RCVBUF size; default 2 MiB
}

// DefaultIngressConfig trả về cấu hình mặc định cho production.
func DefaultIngressConfig() IngressConfig {
	return IngressConfig{
		Addr:            ":5004",
		MaxPacket:       1500,
		ReadBufferBytes: 2 * 1024 * 1024,
	}
}

// IngressStats là snapshot counters của UDP ingress.
type IngressStats struct {
	Received            uint64 // tổng datagram nhận được
	Routed              uint64 // packet đưa vào queue thành công
	DroppedParseError   uint64 // malformed RTP header
	DroppedUnknownSSRC  uint64 // SSRC không khớp session nào
	DroppedQueueFull    uint64 // session PacketQueue đầy
}

// TotalDropped trả về tổng số packet bị drop vì bất kỳ lý do nào.
func (s IngressStats) TotalDropped() uint64 {
	return s.DroppedParseError + s.DroppedUnknownSSRC + s.DroppedQueueFull
}

// Ingress lắng nghe UDP, parse RTP, và route packet vào session PacketQueue.
//
// Dropped packet (parse error, unknown SSRC, queue full) được đếm nhưng không
// block receive loop — UDP socket luôn được drain liên tục.
type Ingress struct {
	cfg    IngressConfig
	router SessionRouter

	received           atomic.Uint64
	routed             atomic.Uint64
	droppedParse       atomic.Uint64
	droppedUnknownSSRC atomic.Uint64
	droppedQueueFull   atomic.Uint64
}

// NewIngress tạo Raw RTP ingress với router cho trước.
func NewIngress(cfg IngressConfig, router SessionRouter) *Ingress {
	if cfg.MaxPacket <= 0 {
		cfg.MaxPacket = 1500
	}
	if cfg.ReadBufferBytes <= 0 {
		cfg.ReadBufferBytes = 2 * 1024 * 1024
	}
	return &Ingress{cfg: cfg, router: router}
}

// Run bind UDP socket và xử lý datagram cho đến khi ctx bị cancel.
func (g *Ingress) Run(ctx context.Context) error {
	addr, err := net.ResolveUDPAddr("udp", g.cfg.Addr)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return err
	}
	return g.serve(ctx, conn)
}

// serve là core loop có thể test được — nhận conn đã bind sẵn.
func (g *Ingress) serve(ctx context.Context, conn *net.UDPConn) error {
	defer conn.Close()
	// Best-effort: kernel may cap this below the requested value on some systems.
	_ = conn.SetReadBuffer(g.cfg.ReadBufferBytes)
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	buf := make([]byte, g.cfg.MaxPacket)
	for {
		n, remoteAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		g.received.Add(1)
		addrStr := ""
		if remoteAddr != nil {
			addrStr = remoteAddr.String()
		}
		g.handle(buf[:n], addrStr)
	}
}

// Stats trả về snapshot counters tại thời điểm gọi.
func (g *Ingress) Stats() IngressStats {
	return IngressStats{
		Received:           g.received.Load(),
		Routed:             g.routed.Load(),
		DroppedParseError:  g.droppedParse.Load(),
		DroppedUnknownSSRC: g.droppedUnknownSSRC.Load(),
		DroppedQueueFull:   g.droppedQueueFull.Load(),
	}
}
