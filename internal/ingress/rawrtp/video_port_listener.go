package rawrtp

import (
	"fmt"
	"net"
	"strconv"
	"time"

	pionrtp "github.com/pion/rtp"
	"go.uber.org/zap"

	"media-ai-gateway/internal/pipeline"
	"media-ai-gateway/internal/session"
)

// StartVideoSessionListener bind một UDP socket riêng cho track VIDEO của
// session tại bindIP:port, parse RTP (H264) và forward vào
// sess.VideoPacketQueue cho tới khi sess.Ctx bị cancel.
func StartVideoSessionListener(
	sess *session.Session,
	bindIP string,
	port int,
	releaser PortReleaser,
) error {
	if !sess.VideoEnabled || sess.VideoPacketQueue == nil {
		return fmt.Errorf("rtp: video: session has no video queue")
	}
	addr := net.JoinHostPort(bindIP, strconv.Itoa(port))
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return fmt.Errorf("rtp: video: bind %s: %w", addr, err)
	}
	if uc, ok := conn.(*net.UDPConn); ok {
		_ = uc.SetReadBuffer(2 * 1024 * 1024)
	}

	go func() {
		<-sess.Ctx.Done()
		conn.Close()
	}()

	go func() {
		defer releaser.Release(port)
		defer conn.Close()

		loggedFirst := false
		buf := make([]byte, 65535)
		for {
			n, _, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			var packet pionrtp.Packet
			if err := packet.Unmarshal(buf[:n]); err != nil || packet.Version != 2 || len(packet.Payload) == 0 {
				continue
			}
			hdr := packet.Header
			if !loggedFirst {
				loggedFirst = true
				zap.L().Debug("rtp: video: first packet received",
					zap.String("session_id", sess.ID), zap.Int("port", port),
					zap.Uint32("ssrc", hdr.SSRC), zap.Uint8("pt", hdr.PayloadType))
			}
			pkt := pipeline.MediaPacket{
				SessionID:    sess.ID,
				SourceType:   sess.SourceType,
				SSRC:         hdr.SSRC,
				PayloadType:  hdr.PayloadType,
				Sequence:     hdr.SequenceNumber,
				Timestamp:    hdr.Timestamp,
				Marker:       hdr.Marker,
				Payload:      append([]byte(nil), packet.Payload...),
				ReceivedAtMs: time.Now().UnixMilli(),
				Codec:        sess.VideoCodec,
				SampleRate:   90000,
			}
			select {
			case sess.VideoPacketQueue <- pkt:
			case <-sess.Ctx.Done():
				return
			default:
				sess.VideoPacketQueueDropped.Add(1)
			}
		}
	}()

	return nil
}
