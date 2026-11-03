package codec

import (
	"testing"
)

// --- G.711 known-value tests (theo ITU-T G.711 reference) ---
//
// PCMU (µ-law):
//   wire 0xFF → 0     (positive silence / idle code)
//   wire 0x7F → 0     (negative silence)
//   wire 0x00 → +32124 (maximum positive amplitude)
//   wire 0x80 → -32124 (maximum negative amplitude)
//
// PCMA (A-law) — ITU-T G.711, step-size formula differs from µ-law:
//   wire 0xD5 → +8    (positive silence: exp=0, mantissa=0 → 0×16+8=8)
//   wire 0x55 → -8    (negative silence)
//   wire 0xAA → +32256 (maximum positive: exp=7, mantissa=15 → (15×16+264)<<6=32256)
//   wire 0x2A → -32256 (maximum negative)

func TestDecodeUlaw_KnownValues(t *testing.T) {
	cases := []struct {
		wire byte
		want int16
	}{
		{0xFF, 0},     // positive silence
		{0x7F, 0},     // negative silence
		{0x00, 32124}, // max positive
		{0x80, -32124}, // max negative
	}
	for _, c := range cases {
		got := decodeUlaw(c.wire)
		if got != c.want {
			t.Errorf("decodeUlaw(0x%02X) = %d, want %d", c.wire, got, c.want)
		}
	}
}

func TestDecodeAlaw_KnownValues(t *testing.T) {
	cases := []struct {
		wire byte
		want int16
	}{
		{0xD5, 8},      // positive silence (exp=0, mantissa=0 → 8)
		{0x55, -8},     // negative silence
		{0xAA, 32256},  // max positive (exp=7, mantissa=15 → (240+264)<<6=32256)
		{0x2A, -32256}, // max negative
	}
	for _, c := range cases {
		got := decodeAlaw(c.wire)
		if got != c.want {
			t.Errorf("decodeAlaw(0x%02X) = %d, want %d", c.wire, got, c.want)
		}
	}
}

// TestLookupTableMatchesFormula đảm bảo lookup table khớp với hàm decode gốc.
func TestLookupTableMatchesFormula(t *testing.T) {
	for i := 0; i < 256; i++ {
		b := byte(i)
		if got, want := ulawTable[b], decodeUlaw(b); got != want {
			t.Errorf("ulawTable[%d] = %d, decodeUlaw = %d", i, got, want)
		}
		if got, want := alawTable[b], decodeAlaw(b); got != want {
			t.Errorf("alawTable[%d] = %d, decodeAlaw = %d", i, got, want)
		}
	}
}

// --- Decoder interface tests ---

func TestNew_PCMU(t *testing.T) {
	d, err := New("PCMU", 8000, 1)
	if err != nil {
		t.Fatalf("New PCMU: %v", err)
	}
	if d.SampleRate() != 8000 {
		t.Errorf("SampleRate = %d, want 8000", d.SampleRate())
	}
	if d.Channels() != 1 {
		t.Errorf("Channels = %d, want 1", d.Channels())
	}
}

func TestNew_PCMA(t *testing.T) {
	d, err := New("PCMA", 8000, 1)
	if err != nil {
		t.Fatalf("New PCMA: %v", err)
	}
	if d.SampleRate() != 8000 || d.Channels() != 1 {
		t.Errorf("wrong SampleRate/Channels")
	}
}

