package rawrtp

import (
	"fmt"
	"net"
	"sync/atomic"
	"time"

	pionrtp "github.com/pion/rtp"
	"go.uber.org/zap"

	"media-ai-gateway/internal/egress/rtpout"
	"media-ai-gateway/internal/pipeline"
	"media-ai-gateway/internal/session"
)

// PortReleaser is the interface satisfied by controlplane.PortAllocator.
// Defined here to avoid a circular import between rawrtp and controlplane.
type PortReleaser interface {
	Release(port int)
}

// StartSessionListener binds a dedicated UDP socket to bindIP:port and forwards
// parsed RTP packets to sess.PacketQueue until sess.Ctx is cancelled.
//
// The port is released via releaser.Release when the read goroutine exits.
// On bind failure the error is returned immediately; the caller must release
// the port manually.
//
// egressQueueFrames sets the egress pacer buffer size (frames). Suggested: 200
// (4s at 20ms/frame). The pacer paces AI audio_payload back to MF at real-time
// speed instead of sending a full utterance as a burst.
func StartSessionListener(
	sess *session.Session,
	bindIP string,
	port int,
	releaser PortReleaser,
	egressQueueFrames int,
) error {
	addr := fmt.Sprintf("%s:%d", bindIP, port)
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return fmt.Errorf("rtp: bind %s: %w", addr, err)
	}
	// Best-effort: set large socket receive buffer to absorb bursts.
	if uc, ok := conn.(*net.UDPConn); ok {
		_ = uc.SetReadBuffer(2 * 1024 * 1024)
	}

	// Unblock ReadFrom when the session ends.
	go func() {
		<-sess.Ctx.Done()
		conn.Close()
	}()

	go func() {
		defer releaser.Release(port)
		defer conn.Close()

		var firstPkt atomic.Bool
		buf := make([]byte, 1500)
		for {
			n, remoteAddr, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			var hdr pionrtp.Header
			offset, err := hdr.Unmarshal(buf[:n])
			if err != nil || offset >= n {
				continue
			}
			if firstPkt.CompareAndSwap(false, true) {
				zap.L().Debug("rtp: first packet received", zap.String("session_id", sess.ID), zap.Int("port", port), zap.Uint32("ssrc", hdr.SSRC), zap.Uint8("pt", hdr.PayloadType))
				// Tạo egress sender + pacer dùng lại cùng socket, gửi về remoteAddr (MF source).
				// SSRC egress = inbound SSRC XOR mask để đảm bảo khác.
				const ptimeMs = 20
				sender := rtpout.New(conn, remoteAddr, hdr.SSRC^0x80000000, hdr.PayloadType, sess.SampleRate, ptimeMs)
				pacer := rtpout.NewPacer(sender, sess.ID, ptimeMs, egressQueueFrames)
				go pacer.Run(sess.Ctx)
				sess.SetRTPEgress(func(payload []byte) error {
					if n := pacer.Push(payload); n > 0 {
						zap.L().Warn("rtp: egress pacer queue full, frames dropped",
							zap.String("session_id", sess.ID),
							zap.Int("dropped_frames", n))
					}
					return nil
				})
				zap.L().Debug("rtp: egress sender ready", zap.String("session_id", sess.ID), zap.String("remote_addr", remoteAddr.String()))
			}
			pkt := pipeline.MediaPacket{
				SessionID:    sess.ID,
				SourceType:   sess.SourceType,
				SSRC:         hdr.SSRC,
				PayloadType:  hdr.PayloadType,
				Sequence:     hdr.SequenceNumber,
				Timestamp:    hdr.Timestamp,
				Marker:       hdr.Marker,
				Payload:      append([]byte(nil), buf[offset:n]...),
				ReceivedAtMs: time.Now().UnixMilli(),
				Codec:        sess.Codec,
				SampleRate:   sess.SampleRate,
				Channels:     sess.Channels,
			}
			select {
			case sess.PacketQueue <- pkt:
			case <-sess.Ctx.Done():
				return
			default:
				sess.PacketQueueDropped.Add(1)
			}
		}
	}()

	return nil
}
