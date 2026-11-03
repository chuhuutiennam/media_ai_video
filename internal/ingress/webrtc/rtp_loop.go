package webrtc

import (
	"context"
	"time"

	pion "github.com/pion/webrtc/v4"

	"media-ai-gateway/internal/pipeline"
	"media-ai-gateway/internal/session"
)

// readRTPLoop reads RTP packets from a WebRTC audio track and pushes them as
// MediaPackets into sess.PacketQueue until the track closes or ctx is cancelled.
//
// Drop policy: non-blocking send — if the queue is full the packet is discarded,
// consistent with the Raw RTP ingress. The audio pipeline handles codec decoding
// (Opus payload bytes are forwarded as-is).
func readRTPLoop(ctx context.Context, track *pion.TrackRemote, sess *session.Session) {
	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			// io.EOF when the track is removed or PeerConnection closes.
			return
		}

		// Copy payload — pion may reuse the underlying buffer after ReadRTP returns.
		payload := make([]byte, len(pkt.Payload))
		copy(payload, pkt.Payload)

		mp := pipeline.MediaPacket{
			SessionID:    sess.ID,
			SourceType:   sess.SourceType,
			SSRC:         pkt.SSRC,
			PayloadType:  uint8(pkt.PayloadType),
			Sequence:     pkt.SequenceNumber,
			Timestamp:    pkt.Timestamp,
			Marker:       pkt.Marker,
			Payload:      payload,
			ReceivedAtMs: time.Now().UnixMilli(),
			Codec:        sess.Codec,
			SampleRate:   sess.SampleRate,
			Channels:     sess.Channels,
		}

		select {
		case sess.PacketQueue <- mp:
			sess.Touch()
		case <-ctx.Done():
			return
		default:
			// Queue full — drop packet.
		}
	}
}
