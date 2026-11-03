// Package audio xử lý PCM: resample, mix stereo→mono, chunk.
package audio

// Resampler convert PCM int16 từ bất kỳ rate/channels sang outRate mono.
//
// Thuật toán: integer phase accumulator — không có float64 rounding error.
//
//	phaseNum tăng inRate mỗi output sample.
//	Input index = phaseNum / outRate (integer division).
//	Fractional part = phaseNum % outRate → dùng cho linear interpolation.
//
// Phase được giữ qua các lần gọi Resample liên tiếp để đảm bảo
// continuity tại chunk boundary — không bị bước nhảy ở mỗi 20ms packet.
//
// Các case phổ biến:
//
//	8000 Hz mono    → 16000 Hz mono   (PCMU/PCMA telecom, upsample 2x)
//	48000 Hz mono   → 16000 Hz mono   (Opus WebRTC, downsample 3x)
//	48000 Hz stereo → 16000 Hz mono   (Opus WebRTC stereo)
//	16000 Hz mono   → 16000 Hz mono   (AMR-WB, pass-through)
//
// Không goroutine-safe: dùng một Resampler per session/goroutine.
type Resampler struct {
	inRate  int
	inCh    int
	outRate int

	// phaseNum là phase tích luỹ theo đơn vị input_position * outRate.
	// Denominator ngầm định = outRate.
	// Tăng inRate mỗi output sample; mang carry sang chunk tiếp theo.
	// Luôn nằm trong [0, inRate).
	phaseNum int64
}

// NewResampler tạo Resampler từ (inRate Hz, inCh channels) → (outRate Hz, mono).
func NewResampler(inRate, inCh, outRate int) *Resampler {
	return &Resampler{inRate: inRate, inCh: inCh, outRate: outRate}
}

func (r *Resampler) InRate() int  { return r.inRate }
func (r *Resampler) InCh() int    { return r.inCh }
func (r *Resampler) OutRate() int { return r.outRate }

// Reset xóa phase state. Gọi khi bắt đầu stream mới trên cùng Resampler.
func (r *Resampler) Reset() { r.phaseNum = 0 }

// Resample convert một chunk PCM về outRate mono.
// Input phải interleaved nếu inCh > 1.
// Trả về nil với input rỗng.
func (r *Resampler) Resample(in []int16) []int16 {
	if len(in) == 0 {
		return nil
	}
	mono := r.toMono(in)
	if r.inRate == r.outRate {
		r.phaseNum = 0
		return mono
	}
	return r.linearResample(mono)
}

// toMono mix interleaved multi-channel PCM thành mono (trung bình các channel).
func (r *Resampler) toMono(in []int16) []int16 {
	if r.inCh == 1 {
		return in
	}
	out := make([]int16, len(in)/r.inCh)
	for i := range out {
		var sum int32
		for ch := 0; ch < r.inCh; ch++ {
			sum += int32(in[i*r.inCh+ch])
		}
		out[i] = int16(sum / int32(r.inCh))
	}
	return out
}

// linearResample thực hiện linear interpolation với integer phase accumulator.
//
// Tại mỗi output sample:
//
//	idx     = phaseNum / outRate          → input sample index (integer)
//	fracNum = phaseNum % outRate          → trọng số interpolation (0..outRate-1)
//	out     = in[idx] + (in[idx+1]-in[idx]) * fracNum / outRate
//
// Không dùng float64 → không có rounding error tích lũy theo thời gian.
//
// Tại chunk boundary (idx == n-1 và fracNum > 0):
//
//	dùng "hold last" để tránh cần sample từ chunk kế tiếp.
//	Sai số tối đa 1 sample/chunk, không đáng kể với STT.
func (r *Resampler) linearResample(mono []int16) []int16 {
	n := int64(len(mono))
	inRate := int64(r.inRate)
	outRate := int64(r.outRate)

	// limit: khi phaseNum >= limit thì đã tiêu thụ hết chunk.
	limit := n * outRate

	// Pre-allocate: số output ≈ n * outRate / inRate.
	outCap := n*outRate/inRate + 2
	out := make([]int16, 0, outCap)

	phaseNum := r.phaseNum
	lastSample := mono[n-1]

	for phaseNum < limit {
		idx := phaseNum / outRate
		fracNum := phaseNum % outRate

		var sample int16
		if idx >= n-1 {
			// Chunk boundary: hold last sample.
			sample = lastSample
		} else if fracNum == 0 {
			sample = mono[idx]
		} else {
			// Linear interpolation với integer arithmetic.
			s0 := int64(mono[idx])
			s1 := int64(mono[idx+1])
			sample = int16(s0 + (s1-s0)*fracNum/outRate)
		}
		out = append(out, sample)
		phaseNum += inRate
	}

	// Lưu carry sang chunk tiếp theo.
	r.phaseNum = phaseNum - limit
	return out
}
