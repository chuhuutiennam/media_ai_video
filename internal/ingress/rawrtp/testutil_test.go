package rawrtp

import "encoding/binary"

// buildRTP tạo một RTP datagram tối giản (V=2, no CSRC, no extension).
func buildRTP(seq uint16, ts, ssrc uint32, pt uint8, marker bool, payload []byte) []byte {
	b := make([]byte, 12+len(payload))
	b[0] = 0x80 // V=2, P=0, X=0, CC=0
	b[1] = pt & 0x7F
	if marker {
		b[1] |= 0x80
	}
	binary.BigEndian.PutUint16(b[2:], seq)
	binary.BigEndian.PutUint32(b[4:], ts)
	binary.BigEndian.PutUint32(b[8:], ssrc)
	copy(b[12:], payload)
	return b
}
