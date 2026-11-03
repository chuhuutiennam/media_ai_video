package webrtc

import (
	"context"
	"net"

	pion "github.com/pion/webrtc/v4"
	"go.uber.org/zap"
)

const udpProxyMTU = 65535

// dcRelayer abstracts *pion.DataChannel for testability.
type dcRelayer interface {
	ReadyState() pion.DataChannelState
	Send(data []byte) error
	OnMessage(f func(pion.DataChannelMessage))
}

// udpProxy implements AC.6 UDP Proxy Mode (Figure AC.6-2):
//
//	UE → WebRTC DataChannel (SCTP/DTLS) → gateway UDP socket → DC App Server
//	DC App Server → UDP → gateway UDP socket → WebRTC DataChannel → UE
//
// The gateway binds a local UDP port (OS-assigned). The App Server must be
// configured to send responses to gateway_ip:LocalPort(). This port is returned
// in the AnswerResponse.udp_proxy_port field so the client (or orchestration
// layer) can communicate it to the App Server.
//
// Unlike HTTP Proxy Mode (Mode 2.1), the payload is forwarded as raw bytes
// without interpretation — matching the "transparent proxying" requirement of
// AC.6 UDP Proxy Mode.
type udpProxy struct {
	sessID  string
	appAddr *net.UDPAddr
	conn    *net.UDPConn // local UDP socket; closed when ctx is cancelled
}

// newUDPProxy creates a udpProxy that will relay data to appServerAddr.
// A local UDP socket is bound immediately (OS assigns an ephemeral port).
func newUDPProxy(sessID, appServerAddr string) (*udpProxy, error) {
	appAddr, err := net.ResolveUDPAddr("udp", appServerAddr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		return nil, err
	}
	return &udpProxy{sessID: sessID, appAddr: appAddr, conn: conn}, nil
}

// LocalPort returns the OS-assigned UDP port this proxy listens on.
// Expose this to the DC App Server so it can send response datagrams here.
func (p *udpProxy) LocalPort() int {
	return p.conn.LocalAddr().(*net.UDPAddr).Port
}

// wire hooks dc.OnMessage so that every DataChannel message from the UE is
// forwarded as a UDP datagram to the DC App Server (UE → App Server direction).
func (p *udpProxy) wire(ctx context.Context, dc dcRelayer) {
	dc.OnMessage(func(msg pion.DataChannelMessage) {
		if ctx.Err() != nil {
			return
		}
		if _, err := p.conn.WriteToUDP(msg.Data, p.appAddr); err != nil {
			zap.L().Warn("udp proxy: forward to app server failed",
				zap.String("session_id", p.sessID), zap.Error(err))
		}
	})
}

// run reads UDP datagrams arriving from the DC App Server and sends them back
// to the UE via the DataChannel (App Server → UE direction). Blocks until ctx
// is cancelled or the local socket is closed.
func (p *udpProxy) run(ctx context.Context, dc dcRelayer) {
	// Close the socket when the session ends so ReadFromUDP unblocks.
	go func() {
		<-ctx.Done()
		_ = p.conn.Close()
	}()

	buf := make([]byte, udpProxyMTU)
	for {
		n, _, err := p.conn.ReadFromUDP(buf)
		if err != nil {
			return // socket closed — normal exit on ctx cancel
		}
		if dc.ReadyState() != pion.DataChannelStateOpen {
			continue // DataChannel not ready yet; drop this datagram
		}
		data := make([]byte, n)
		copy(data, buf[:n])
		if err := dc.Send(data); err != nil {
			zap.L().Warn("udp proxy: relay to DataChannel failed",
				zap.String("session_id", p.sessID), zap.Error(err))
		}
	}
}
