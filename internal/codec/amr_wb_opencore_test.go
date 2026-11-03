//go:build opencore_amrwb

package codec

import "testing"

// TOC byte layout (RFC 4867): bit7=F, bits6-3=FT, bit2=Q, bits1-0=P
// tocByte constructs a last-entry (F=0) TOC byte for a given FT and Q=1.
func tocByte(ft uint8) byte {
	return (ft << 3) | 0x04 // F=0, FT=ft, Q=1, P=00
}

func TestAMRWBDecoder_Init(t *testing.T) {
	d, err := newAMRWBDecoder(16000, 1)
	if err != nil {
		t.Fatalf("newAMRWBDecoder: %v", err)
	}
	if d.SampleRate() != 16000 {
		t.Errorf("SampleRate = %d, want 16000", d.SampleRate())
	}
	if d.Channels() != 1 {
		t.Errorf("Channels = %d, want 1", d.Channels())
	}
}

func TestAMRWBDecoder_ViaNew(t *testing.T) {
	d, err := New("AMR-WB", 16000, 1)
	if err != nil {
		t.Fatalf("New AMR-WB: %v", err)
	}
	if d.SampleRate() != 16000 || d.Channels() != 1 {
		t.Error("unexpected SampleRate/Channels from New(AMR-WB)")
	}
}

// TestAMRWBDecoder_NoDataFrame verifies FT=15 (NO_DATA) produces 0 samples.
func TestAMRWBDecoder_NoDataFrame(t *testing.T) {
	d, err := newAMRWBDecoder(16000, 1)
	if err != nil {
		t.Fatalf("newAMRWBDecoder: %v", err)
	}
	// CMR=0xF0, TOC=(F=0,FT=15,Q=1)=0x7C, no frame bytes
	payload := []byte{0xF0, tocByte(15)}
	pcm, err := d.Decode(payload)
	if err != nil {
		t.Fatalf("Decode NO_DATA: %v", err)
	}
	if len(pcm) != 0 {
		t.Errorf("len(pcm) = %d for NO_DATA, want 0", len(pcm))
	}
}

// TestAMRWBDecoder_SIDFrame verifies FT=9 (SID, 5 bytes) produces 320 samples.
func TestAMRWBDecoder_SIDFrame(t *testing.T) {
	d, err := newAMRWBDecoder(16000, 1)
	if err != nil {
		t.Fatalf("newAMRWBDecoder: %v", err)
	}
	payload := make([]byte, 2+5) // CMR + TOC + 5 SID bytes
	payload[0] = 0xF0
	payload[1] = tocByte(9) // FT=9
	pcm, err := d.Decode(payload)
	if err != nil {
		t.Fatalf("Decode SID: %v", err)
	}
	if len(pcm) != 320 {
		t.Errorf("len(pcm) = %d for SID frame, want 320", len(pcm))
	}
}

// TestAMRWBDecoder_SpeechFT0 verifies FT=0 (mode 0, 6.60 kbps, 17 bytes) produces 320 samples.
func TestAMRWBDecoder_SpeechFT0(t *testing.T) {
	d, err := newAMRWBDecoder(16000, 1)
	if err != nil {
		t.Fatalf("newAMRWBDecoder: %v", err)
	}
	payload := make([]byte, 2+17) // CMR + TOC + 17 frame bytes
	payload[0] = 0xF0
	payload[1] = tocByte(0) // FT=0
	pcm, err := d.Decode(payload)
	if err != nil {
		t.Fatalf("Decode FT0: %v", err)
	}
	if len(pcm) != 320 {
		t.Errorf("len(pcm) = %d for FT0 frame, want 320", len(pcm))
	}
}

// TestAMRWBDecoder_MultiFrame verifies two frames in one RTP packet → 640 samples.
func TestAMRWBDecoder_MultiFrame(t *testing.T) {
	d, err := newAMRWBDecoder(16000, 1)
	if err != nil {
		t.Fatalf("newAMRWBDecoder: %v", err)
	}
	// Two FT=9 SID frames: CMR + 2 TOC bytes (F=1 on first) + 5+5 bytes
	// First TOC: F=1, FT=9, Q=1 → 0x80 | tocByte(9)
	payload := make([]byte, 1+2+5+5)
	payload[0] = 0xF0                  // CMR
	payload[1] = 0x80 | tocByte(9)     // F=1, FT=9, Q=1
	payload[2] = tocByte(9)            // F=0, FT=9, Q=1
	// payload[3:8] = first SID frame (5 zeros)
	// payload[8:13] = second SID frame (5 zeros)

	pcm, err := d.Decode(payload)
	if err != nil {
		t.Fatalf("Decode multi-frame: %v", err)
	}
	if len(pcm) != 640 {
		t.Errorf("len(pcm) = %d for 2×SID frames, want 640", len(pcm))
	}
}

// TestAMRWBDecoder_BadFrameIndicator verifies BFI=1 (Q=0) doesn't panic.
func TestAMRWBDecoder_BadFrameIndicator(t *testing.T) {
	d, err := newAMRWBDecoder(16000, 1)
	if err != nil {
		t.Fatalf("newAMRWBDecoder: %v", err)
	}
	// FT=0, Q=0 (bad frame): TOC bit2=0
	payload := make([]byte, 2+17)
	payload[0] = 0xF0
	payload[1] = (0 << 3) // F=0, FT=0, Q=0, P=0 → 0x00
	pcm, err := d.Decode(payload)
	if err != nil {
		t.Fatalf("Decode BFI: %v", err)
	}
	if len(pcm) != 320 {
		t.Errorf("len(pcm) = %d for bad frame, want 320", len(pcm))
	}
}

// TestAMRWBDecoder_OutputNotShared đảm bảo hai lần Decode không share slice.
func TestAMRWBDecoder_OutputNotShared(t *testing.T) {
	d, err := newAMRWBDecoder(16000, 1)
	if err != nil {
		t.Fatalf("newAMRWBDecoder: %v", err)
	}
	payload := make([]byte, 2+17)
	payload[0] = 0xF0
	payload[1] = tocByte(0)

	pcm1, _ := d.Decode(payload)
	pcm2, _ := d.Decode(payload)

	if len(pcm1) == 0 || len(pcm2) == 0 {
		t.Skip("empty output, nothing to check")
	}
	pcm1[0] = 9999
	if pcm2[0] == 9999 {
		t.Error("pcm1 and pcm2 share underlying array")
	}
}
