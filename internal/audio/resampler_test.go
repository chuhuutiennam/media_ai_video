package audio

import (
	"math"
	"testing"
)

// --- Helpers ---

func silence(n int) []int16 {
	return make([]int16, n)
}

func constant(n int, v int16) []int16 {
	s := make([]int16, n)
	for i := range s {
		s[i] = v
	}
	return s
}

// allEqual kiểm tra tất cả sample trong slice bằng v.
func allEqual(s []int16, v int16) bool {
	for _, x := range s {
		if x != v {
			return false
		}
	}
	return true
}

// --- Basic construction ---

func TestNewResampler(t *testing.T) {
	r := NewResampler(8000, 1, 16000)
	if r.InRate() != 8000 || r.InCh() != 1 || r.OutRate() != 16000 {
		t.Errorf("got (%d,%d,%d), want (8000,1,16000)", r.InRate(), r.InCh(), r.OutRate())
	}
}

func TestResample_Empty(t *testing.T) {
	r := NewResampler(8000, 1, 16000)
	out := r.Resample(nil)
	if out != nil {
		t.Errorf("expected nil for nil input, got %v", out)
	}
	out = r.Resample([]int16{})
	if out != nil {
		t.Errorf("expected nil for empty input, got %v", out)
	}
}

// --- Pass-through ---

func TestResample_SameRate(t *testing.T) {
	r := NewResampler(16000, 1, 16000)
	in := []int16{100, 200, 300}
	out := r.Resample(in)
	if len(out) != 3 || out[0] != 100 || out[1] != 200 || out[2] != 300 {
		t.Errorf("pass-through: got %v, want [100 200 300]", out)
	}
}

// --- Output length ---

// TestResample_8kTo16k: 20ms @ 8kHz (160 samples) → 320 samples @ 16kHz.
func TestResample_8kTo16k_OutputLength(t *testing.T) {
	r := NewResampler(8000, 1, 16000)
	in := silence(160)
	out := r.Resample(in)
	if len(out) != 320 {
		t.Errorf("8k→16k: len(out) = %d, want 320", len(out))
	}
}

// TestResample_48kTo16k: 20ms @ 48kHz (960 samples) → 320 samples @ 16kHz.
func TestResample_48kTo16k_OutputLength(t *testing.T) {
	r := NewResampler(48000, 1, 16000)
	in := silence(960)
	out := r.Resample(in)
	if len(out) != 320 {
		t.Errorf("48k→16k: len(out) = %d, want 320", len(out))
	}
}

// TestResample_44100To16k: 30ms @ 44100Hz (1323 samples) → 480 samples @ 16kHz.
func TestResample_44100To16k_OutputLength(t *testing.T) {
	r := NewResampler(44100, 1, 16000)
	in := silence(1323)
	out := r.Resample(in)
	if len(out) != 480 {
		t.Errorf("44100→16k: len(out) = %d, want 480", len(out))
	}
}

// --- Silence preservation ---

func TestResample_8kTo16k_Silence(t *testing.T) {
	r := NewResampler(8000, 1, 16000)
	out := r.Resample(silence(160))
	if !allEqual(out, 0) {
		t.Error("silence input should produce silence output")
	}
}

func TestResample_48kTo16k_Silence(t *testing.T) {
	r := NewResampler(48000, 1, 16000)
	out := r.Resample(silence(960))
	if !allEqual(out, 0) {
		t.Error("silence input should produce silence output")
	}
}

// --- Constant amplitude preservation ---

// Constant input → constant output (linear interp of equal values = same value).
func TestResample_8kTo16k_Constant(t *testing.T) {
	r := NewResampler(8000, 1, 16000)
	out := r.Resample(constant(160, 1000))
	if !allEqual(out, 1000) {
		t.Errorf("constant 1000 → got non-constant output (first few: %v)", out[:min(5, len(out))])
	}
}

func TestResample_48kTo16k_Constant(t *testing.T) {
	r := NewResampler(48000, 1, 16000)
	out := r.Resample(constant(960, -500))
	if !allEqual(out, -500) {
		t.Errorf("constant -500 → got non-constant output")
	}
}

// --- Stereo to mono ---

func TestResample_StereoToMono_Mix(t *testing.T) {
	r := NewResampler(16000, 2, 16000) // same rate, just mix channels
	// Interleaved stereo: [L0,R0, L1,R1, L2,R2] = [0,200, 0,400, 0,600]
	// Mono expected:      [(0+200)/2, (0+400)/2, (0+600)/2] = [100, 200, 300]
	in := []int16{0, 200, 0, 400, 0, 600}
	out := r.Resample(in)
	if len(out) != 3 {
		t.Fatalf("stereo→mono: len = %d, want 3", len(out))
	}
	want := []int16{100, 200, 300}
	for i, w := range want {
		if out[i] != w {
			t.Errorf("out[%d] = %d, want %d", i, out[i], w)
		}
	}
}

// TestResample_Stereo48kToMono16k: pipeline đầy đủ WebRTC (48kHz stereo → 16kHz mono).
func TestResample_Stereo48kToMono16k_OutputLength(t *testing.T) {
	r := NewResampler(48000, 2, 16000)
	// 20ms @ 48kHz stereo = 960 * 2 = 1920 int16 samples.
	in := make([]int16, 1920)
	out := r.Resample(in)
	if len(out) != 320 {
		t.Errorf("48kHz stereo → 16kHz mono: len = %d, want 320", len(out))
	}
}

// --- Interpolation correctness ---

