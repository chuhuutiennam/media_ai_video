package codec

import (
	"errors"
	"testing"
)

// ── New() factory ──────────────────────────────────────────────────────────────

func TestNew_AMR_Defaults(t *testing.T) {
	d, err := New("AMR", 0, 0)
	if err != nil {
		t.Fatalf("New AMR: %v", err)
	}
	if d.SampleRate() != 8000 {
		t.Errorf("SampleRate: want 8000, got %d", d.SampleRate())
	}
	if d.Channels() != 1 {
		t.Errorf("Channels: want 1, got %d", d.Channels())
	}
}

func TestNew_AMRWB_Defaults(t *testing.T) {
	d, err := New("AMR-WB", 0, 0)
	if err != nil {
		t.Fatalf("New AMR-WB: %v", err)
	}
	if d.SampleRate() != 16000 {
		t.Errorf("SampleRate: want 16000, got %d", d.SampleRate())
	}
	if d.Channels() != 1 {
		t.Errorf("Channels: want 1, got %d", d.Channels())
	}
}

func TestNew_AMR_CaseInsensitive(t *testing.T) {
	for _, name := range []string{"amr", "Amr", "AMR", "amr-wb", "AMR-WB", "Amr-Wb"} {
		_, err := New(name, 0, 0)
		if err != nil {
			t.Errorf("New(%q): unexpected error: %v", name, err)
		}
	}
}

func TestNew_AMR_ExplicitSampleRate(t *testing.T) {
	d, err := New("AMR", 8000, 1)
	if err != nil {
		t.Fatalf("New AMR: %v", err)
	}
	if d.SampleRate() != 8000 || d.Channels() != 1 {
		t.Errorf("unexpected SampleRate=%d Channels=%d", d.SampleRate(), d.Channels())
	}
}

// ── Decode() returns sentinel error ───────────────────────────────────────────

func TestDecode_AMR_ReturnsError(t *testing.T) {
	d, _ := New("AMR", 0, 0)
	_, err := d.Decode([]byte{0x00, 0x3C, 0xAB})
	if !errors.Is(err, ErrAMRNotAvailable) {
		t.Errorf("want ErrAMRNotAvailable, got %v", err)
	}
}

func TestDecode_AMRWB_ReturnsError(t *testing.T) {
	d, _ := New("AMR-WB", 0, 0)
	_, err := d.Decode([]byte{0x00, 0x3C})
	if !errors.Is(err, ErrAMRNotAvailable) {
		t.Errorf("want ErrAMRNotAvailable, got %v", err)
	}
}

// ── ParseAMRFrames ────────────────────────────────────────────────────────────

// buildAMROctetAligned xây dựng octet-aligned AMR payload với một frame.
// byte 0 = CMR | padding, byte 1 = TOC (F=0 | FT | Q=1 | PP), bytes 2+ = payload.
func buildAMROctetAligned(ft uint8, frameBytes []byte) []byte {
	cmr := byte(0xF0)                     // CMR = 15 (no mode request), padding = 0
	toc := byte((ft << 3) | 0x04)        // F=0, FT, Q=1, P=0, P=0
	pkt := append([]byte{cmr, toc}, frameBytes...)
	return pkt
}

