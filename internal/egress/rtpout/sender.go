// Package rtpout packetizes raw encoded audio into RTP and sends it over an
// existing net.PacketConn back to a remote addr (MF RTP receive endpoint).
// The Sender reuses the per-session UDP socket already opened by rawrtp ingress
// so no additional file descriptor is needed.
package rtpout

import (
	"fmt"
	"net"
	"sync"

	pionrtp "github.com/pion/rtp"
)

// Sender sends RTP packets over conn to remoteAddr.
// All exported methods are safe for concurrent use.
type Sender struct {
	conn       net.PacketConn
	remoteAddr net.Addr
	mu         sync.Mutex
	ssrc       uint32
	seq        uint16
	ts         uint32
	tsIncr     uint32
	pt         uint8
}

// New creates a Sender.
//
//   - conn:       PacketConn already bound by the ingress listener (shared socket).
//   - remoteAddr: MF source address captured from the first inbound RTP packet.
//   - ssrc:       Egress SSRC — must differ from the inbound SSRC.
//   - pt:         RTP payload type matching the session codec (e.g. 0 for PCMU).
//   - sampleRate: Hz (e.g. 8000 for PCMU).
//   - ptimeMs:    Packet time in ms (e.g. 20); used to advance RTP timestamp per packet.
func New(conn net.PacketConn, remoteAddr net.Addr, ssrc uint32, pt uint8, sampleRate, ptimeMs int) *Sender {
	return &Sender{
		conn:       conn,
		remoteAddr: remoteAddr,
		ssrc:       ssrc,
		pt:         pt,
		tsIncr:     uint32(sampleRate * ptimeMs / 1000), //nolint:gosec
	}
}

// Send wraps payload in an RTP packet and writes it to remoteAddr.
func (s *Sender) Send(payload []byte) error { return s.send(payload, false) }

// SendMarked is like Send but sets the RTP Marker bit — use for the first
// packet of each talkspurt (after silence or stream start).
func (s *Sender) SendMarked(payload []byte) error { return s.send(payload, true) }

// AdvanceFrames advances the RTP timestamp by frames × tsIncr without sending
// a packet — used to account for silence gaps so MF's jitter buffer stays aligned.
func (s *Sender) AdvanceFrames(frames int) {
	if frames <= 0 {
		return
	}
	s.mu.Lock()
	s.ts += uint32(frames) * s.tsIncr //nolint:gosec
	s.mu.Unlock()
}

func (s *Sender) send(payload []byte, marker bool) error {
	s.mu.Lock()
	pkt := pionrtp.Packet{
		Header: pionrtp.Header{
			Version:        2,
			Marker:         marker,
			PayloadType:    s.pt,
			SequenceNumber: s.seq,
			Timestamp:      s.ts,
			SSRC:           s.ssrc,
		},
		Payload: payload,
	}
	s.seq++
	s.ts += s.tsIncr
	s.mu.Unlock()

	raw, err := pkt.Marshal()
	if err != nil {
		return fmt.Errorf("rtpout: marshal: %w", err)
	}
	if _, err = s.conn.WriteTo(raw, s.remoteAddr); err != nil {
		return fmt.Errorf("rtpout: write: %w", err)
	}
	return nil
}
