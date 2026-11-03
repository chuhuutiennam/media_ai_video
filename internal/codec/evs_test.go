package codec

import (
	"errors"
	"testing"
)

// ── New() factory ──────────────────────────────────────────────────────────────

func TestNew_EVS_Defaults(t *testing.T) {
	d, err := New("EVS", 0, 0)
	if err != nil {
		t.Fatalf("New EVS: %v", err)
	}
	if d.SampleRate() != 16000 {
		t.Errorf("SampleRate: want 16000, got %d", d.SampleRate())
	}
	if d.Channels() != 1 {
		t.Errorf("Channels: want 1, got %d", d.Channels())
	}
}

func TestNew_EVS_ExplicitSampleRate(t *testing.T) {
	for _, sr := range []int{8000, 16000, 32000, 48000} {
		d, err := New("EVS", sr, 1)
		if err != nil {
			t.Fatalf("New EVS %d Hz: %v", sr, err)
		}
		if d.SampleRate() != sr {
			t.Errorf("SampleRate: want %d, got %d", sr, d.SampleRate())
		}
	}
}

func TestNew_EVS_CaseInsensitive(t *testing.T) {
	for _, name := range []string{"evs", "Evs", "EVS"} {
		_, err := New(name, 0, 0)
		if err != nil {
			t.Errorf("New(%q): unexpected error: %v", name, err)
		}
	}
}

// ── Decode() returns sentinel error ───────────────────────────────────────────

func TestDecode_EVS_ReturnsError(t *testing.T) {
	d, _ := New("EVS", 0, 0)
	_, err := d.Decode([]byte{0x01, 0x02, 0x03})
	if !errors.Is(err, ErrEVSNotAvailable) {
		t.Errorf("want ErrEVSNotAvailable, got %v", err)
	}
}

func TestDecode_EVS_EmptyPayload(t *testing.T) {
	d, _ := New("EVS", 0, 0)
	_, err := d.Decode([]byte{})
	if !errors.Is(err, ErrEVSNotAvailable) {
		t.Errorf("want ErrEVSNotAvailable for empty payload, got %v", err)
	}
}

// ── DetectEVSFormat ───────────────────────────────────────────────────────────

func TestDetectEVSFormat_Compact_KnownSize(t *testing.T) {
	// 24 bytes = 5.9 kbps compact frame
	payload := make([]byte, 24)
	payload[0] = 0x00 // H bit = 0
	if f := DetectEVSFormat(payload); f != EVSCompact {
		t.Errorf("24-byte payload: want Compact, got %v", f)
	}
}

func TestDetectEVSFormat_HeaderFull_HBitSet(t *testing.T) {
	// byte 0 bit7 = 1 → Header-Full
	payload := make([]byte, 24)
	payload[0] = 0x80
	if f := DetectEVSFormat(payload); f != EVSHeaderFull {
		t.Errorf("H=1 payload: want HeaderFull, got %v", f)
	}
}

func TestDetectEVSFormat_HeaderFull_UnknownSize(t *testing.T) {
	// H=0 ma kích thước không khớp bất kỳ bit rate nào → Header-Full
	payload := make([]byte, 15)
	payload[0] = 0x00
	if f := DetectEVSFormat(payload); f != EVSHeaderFull {
		t.Errorf("unknown size payload: want HeaderFull, got %v", f)
	}
}

func TestDetectEVSFormat_Empty(t *testing.T) {
	// payload rỗng → Compact (không đủ data để xác định)
	if f := DetectEVSFormat([]byte{}); f != EVSCompact {
		t.Errorf("empty payload: want Compact, got %v", f)
	}
}

// ── EVSPrimaryBitrate ─────────────────────────────────────────────────────────

func TestEVSPrimaryBitrate_KnownSizes(t *testing.T) {
	cases := []struct {
		payloadLen int
		wantRate   int
	}{
		{7, 2800},   // SID / AMR-WB IO SID
		{24, 5900},
		{47, 13200},
		{83, 24400},
		{160, 48000},
		{213, 64000},
		{426, 128000},
	}
	for _, c := range cases {
		got := EVSPrimaryBitrate(c.payloadLen)
		if got != c.wantRate {
			t.Errorf("EVSPrimaryBitrate(%d) = %d, want %d", c.payloadLen, got, c.wantRate)
		}
	}
}