func TestParseAMRFrames_NB_SingleFrame(t *testing.T) {
	// FT=7 (12.2 kbps) = 31 bytes payload
	payload := buildAMROctetAligned(7, make([]byte, 31))

	frames, err := ParseAMRFrames(payload, amrNB)
	if err != nil {
		t.Fatalf("ParseAMRFrames: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("want 1 frame, got %d", len(frames))
	}
	if frames[0].FT != 7 {
		t.Errorf("FT: want 7, got %d", frames[0].FT)
	}
	if !frames[0].Quality {
		t.Error("Quality bit should be true (Q=1)")
	}
	if len(frames[0].Payload) != 31 {
		t.Errorf("Payload len: want 31, got %d", len(frames[0].Payload))
	}
}

func TestParseAMRFrames_WB_SingleFrame(t *testing.T) {
	// FT=8 (23.85 kbps) = 60 bytes payload
	payload := buildAMROctetAligned(8, make([]byte, 60))

	frames, err := ParseAMRFrames(payload, amrWB)
	if err != nil {
		t.Fatalf("ParseAMRFrames WB: %v", err)
	}
	if len(frames) != 1 || frames[0].FT != 8 || len(frames[0].Payload) != 60 {
		t.Errorf("unexpected frame: %+v", frames)
	}
}

func TestParseAMRFrames_NB_SIDFrame(t *testing.T) {
	// FT=8 = SID = 5 bytes
	payload := buildAMROctetAligned(8, make([]byte, 5))
	frames, err := ParseAMRFrames(payload, amrNB)
	if err != nil {
		t.Fatalf("ParseAMRFrames SID: %v", err)
	}
	if len(frames) != 1 || frames[0].FT != 8 || len(frames[0].Payload) != 5 {
		t.Errorf("unexpected SID frame: %+v", frames)
	}
}

func TestParseAMRFrames_NB_NoDataFrame(t *testing.T) {
	// FT=15 = NO_DATA = 0 bytes payload
	payload := buildAMROctetAligned(15, []byte{})
	frames, err := ParseAMRFrames(payload, amrNB)
	if err != nil {
		t.Fatalf("ParseAMRFrames NO_DATA: %v", err)
	}
	if len(frames) != 1 || frames[0].FT != 15 || len(frames[0].Payload) != 0 {
		t.Errorf("unexpected NO_DATA frame: %+v", frames)
	}
}

func TestParseAMRFrames_MultiFrame(t *testing.T) {
	// Hai frame: CMR, TOC(F=1, FT=4, Q=1), TOC(F=0, FT=7, Q=1), payload1(19), payload2(31)
	cmr := byte(0xF0)
	toc0 := byte(0x80 | (4 << 3) | 0x04) // F=1, FT=4, Q=1
	toc1 := byte(0x00 | (7 << 3) | 0x04) // F=0, FT=7, Q=1
	pkt := []byte{cmr, toc0, toc1}
	pkt = append(pkt, make([]byte, 19)...) // FT=4 payload
	pkt = append(pkt, make([]byte, 31)...) // FT=7 payload

	frames, err := ParseAMRFrames(pkt, amrNB)
	if err != nil {
		t.Fatalf("ParseAMRFrames multi: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("want 2 frames, got %d", len(frames))
	}
	if frames[0].FT != 4 || len(frames[0].Payload) != 19 {
		t.Errorf("frame0: FT=%d payloadLen=%d", frames[0].FT, len(frames[0].Payload))
	}
	if frames[1].FT != 7 || len(frames[1].Payload) != 31 {
		t.Errorf("frame1: FT=%d payloadLen=%d", frames[1].FT, len(frames[1].Payload))
	}
}

func TestParseAMRFrames_TooShort(t *testing.T) {
	_, err := ParseAMRFrames([]byte{0xF0}, amrNB) // chỉ CMR, thiếu TOC
	if !errors.Is(err, ErrAMRNotAvailable) {
		t.Errorf("want ErrAMRNotAvailable for too-short payload, got %v", err)
	}
}

func TestParseAMRFrames_TruncatedPayload(t *testing.T) {
	// FT=7 = 31 bytes nhưng chỉ cung cấp 10 bytes
	payload := buildAMROctetAligned(7, make([]byte, 10))
	_, err := ParseAMRFrames(payload, amrNB)
	if !errors.Is(err, ErrAMRNotAvailable) {
		t.Errorf("want ErrAMRNotAvailable for truncated payload, got %v", err)
	}
}

func TestParseAMRFrames_QualityBitFalse(t *testing.T) {
	// TOC với Q=0 (byte thứ 3: F=0, FT=7, Q=0)
	cmr := byte(0xF0)
	toc := byte((7 << 3) & ^byte(0x04)) // Q bit = 0
	pkt := append([]byte{cmr, toc}, make([]byte, 31)...)

	frames, err := ParseAMRFrames(pkt, amrNB)
	if err != nil {
		t.Fatalf("ParseAMRFrames Q=0: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("want 1 frame, got %d", len(frames))
	}
	if frames[0].Quality {
		t.Error("Quality should be false when Q bit = 0")
	}
}

func TestParseAMRFrames_EmptyPayload(t *testing.T) {
	_, err := ParseAMRFrames([]byte{}, amrNB)
	if !errors.Is(err, ErrAMRNotAvailable) {
		t.Errorf("want ErrAMRNotAvailable for empty payload, got %v", err)
	}
}

// TestParseAMRFrames_NB_AllSpeechFTs kiểm tra tất cả 8 speech frame types của AMR-NB.
func TestParseAMRFrames_NB_AllSpeechFTs(t *testing.T) {
	cases := []struct {
		ft   uint8
		size int
	}{
		{0, 12}, {1, 13}, {2, 15}, {3, 17},
		{4, 19}, {5, 20}, {6, 26}, {7, 31},
	}
	for _, c := range cases {
		payload := buildAMROctetAligned(c.ft, make([]byte, c.size))
		frames, err := ParseAMRFrames(payload, amrNB)
		if err != nil {
			t.Errorf("FT=%d: unexpected error: %v", c.ft, err)
			continue
		}
		if len(frames) != 1 {
			t.Errorf("FT=%d: want 1 frame, got %d", c.ft, len(frames))
			continue
		}
		if frames[0].FT != c.ft {
			t.Errorf("FT=%d: parsed FT=%d", c.ft, frames[0].FT)
		}
		if len(frames[0].Payload) != c.size {
			t.Errorf("FT=%d: payload len want %d, got %d", c.ft, c.size, len(frames[0].Payload))
		}
	}
}

// TestParseAMRFrames_WB_AllFTs kiểm tra tất cả frame types của AMR-WB (speech + SID + zero-byte).
func TestParseAMRFrames_WB_AllFTs(t *testing.T) {
	cases := []struct {
		ft   uint8
		size int
	}{
		{0, 17}, {1, 23}, {2, 32}, {3, 36},
		{4, 40}, {5, 46}, {6, 50}, {7, 58},
		{8, 60}, {9, 5}, // SID
		{14, 0}, // SPEECH_LOST
		{15, 0}, // NO_DATA
	}
	for _, c := range cases {
		payload := buildAMROctetAligned(c.ft, make([]byte, c.size))
		frames, err := ParseAMRFrames(payload, amrWB)
		if err != nil {
			t.Errorf("WB FT=%d: unexpected error: %v", c.ft, err)
			continue
		}
		if len(frames) != 1 {
			t.Errorf("WB FT=%d: want 1 frame, got %d", c.ft, len(frames))
			continue
		}
		if len(frames[0].Payload) != c.size {
			t.Errorf("WB FT=%d: payload len want %d, got %d", c.ft, c.size, len(frames[0].Payload))
		}
	}
}

// TestParseAMRFrames_NB_ReservedFTs kiểm tra FT 9-14 (reserved) có 0 bytes payload.
func TestParseAMRFrames_NB_ReservedFTs(t *testing.T) {
	for ft := uint8(9); ft <= 14; ft++ {
		payload := buildAMROctetAligned(ft, []byte{})
		frames, err := ParseAMRFrames(payload, amrNB)
		if err != nil {
			t.Errorf("NB reserved FT=%d: unexpected error: %v", ft, err)
			continue
		}
		if len(frames) != 1 || len(frames[0].Payload) != 0 {
			t.Errorf("NB reserved FT=%d: want 1 zero-byte frame, got %v", ft, frames)
		}
	}
}

// TestParseAMRFrames_PayloadSliceCorrect kiểm tra bytes của frame[0].Payload là đúng vị trí trong packet.
func TestParseAMRFrames_PayloadSliceCorrect(t *testing.T) {
	// FT=0 (4.75 kbps) = 12 bytes; điền giá trị nhận biết được
	data := make([]byte, 12)
	for i := range data {
		data[i] = byte(0xA0 + i)
	}
	payload := buildAMROctetAligned(0, data)

	frames, err := ParseAMRFrames(payload, amrNB)
	if err != nil {
		t.Fatalf("ParseAMRFrames: %v", err)
	}
	for i, b := range frames[0].Payload {
		if b != data[i] {
			t.Errorf("Payload[%d] = 0x%02X, want 0x%02X", i, b, data[i])
		}
	}
}

// TestParseAMRFrames_ThreeFrames kiểm tra parse 3 frame trong một packet.
func TestParseAMRFrames_ThreeFrames(t *testing.T) {
	cmr := byte(0xF0)
	toc0 := byte(0x80 | (7 << 3) | 0x04) // F=1, FT=7, Q=1 → 31 bytes
	toc1 := byte(0x80 | (8 << 3) | 0x04) // F=1, FT=8 (SID), Q=1 → 5 bytes
	toc2 := byte(0x00 | (15 << 3))        // F=0, FT=15 (NO_DATA), Q=0 → 0 bytes
	pkt := []byte{cmr, toc0, toc1, toc2}
	pkt = append(pkt, make([]byte, 31)...)
	pkt = append(pkt, make([]byte, 5)...)
	// NO_DATA: 0 bytes

	frames, err := ParseAMRFrames(pkt, amrNB)
	if err != nil {
		t.Fatalf("ParseAMRFrames 3 frames: %v", err)
	}
	if len(frames) != 3 {
		t.Fatalf("want 3 frames, got %d", len(frames))
	}
	if frames[0].FT != 7 || len(frames[0].Payload) != 31 {
		t.Errorf("frame0: FT=%d len=%d", frames[0].FT, len(frames[0].Payload))
	}
	if frames[1].FT != 8 || len(frames[1].Payload) != 5 {
		t.Errorf("frame1: FT=%d len=%d", frames[1].FT, len(frames[1].Payload))
	}
	if frames[2].FT != 15 || len(frames[2].Payload) != 0 {
		t.Errorf("frame2: FT=%d len=%d", frames[2].FT, len(frames[2].Payload))
	}
}

// ── Frame size tables ─────────────────────────────────────────────────────────

func TestAMRNBFrameBytes_MatchesRFC4867(t *testing.T) {
	// RFC 4867 Table 1: FT → bytes
	expected := map[int]int{
		0: 12, 1: 13, 2: 15, 3: 17,
		4: 19, 5: 20, 6: 26, 7: 31,
		8: 5, // SID
	}
	for ft, want := range expected {
		if got := amrNBFrameBytes[ft]; got != want {
			t.Errorf("amrNBFrameBytes[%d] = %d, want %d", ft, got, want)
		}
	}
}

func TestAMRWBFrameBytes_MatchesRFC4867(t *testing.T) {
	// RFC 4867 Table 2: FT → bytes
	expected := map[int]int{
		0: 17, 1: 23, 2: 32, 3: 36,
		4: 40, 5: 46, 6: 50, 7: 58,
		8: 60, 9: 5, // SID
	}
	for ft, want := range expected {
		if got := amrWBFrameBytes[ft]; got != want {
			t.Errorf("amrWBFrameBytes[%d] = %d, want %d", ft, got, want)
		}
	}
}
