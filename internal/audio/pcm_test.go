package audio

import (
	"errors"
	"math"
	"testing"
)

// ── S16LEToInt16 ──────────────────────────────────────────────────────────────

func TestS16LEToInt16_Empty(t *testing.T) {
	out, err := S16LEToInt16(nil)
	if err != nil || out != nil {
		t.Errorf("nil input: want (nil, nil), got (%v, %v)", out, err)
	}
	out, err = S16LEToInt16([]byte{})
	if err != nil || out != nil {
		t.Errorf("empty input: want (nil, nil), got (%v, %v)", out, err)
	}
}

func TestS16LEToInt16_OddLength(t *testing.T) {
	_, err := S16LEToInt16([]byte{0x01})
	if !errors.Is(err, ErrOddByteCount) {
		t.Errorf("want ErrOddByteCount, got %v", err)
	}
	_, err = S16LEToInt16([]byte{0x01, 0x02, 0x03})
	if !errors.Is(err, ErrOddByteCount) {
		t.Errorf("3 bytes: want ErrOddByteCount, got %v", err)
	}
}

func TestS16LEToInt16_KnownValues(t *testing.T) {
	// 0x0000 = 0, 0x0100 = 256 (little-endian), 0xFF7F = 32767, 0x0080 = -32768
	b := []byte{
		0x00, 0x00, // 0
		0x00, 0x01, // 256
		0xFF, 0x7F, // 32767
		0x00, 0x80, // -32768
	}
	want := []int16{0, 256, 32767, -32768}

	got, err := S16LEToInt16(b)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("len: want %d, got %d", len(want), len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d]: want %d, got %d", i, want[i], got[i])
		}
	}
}

// ── Int16ToS16LE ──────────────────────────────────────────────────────────────

func TestInt16ToS16LE_Empty(t *testing.T) {
	if b := Int16ToS16LE(nil); b != nil {
		t.Errorf("nil input: want nil, got %v", b)
	}
	if b := Int16ToS16LE([]int16{}); b != nil {
		t.Errorf("empty input: want nil, got %v", b)
	}
}

func TestInt16ToS16LE_KnownValues(t *testing.T) {
	samples := []int16{0, 256, 32767, -32768}
	want := []byte{
		0x00, 0x00,
		0x00, 0x01,
		0xFF, 0x7F,
		0x00, 0x80,
	}
	got := Int16ToS16LE(samples)
	if len(got) != len(want) {
		t.Fatalf("len: want %d, got %d", len(want), len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("byte[%d]: want 0x%02X, got 0x%02X", i, want[i], got[i])
		}
	}
}

// ── Roundtrip S16LE ↔ int16 ──────────────────────────────────────────────────

func TestS16LE_Roundtrip(t *testing.T) {
	original := []int16{-32768, -1000, 0, 1000, 32767, 42}
	encoded := Int16ToS16LE(original)
	decoded, err := S16LEToInt16(encoded)
	if err != nil {
		t.Fatalf("S16LEToInt16: %v", err)
	}
	for i, want := range original {
		if decoded[i] != want {
			t.Errorf("[%d]: roundtrip want %d, got %d", i, want, decoded[i])
		}
	}
}

// ── Energy ────────────────────────────────────────────────────────────────────

func TestEnergy_Empty(t *testing.T) {
	if e := Energy(nil); e != 0 {
		t.Errorf("nil: want 0, got %f", e)
	}
	if e := Energy([]int16{}); e != 0 {
		t.Errorf("empty: want 0, got %f", e)
	}
}

func TestEnergy_Silence(t *testing.T) {
	samples := make([]int16, 160) // all zeros
	if e := Energy(samples); e != 0 {
		t.Errorf("silence: want 0, got %f", e)
	}
}

func TestEnergy_MaxAmplitude(t *testing.T) {
	// +32767 → RMS = 32767/32768 ≈ 0.9999…
	samples := make([]int16, 160)
	for i := range samples {
		samples[i] = 32767
	}
	e := Energy(samples)
	if e < 0.999 || e > 1.0 {
		t.Errorf("max amplitude: want ~1.0, got %f", e)
	}
}

func TestEnergy_SineApprox(t *testing.T) {
	// Sóng hình vuông ±16384 → RMS = 16384/32768 = 0.5
	samples := make([]int16, 160)
	for i := range samples {
		if i%2 == 0 {
			samples[i] = 16384
		} else {
			samples[i] = -16384
		}
	}
	e := Energy(samples)
	if math.Abs(e-0.5) > 0.001 {
		t.Errorf("square wave ±16384: want ~0.5, got %f", e)
	}
}

func TestEnergy_Range(t *testing.T) {
	// Energy phải luôn trong [0, 1] với bất kỳ int16 input nào.
	cases := [][]int16{
		{32767, -32767, 32767, -32767},
		{-32768, -32768},
		{1, 2, 3, 4, 5},
		make([]int16, 1000),
	}
	for _, c := range cases {
		e := Energy(c)
		if e < 0 || e > 1 {
			t.Errorf("Energy out of [0,1]: %f", e)
		}
	}
}

