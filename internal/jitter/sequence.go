package jitter

// SeqDiff trả về a - b có xét wraparound uint16.
// Kết quả nằm trong [-32768, 32767]:
//   - dương: a đến sau b
//   - âm:    a đến trước b (hoặc là duplicate/quá cũ)
//   - 0:     cùng sequence
func SeqDiff(a, b uint16) int {
	return int(int16(a - b))
}

// SeqLess trả về true nếu a đến trước b theo thứ tự RTP (có wraparound).
func SeqLess(a, b uint16) bool {
	return SeqDiff(a, b) < 0
}
