package codec

import "errors"

// ErrEVSNotAvailable được trả về khi EVS decoder chưa được link vào binary.
//
// EVS (Enhanced Voice Services, 3GPP TS 26.442/26.443) là codec thế hệ tiếp theo
// của AMR-WB, hỗ trợ 5.9–128 kbps và sample rate 8/16/32/48 kHz.
// VoLTE thường dùng 16 kHz primary mode hoặc AMR-WB IO mode (16 kHz).
//
// Để enable:
//
// Option A — 3GPP reference codec (C, phải link qua CGO):
//
//	Download từ https://www.3gpp.org/ftp/Specs/archive/26_series/26.443/
//	Build thư viện C, wrap bằng CGO, thay evsDecoder.Decode().
//
// Option B — Khi chỉ cần AMR-WB IO mode:
//
//	EVS ở AMR-WB IO mode có payload format giống AMR-WB (RFC 4867).
//	Dùng amrDecoder với variant=amrWB cho IO mode frames.
var ErrEVSNotAvailable = errors.New("evs decoder not available: link 3GPP EVS codec via CGO (see evs.go)")

// evsPrimaryFrameBytes ánh xạ bit rate (bps) → kích thước frame bytes
// trong EVS compact payload format (RFC 8130 §3.6).
//
// Nguồn: RFC 8130 Table A.1.
var evsPrimaryFrameBytes = map[int]int{
	2800:   7,   // EVS AMR-WB IO SID
	5900:   24,
	7200:   27,
	8000:   29,
	9600:   33,
	13200:  47,
	16400:  57,
	24400:  83,
	32000:  107,
	48000:  160,
	64000:  213,
	96000:  320,
	128000: 426,
}

// EVSPayloadFormat xác định sub-format của EVS RTP payload.
// RFC 8130 định nghĩa hai format: Compact và Header-Full.
type EVSPayloadFormat uint8

const (
	// EVSCompact: frame đơn, không có header — 1 byte đầu cho biết bit rate
	// nếu kích thước payload khớp đúng một entry trong evsPrimaryFrameBytes.
	EVSCompact EVSPayloadFormat = iota

	// EVSHeaderFull: multi-frame hoặc khi cần tường minh; header 1 byte (H=1)
	// chứa CMR (4 bits) và padding.
	EVSHeaderFull
)

// evsDecoder implement Decoder cho EVS (RFC 8130).
//
// EVS RTP payload có hai sub-format (§3.4):
//
//	Compact (thường dùng cho single-frame, điển hình trong VoLTE):
//	  - Payload size xác định bit rate (xem evsPrimaryFrameBytes).
//	  - Không có header byte.
//	  - Compact SID: 56 bits = 7 bytes.
//
//	Header-Full (dùng khi multi-frame hoặc AMR-WB IO mode):
//	  - Byte 0: H=1(1bit)|CMR(4bits)|padding(3bits) — khi H=1.
//	  - Byte 0: H=0(1bit)|ToC entries — khi H=0 (multi-frame).
//
// AMR-WB IO mode trong EVS có thể decode bằng amrDecoder nếu chỉ cần IO frames.
type evsDecoder struct {
	sampleRate int
	channels   int
}

func newEVSDecoder(sampleRate, channels int) (Decoder, error) {
	return &evsDecoder{sampleRate: sampleRate, channels: channels}, nil
}

func (d *evsDecoder) SampleRate() int { return d.sampleRate }
func (d *evsDecoder) Channels() int   { return d.channels }

// Decode parse EVS payload header nhưng không thể giải mã ACELP/TCX frames
// khi chưa link 3GPP EVS codec. Trả về ErrEVSNotAvailable.
func (d *evsDecoder) Decode(_ []byte) ([]int16, error) {
	return nil, ErrEVSNotAvailable
}

// DetectEVSFormat phát hiện sub-format của EVS payload (RFC 8130 §3.4).
// Compact khi không có byte nào set H bit (bit 7 của byte đầu = 0)
// VÀ kích thước payload khớp một mục trong evsPrimaryFrameBytes.
// Hữu ích khi parse payload mà không cần decode.
func DetectEVSFormat(payload []byte) EVSPayloadFormat {
	if len(payload) == 0 {
		return EVSCompact
	}
	// Header-Full: byte 0 bit 7 = 1
	if payload[0]&0x80 != 0 {
		return EVSHeaderFull
	}
	// Compact nếu kích thước khớp một bit rate đã biết
	for _, size := range evsPrimaryFrameBytes {
		if len(payload) == size {
			return EVSCompact
		}
	}
	return EVSHeaderFull
}

// EVSPrimaryBitrate trả về bit rate (bps) ứng với kích thước compact payload.
// Trả về 0 nếu không khớp bit rate nào trong evsPrimaryFrameBytes.
func EVSPrimaryBitrate(payloadLen int) int {
	for rate, size := range evsPrimaryFrameBytes {
		if payloadLen == size {
			return rate
		}
	}
	return 0
}
