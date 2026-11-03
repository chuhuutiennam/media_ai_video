package audio

import (
	"encoding/binary"
	"errors"
	"math"
)

// ErrOddByteCount được trả về khi byte slice có độ dài lẻ — không hợp lệ với S16LE.
var ErrOddByteCount = errors.New("audio: PCM S16LE requires even byte count")

// S16LEToInt16 decode PCM S16LE bytes thành []int16.
// Trả về ErrOddByteCount nếu len(b) lẻ.
// Trả về nil cho slice rỗng.
func S16LEToInt16(b []byte) ([]int16, error) {
	if len(b) == 0 {
		return nil, nil
	}
	if len(b)%2 != 0 {
		return nil, ErrOddByteCount
	}
	out := make([]int16, len(b)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(b[i*2:]))
	}
	return out, nil
}

// Int16ToS16LE encode []int16 thành PCM S16LE bytes (little-endian, 2 bytes/sample).
// Trả về nil cho slice rỗng.
func Int16ToS16LE(samples []int16) []byte {
	if len(samples) == 0 {
		return nil
	}
	b := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(s))
	}
	return b
}

// Energy tính RMS (Root Mean Square) của PCM samples, normalize về [0.0, 1.0]
// trong đó 1.0 tương đương biên độ tối đa int16 (32768).
// Trả về 0 cho slice rỗng.
func Energy(samples []int16) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum int64
	for _, s := range samples {
		v := int64(s)
		sum += v * v
	}
	rms := math.Sqrt(float64(sum) / float64(len(samples)))
	return rms / 32768.0
}

// Peak trả về biên độ tuyệt đối lớn nhất trong samples.
// Trả về 0 cho slice rỗng.
func Peak(samples []int16) int16 {
	var peak int16
	for _, s := range samples {
		if s < 0 {
			// Tránh overflow khi s == math.MinInt16: so sánh trực tiếp.
			if -s > peak && s != -32768 {
				peak = -s
			} else if s == -32768 {
				return 32767 // clamp: abs(MinInt16) không fit vào int16
			}
		} else if s > peak {
			peak = s
		}
	}
	return peak
}

// IsSilence trả về true nếu RMS energy của samples thấp hơn threshold.
// threshold là giá trị normalize [0.0, 1.0]; giá trị thực tế cho VAD: 0.01 (~-40 dBFS).
func IsSilence(samples []int16, threshold float64) bool {
	return Energy(samples) < threshold
}

// MixMono cộng hai luồng PCM mono sample-by-sample, clamp tránh int16 overflow.
// Nếu hai slice khác độ dài, kết quả có độ dài bằng slice ngắn hơn.
func MixMono(a, b []int16) []int16 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	out := make([]int16, n)
	for i := 0; i < n; i++ {
		out[i] = Clamp16(int32(a[i]) + int32(b[i]))
	}
	return out
}

// Clamp16 giới hạn v vào phạm vi int16 [-32768, 32767].
// Dùng khi cộng/trừ PCM samples để tránh wraparound overflow.
func Clamp16(v int32) int16 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int16(v)
}