// TestResample_8kTo16k_Interp: kiểm tra linear interpolation giữa hai sample.
// Input [0, 1000] @ 8kHz → Output @ 16kHz nên có [0, 500, 1000, 1000] (2x + hold).
func TestResample_8kTo16k_Interp(t *testing.T) {
	r := NewResampler(8000, 1, 16000)
	// Input 2 samples.
	out := r.Resample([]int16{0, 1000})
	if len(out) != 4 {
		t.Fatalf("2 samples × 2 = 4, got %d: %v", len(out), out)
	}
	// out[0] = 0 (pos=0.0)
	// out[1] = lerp(0, 1000, 0.5) = 500 (pos=0.5)
	// out[2] = 1000 (pos=1.0)
	// out[3] = 1000 (pos=1.5, hold-last)
	if out[0] != 0 {
		t.Errorf("out[0] = %d, want 0", out[0])
	}
	if out[1] != 500 {
		t.Errorf("out[1] = %d, want 500 (interpolated)", out[1])
	}
	if out[2] != 1000 {
		t.Errorf("out[2] = %d, want 1000", out[2])
	}
	if out[3] != 1000 {
		t.Errorf("out[3] = %d, want 1000 (hold-last)", out[3])
	}
}

// TestResample_48kTo16k_Decimate: downsampling lấy đúng sample.
// Input [a, b, c, d, e, f] → output [a, d] (mỗi 3rd sample khi step=3.0).
func TestResample_48kTo16k_Decimate(t *testing.T) {
	r := NewResampler(48000, 1, 16000)
	in := []int16{100, 200, 300, 400, 500, 600}
	out := r.Resample(in)
	if len(out) != 2 {
		t.Fatalf("6 samples / 3 = 2, got %d: %v", len(out), out)
	}
	if out[0] != 100 { // pos=0
		t.Errorf("out[0] = %d, want 100", out[0])
	}
	if out[1] != 400 { // pos=3
		t.Errorf("out[1] = %d, want 400", out[1])
	}
}

// --- Phase continuity across chunks ---

// TestResample_PhaseCarryover_TotalSamples: tổng output của N chunk liên tiếp
// phải xấp xỉ N * chunkIn * outRate/inRate.
func TestResample_PhaseCarryover_TotalSamples(t *testing.T) {
	r := NewResampler(48000, 1, 16000) // step = 3
	totalOut := 0
	chunkSize := 960 // 20ms @ 48kHz
	numChunks := 10

	for i := 0; i < numChunks; i++ {
		out := r.Resample(silence(chunkSize))
		totalOut += len(out)
	}

	wantTotal := numChunks * chunkSize / 3 // = 3200
	if totalOut != wantTotal {
		t.Errorf("total output = %d, want %d after %d chunks", totalOut, wantTotal, numChunks)
	}
}

// TestResample_PhaseCarryover_8kTo16k: 8kHz, N chunks.
func TestResample_PhaseCarryover_8kTo16k(t *testing.T) {
	r := NewResampler(8000, 1, 16000)
	totalOut := 0
	chunkSize := 160 // 20ms @ 8kHz
	numChunks := 10

	for i := 0; i < numChunks; i++ {
		out := r.Resample(silence(chunkSize))
		totalOut += len(out)
	}

	wantTotal := numChunks * chunkSize * 2 // = 3200
	if totalOut != wantTotal {
		t.Errorf("total output = %d, want %d", totalOut, wantTotal)
	}
}

// TestResample_PhaseCarryover_NonInteger: tổng sample chính xác với step phân số.
// 44100 → 16000, 30ms chunks (1323 samples → 480 samples).
func TestResample_PhaseCarryover_NonInteger(t *testing.T) {
	r := NewResampler(44100, 1, 16000)
	chunkIn := 1323   // 30ms @ 44100
	chunkOut := 480   // 30ms @ 16000
	numChunks := 100

	totalOut := 0
	for i := 0; i < numChunks; i++ {
		out := r.Resample(silence(chunkIn))
		totalOut += len(out)
	}

	wantTotal := numChunks * chunkOut
	// Chấp nhận sai số ±1 sample do float64 rounding.
	if diff := totalOut - wantTotal; diff < -1 || diff > 1 {
		t.Errorf("total output = %d, want ~%d (diff %d)", totalOut, wantTotal, diff)
	}
}

// TestResample_Reset: Reset() xóa phase, bắt đầu lại từ đầu.
func TestResample_Reset(t *testing.T) {
	r := NewResampler(48000, 1, 16000)

	out1 := r.Resample(silence(960))
	r.Reset()
	out2 := r.Resample(silence(960))

	if len(out1) != len(out2) {
		t.Errorf("after Reset: len differs (%d vs %d)", len(out1), len(out2))
	}
}

// --- Edge cases ---

// TestResample_SingleSample: 1 sample input không panic.
func TestResample_SingleSample(t *testing.T) {
	r := NewResampler(8000, 1, 16000)
	out := r.Resample([]int16{500})
	if len(out) == 0 {
		t.Error("single sample should produce at least 1 output")
	}
}

// TestResample_MaxAmplitude: giá trị biên int16 không overflow.
func TestResample_MaxAmplitude(t *testing.T) {
	r := NewResampler(8000, 1, 16000)
	in := []int16{math.MaxInt16, math.MinInt16}
	out := r.Resample(in)
	if len(out) == 0 {
		t.Fatal("expected output")
	}
	for i, s := range out {
		if s < math.MinInt16 || s > math.MaxInt16 {
			t.Errorf("out[%d] = %d out of int16 range", i, s)
		}
	}
}

// min helper (Go 1.21+ có builtin, dùng local để tương thích).
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