func TestNew_DefaultSampleRate(t *testing.T) {
	d, err := New("PCMU", 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.SampleRate() != 8000 {
		t.Errorf("default SampleRate = %d, want 8000", d.SampleRate())
	}
	if d.Channels() != 1 {
		t.Errorf("default Channels = %d, want 1", d.Channels())
	}
}

func TestNew_Unsupported(t *testing.T) {
	_, err := New("G729", 8000, 1)
	if err == nil {
		t.Fatal("expected error for unsupported codec")
	}
}

func TestNew_CaseInsensitive(t *testing.T) {
	for _, codec := range []string{"pcmu", "Pcmu", "PCMU", "pcma", "PCMA", "opus", "OPUS"} {
		_, err := New(codec, 0, 0)
		if err != nil {
			t.Errorf("New(%q): unexpected error: %v", codec, err)
		}
	}
}

func TestDecode_PCMU_Silence(t *testing.T) {
	d, _ := New("PCMU", 8000, 1)
	// 20ms silence frame @ 8kHz = 160 bytes, all 0xFF (µ-law silence).
	payload := make([]byte, 160)
	for i := range payload {
		payload[i] = 0xFF
	}
	pcm, err := d.Decode(payload)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(pcm) != 160 {
		t.Fatalf("len(pcm) = %d, want 160", len(pcm))
	}
	for i, s := range pcm {
		if s != 0 {
			t.Errorf("pcm[%d] = %d, want 0 (silence)", i, s)
			break
		}
	}
}

func TestDecode_PCMA_Silence(t *testing.T) {
	d, _ := New("PCMA", 8000, 1)
	// A-law positive silence = 0xD5 → decodes to +8 (smallest quantization step).
	// µ-law silence (0xFF) decodes to exactly 0; A-law silence does not.
	payload := make([]byte, 160)
	for i := range payload {
		payload[i] = 0xD5
	}
	pcm, err := d.Decode(payload)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	for i, s := range pcm {
		if s != 8 {
			t.Errorf("pcm[%d] = %d, want 8 (A-law silence)", i, s)
			break
		}
	}
}

func TestDecode_PCMU_MaxAmplitude(t *testing.T) {
	d, _ := New("PCMU", 8000, 1)
	// 0x00 = max positive, 0x80 = max negative.
	payload := []byte{0x00, 0x80}
	pcm, err := d.Decode(payload)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if pcm[0] != 32124 {
		t.Errorf("pcm[0] = %d, want 32124 (max positive)", pcm[0])
	}
	if pcm[1] != -32124 {
		t.Errorf("pcm[1] = %d, want -32124 (max negative)", pcm[1])
	}
}

func TestDecode_EmptyPayload(t *testing.T) {
	d, _ := New("PCMU", 8000, 1)
	pcm, err := d.Decode([]byte{})
	if err != nil {
		t.Fatalf("Decode empty: %v", err)
	}
	if len(pcm) != 0 {
		t.Errorf("len(pcm) = %d, want 0", len(pcm))
	}
}

// TestDecode_Opus_MalformedPayload verifies that a malformed Opus payload
// returns a non-nil error. {0x01, 0x02} is a code-1 packet (2 equal-size frames)
// with an odd-length payload, which pion/opus rejects.
func TestDecode_Opus_MalformedPayload(t *testing.T) {
	d, err := New("opus", 48000, 1)
	if err != nil {
		t.Fatalf("New opus: %v", err)
	}
	_, err = d.Decode([]byte{0x01, 0x02})
	if err == nil {
		t.Error("expected error for malformed Opus payload, got nil")
	}
}

// TestDecode_Opus_SilenceFrame verifies that a DTX silence frame (single zero byte)
// decodes without error now that pion/opus is wired in.
func TestDecode_Opus_SilenceFrame(t *testing.T) {
	d, err := New("opus", 48000, 1)
	if err != nil {
		t.Fatalf("New opus: %v", err)
	}
	// 0x00 = code-0 frame with empty payload = DTX / comfort noise.
	_, err = d.Decode([]byte{0x00})
	if err != nil {
		t.Errorf("unexpected error for silence frame: %v", err)
	}
}

// TestDecode_OutputNotShared đảm bảo hai lần gọi Decode không share slice.
func TestDecode_OutputNotShared(t *testing.T) {
	d, _ := New("PCMU", 8000, 1)
	payload := []byte{0x00, 0x80}

	pcm1, _ := d.Decode(payload)
	pcm2, _ := d.Decode(payload)

	pcm1[0] = 9999
	if pcm2[0] == 9999 {
		t.Error("pcm1 and pcm2 share underlying array")
	}
}
