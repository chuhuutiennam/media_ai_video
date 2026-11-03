package codec

import "errors"

// ErrAMRNotAvailable được trả về khi AMR decoder chưa được link vào binary.
//
// AMR-NB và AMR-WB là các codec ACELP; không có pure-Go implementation đủ hiệu năng.
// Để enable, dùng một trong hai cách:
//
// Option A — CGO binding với opencore-amr (khuyến nghị cho production):
//
//	go get github.com/pion/mediadevices  (chứa AMR codec wrapper)
//	apt-get install libopencore-amrnb-dev libopencore-amrwb-dev
//	Thay amrDecoder.Decode() bằng gọi opencore-amr C API qua CGO.
//
// Option B — Pure Go (chưa có implementation hoàn chỉnh):
//
//	Tham khảo 3GPP TS 26.090 (AMR-NB) và TS 26.190 (AMR-WB) để implement ACELP.
var ErrAMRNotAvailable = errors.New("amr decoder not available: link opencore-amr via CGO (see amr.go)")

// amrNBFrameBytes là kích thước payload (bytes) của mỗi AMR-NB frame type (FT).
// Nguồn: RFC 4867 Table 1 — octet-aligned mode.
//
//	FT 0-7 = speech frames (4.75–12.2 kbps)
//	FT 8   = SID (Silence Insertion Descriptor)
//	FT 9-14 = reserved/future use (0 bytes)
//	FT 15  = NO_DATA (0 bytes)
var amrNBFrameBytes = [16]int{12, 13, 15, 17, 19, 20, 26, 31, 5, 0, 0, 0, 0, 0, 0, 0}

// amrWBFrameBytes là kích thước payload (bytes) của mỗi AMR-WB frame type (FT).
// Nguồn: RFC 4867 Table 2 — octet-aligned mode.
//
//	FT 0-8  = speech frames (6.60–23.85 kbps)
//	FT 9    = SID
//	FT 10-13 = future use (0 bytes)
//	FT 14   = SPEECH_LOST (0 bytes)
//	FT 15   = NO_DATA (0 bytes)
var amrWBFrameBytes = [16]int{17, 23, 32, 36, 40, 46, 50, 58, 60, 5, 0, 0, 0, 0, 0, 0}

// amrVariant phân biệt AMR-NB và AMR-WB.
type amrVariant uint8

const (
	amrNB amrVariant = iota // AMR Narrowband — 8 kHz, 20 ms → 160 samples/frame
	amrWB                   // AMR Wideband   — 16 kHz, 20 ms → 320 samples/frame
)

// amrDecoder implement Decoder cho AMR-NB và AMR-WB (RFC 4867, octet-aligned mode).
//
// RTP payload structure (octet-aligned, §4.4.1):
//
//	byte 0:    CMR (4 bits) | padding (4 bits)
//	byte 1..N: TOC entries — F(1)|FT(4)|Q(1)|P(2); F=1 nếu còn TOC entry tiếp theo
//	bytes N+1: frame payloads, kích thước theo amrNBFrameBytes / amrWBFrameBytes [FT]
//
// SDP a=fmtp: octet-align=1 (VoLTE mặc định dùng chế độ này).
type amrDecoder struct {
	variant    amrVariant
	sampleRate int
	channels   int
}

func newAMRDecoder(variant amrVariant, sampleRate, channels int) (Decoder, error) {
	if variant == amrWB {
		return newAMRWBDecoder(sampleRate, channels)
	}
	return &amrDecoder{variant: variant, sampleRate: sampleRate, channels: channels}, nil
}

func (d *amrDecoder) SampleRate() int { return d.sampleRate }
func (d *amrDecoder) Channels() int   { return d.channels }

// Decode parse payload header nhưng không thể giải mã ACELP frames
// khi chưa link opencore-amr. Trả về ErrAMRNotAvailable.
func (d *amrDecoder) Decode(_ []byte) ([]int16, error) {
	return nil, ErrAMRNotAvailable
}

// AMRFrameInfo mô tả một frame được parse từ octet-aligned AMR payload.
// Dùng để inspect payload mà không cần decode thành PCM.
type AMRFrameInfo struct {
	FT      uint8  // Frame Type (0-15)
	Quality bool   // Q bit: true = good frame, false = bad/erased
	Payload []byte // compressed frame bytes (0 nếu FT == NO_DATA hoặc SPEECH_LOST)
}

// ParseAMRFrames parse octet-aligned AMR payload (RFC 4867 §4.4.1).
// Trả về danh sách frame info. Không cần opencore-amr.
//
// Lỗi khi:
//   - payload quá ngắn để chứa CMR + ít nhất một TOC entry
//   - payload bị cắt cụt (thiếu bytes so với kích thước frame khai báo trong FT)
func ParseAMRFrames(payload []byte, variant amrVariant) ([]AMRFrameInfo, error) {
	if len(payload) < 2 {
		return nil, ErrAMRNotAvailable // thiếu CMR + TOC
	}

	// Parse TOC entries (bắt đầu từ byte 1, mỗi entry 1 byte).
	var toc []AMRFrameInfo
	i := 1
	for i < len(payload) {
		entry := payload[i]
		i++
		ft := (entry >> 3) & 0x0F
		q := (entry & 0x04) != 0
		toc = append(toc, AMRFrameInfo{FT: ft, Quality: q})
		if entry&0x80 == 0 { // F bit = 0 → last TOC entry
			break
		}
	}

	// Điền payload bytes cho từng frame.
	frameTable := &amrNBFrameBytes
	if variant == amrWB {
		frameTable = &amrWBFrameBytes
	}
	for j := range toc {
		size := frameTable[toc[j].FT]
		if i+size > len(payload) {
			return toc[:j], ErrAMRNotAvailable
		}
		toc[j].Payload = payload[i : i+size]
		i += size
	}
	return toc, nil
}