// ── Peak ─────────────────────────────────────────────────────────────────────

func TestPeak_Empty(t *testing.T) {
	if p := Peak(nil); p != 0 {
		t.Errorf("nil: want 0, got %d", p)
	}
}

func TestPeak_AllPositive(t *testing.T) {
	samples := []int16{100, 200, 50, 300, 150}
	if p := Peak(samples); p != 300 {
		t.Errorf("want 300, got %d", p)
	}
}

func TestPeak_AllNegative(t *testing.T) {
	samples := []int16{-100, -200, -50, -300, -150}
	if p := Peak(samples); p != 300 {
		t.Errorf("want 300 (abs), got %d", p)
	}
}

func TestPeak_Mixed(t *testing.T) {
	samples := []int16{-500, 300, -100, 400}
	if p := Peak(samples); p != 500 {
		t.Errorf("want 500, got %d", p)
	}
}

func TestPeak_MinInt16(t *testing.T) {
	// abs(-32768) không fit vào int16 → clamp về 32767
	samples := []int16{-32768, 100}
	if p := Peak(samples); p != 32767 {
		t.Errorf("MinInt16: want 32767 (clamped), got %d", p)
	}
}

func TestPeak_MaxInt16(t *testing.T) {
	samples := []int16{32767, -100}
	if p := Peak(samples); p != 32767 {
		t.Errorf("want 32767, got %d", p)
	}
}

// ── IsSilence ────────────────────────────────────────────────────────────────

func TestIsSilence_ZeroSamples(t *testing.T) {
	if !IsSilence(make([]int16, 160), 0.01) {
		t.Error("all-zero samples should be silence")
	}
}

func TestIsSilence_LoudSignal(t *testing.T) {
	samples := make([]int16, 160)
	for i := range samples {
		samples[i] = 16000
	}
	if IsSilence(samples, 0.01) {
		t.Error("loud signal should not be silence")
	}
}

func TestIsSilence_AtThreshold(t *testing.T) {
	// Energy = threshold → không phải silence (< không thỏa mãn)
	samples := []int16{32767}
	e := Energy(samples) // ≈ 0.9999
	if IsSilence(samples, e) {
		t.Error("energy == threshold should not be silence")
	}
	if !IsSilence(samples, e+0.001) {
		t.Error("energy below threshold should be silence")
	}
}

// ── Clamp16 ──────────────────────────────────────────────────────────────────

func TestClamp16_InRange(t *testing.T) {
	cases := []int32{0, 1000, -1000, 32767, -32768}
	for _, v := range cases {
		got := Clamp16(v)
		if int32(got) != v {
			t.Errorf("Clamp16(%d) = %d, want no clamp", v, got)
		}
	}
}

func TestClamp16_Overflow(t *testing.T) {
	if Clamp16(32768) != 32767 {
		t.Error("Clamp16(32768): want 32767")
	}
	if Clamp16(100000) != 32767 {
		t.Error("Clamp16(100000): want 32767")
	}
}

func TestClamp16_Underflow(t *testing.T) {
	if Clamp16(-32769) != -32768 {
		t.Error("Clamp16(-32769): want -32768")
	}
	if Clamp16(-100000) != -32768 {
		t.Error("Clamp16(-100000): want -32768")
	}
}

// ── MixMono ──────────────────────────────────────────────────────────────────

func TestMixMono_Basic(t *testing.T) {
	a := []int16{100, -200, 300}
	b := []int16{50, 100, -100}
	got := MixMono(a, b)
	want := []int16{150, -100, 200}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d]: want %d, got %d", i, want[i], got[i])
		}
	}
}

func TestMixMono_Clamps(t *testing.T) {
	a := []int16{32000}
	b := []int16{32000}
	got := MixMono(a, b) // 64000 > 32767
	if got[0] != 32767 {
		t.Errorf("MixMono overflow: want 32767, got %d", got[0])
	}
}

func TestMixMono_NegativeClamps(t *testing.T) {
	a := []int16{-32000}
	b := []int16{-32000}
	got := MixMono(a, b) // -64000 < -32768
	if got[0] != -32768 {
		t.Errorf("MixMono underflow: want -32768, got %d", got[0])
	}
}

func TestMixMono_DifferentLengths(t *testing.T) {
	a := []int16{1, 2, 3, 4, 5}
	b := []int16{10, 20, 30}
	got := MixMono(a, b)
	if len(got) != 3 {
		t.Errorf("len: want 3 (shorter), got %d", len(got))
	}
	want := []int16{11, 22, 33}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d]: want %d, got %d", i, want[i], got[i])
		}
	}
}

func TestMixMono_Silence(t *testing.T) {
	a := []int16{1000, -1000, 500}
	b := make([]int16, 3) // silence
	got := MixMono(a, b)
	for i, v := range a {
		if got[i] != v {
			t.Errorf("[%d]: mix with silence want %d, got %d", i, v, got[i])
		}
	}
}

func TestMixMono_BothEmpty(t *testing.T) {
	got := MixMono([]int16{}, []int16{})
	if len(got) != 0 {
		t.Errorf("both empty: want len=0, got %d", len(got))
	}
}