func TestEVSPrimaryBitrate_Unknown(t *testing.T) {
	// 15 bytes không khớp bit rate nào → trả về 0
	if got := EVSPrimaryBitrate(15); got != 0 {
		t.Errorf("EVSPrimaryBitrate(15) = %d, want 0", got)
	}
}

func TestEVSPrimaryBitrate_Zero(t *testing.T) {
	if got := EVSPrimaryBitrate(0); got != 0 {
		t.Errorf("EVSPrimaryBitrate(0) = %d, want 0", got)
	}
}

// TestDetectEVSFormat_Compact_AllKnownSizes kiểm tra tất cả 13 kích thước compact được nhận diện đúng.
func TestDetectEVSFormat_Compact_AllKnownSizes(t *testing.T) {
	knownSizes := []int{7, 24, 27, 29, 33, 47, 57, 83, 107, 160, 213, 320, 426}
	for _, size := range knownSizes {
		payload := make([]byte, size)
		payload[0] = 0x00 // H bit = 0
		if f := DetectEVSFormat(payload); f != EVSCompact {
			t.Errorf("size=%d: want Compact, got %v", size, f)
		}
	}
}

// TestDetectEVSFormat_HeaderFull_NearKnownSizes kiểm tra kích thước sát cạnh known size là HeaderFull.
func TestDetectEVSFormat_HeaderFull_NearKnownSizes(t *testing.T) {
	// 23 (not in table, nearest is 24) và 25 (not in table) phải là HeaderFull khi H=0
	for _, size := range []int{1, 23, 25, 28, 100, 200, 425, 427} {
		payload := make([]byte, size)
		payload[0] = 0x00
		if f := DetectEVSFormat(payload); f != EVSHeaderFull {
			t.Errorf("size=%d: want HeaderFull, got %v", size, f)
		}
	}
}

// TestEVSPrimaryBitrate_AllRates kiểm tra đầy đủ tất cả 13 bit rates trong bảng RFC 8130.
func TestEVSPrimaryBitrate_AllRates(t *testing.T) {
	cases := []struct {
		payloadLen int
		wantRate   int
	}{
		{7, 2800},
		{24, 5900},
		{27, 7200},
		{29, 8000},
		{33, 9600},
		{47, 13200},
		{57, 16400},
		{83, 24400},
		{107, 32000},
		{160, 48000},
		{213, 64000},
		{320, 96000},
		{426, 128000},
	}
	for _, c := range cases {
		got := EVSPrimaryBitrate(c.payloadLen)
		if got != c.wantRate {
			t.Errorf("EVSPrimaryBitrate(%d) = %d, want %d", c.payloadLen, got, c.wantRate)
		}
	}
}

// TestEVSPrimaryBitrate_Roundtrip kiểm tra mỗi size trong bảng → rate → size lại khớp.
func TestEVSPrimaryBitrate_Roundtrip(t *testing.T) {
	for rate, size := range evsPrimaryFrameBytes {
		gotRate := EVSPrimaryBitrate(size)
		if gotRate != rate {
			t.Errorf("roundtrip rate=%d: EVSPrimaryBitrate(%d) = %d", rate, size, gotRate)
		}
	}
}

// TestDetectEVSFormat_CompactAndBitrateConsistent kiểm tra payload compact size → Compact format.
func TestDetectEVSFormat_CompactAndBitrateConsistent(t *testing.T) {
	for _, size := range evsPrimaryFrameBytes {
		payload := make([]byte, size)
		// H bit = 0, size matches → Compact
		f := DetectEVSFormat(payload)
		if f != EVSCompact {
			t.Errorf("size=%d (known compact): want Compact, got %v", size, f)
		}
	}
}

// ── evsPrimaryFrameBytes table ────────────────────────────────────────────────

func TestEVSFrameBytes_MatchesRFC8130(t *testing.T) {
	// RFC 8130 Table A.1
	expected := map[int]int{
		2800:   7,
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
	for rate, want := range expected {
		got, ok := evsPrimaryFrameBytes[rate]
		if !ok {
			t.Errorf("evsPrimaryFrameBytes[%d] missing", rate)
			continue
		}
		if got != want {
			t.Errorf("evsPrimaryFrameBytes[%d] = %d, want %d", rate, got, want)
		}
	}
}
