package codec

// G.711 PCMU (µ-law) và PCMA (A-law) decoder.
//
// Cả hai dùng chung expLut từ sox/g711 (ITU-T G.711 reference):
//   magnitude = expLut[exponent] + mantissa << (exponent + 3)
//
// Lookup table 256-entry được pre-compute tại init để tránh branch trong hot loop.

type g711Kind uint8

const (
	kindPCMU g711Kind = iota
	kindPCMA
)

// expLut là bảng bias theo exponent, dùng chung cho µ-law và A-law.
// Nguồn: SoX g711.c, ITU-T G.711 Appendix.
var expLut = [8]int32{0, 132, 396, 924, 1980, 4092, 8316, 16764}

// ulawTable[i] = decoded int16 cho wire byte i (PCMU).
var ulawTable [256]int16

// alawTable[i] = decoded int16 cho wire byte i (PCMA).
var alawTable [256]int16

func init() {
	for i := 0; i < 256; i++ {
		ulawTable[i] = decodeUlaw(byte(i))
		alawTable[i] = decodeAlaw(byte(i))
	}
}

// decodeUlaw decode một G.711 µ-law wire byte thành PCM int16.
//
// Thuật toán (ITU-T G.711):
//  1. Complement tất cả bit để lấy codeword gốc.
//  2. Bit 7 = sign (1 = dương, 0 = âm).
//  3. Bits 6-4 = exponent, bits 3-0 = mantissa.
//  4. magnitude = expLut[exp] + mantissa << (exp + 3).
func decodeUlaw(wire byte) int16 {
	u := ^wire
	sign := u & 0x80
	exp := (u & 0x70) >> 4
	mantissa := u & 0x0f
	mag := expLut[exp] + int32(mantissa)<<(uint(exp)+3)
	if sign != 0 {
		return int16(mag)
	}
	return int16(-mag)
}

// decodeAlaw decode một G.711 A-law wire byte thành PCM int16.
//
// Thuật toán (ITU-T G.711):
//  1. XOR với 0x55 để đảo các bit chẵn, lấy codeword gốc.
//  2. Bit 7 = sign (1 = dương, 0 = âm).
//  3. Bits 6-4 = exponent, bits 3-0 = mantissa.
//  4. exp=0: magnitude = mantissa×16 + 8
//     exp≥1: magnitude = (mantissa×16 + 264) << (exp−1)
//  (khác µ-law: A-law dùng step size 16, bias 264=132×2 mỗi segment)
func decodeAlaw(wire byte) int16 {
	a := wire ^ 0x55
	sign := a & 0x80
	exp := (a & 0x70) >> 4
	mantissa := a & 0x0f
	var mag int32
	if exp == 0 {
		mag = int32(mantissa)*16 + 8
	} else {
		mag = (int32(mantissa)*16 + 264) << (uint(exp) - 1)
	}
	if sign != 0 {
		return int16(mag)
	}
	return int16(-mag)
}

// g711Decoder implement Decoder cho PCMU và PCMA.
type g711Decoder struct {
	kind       g711Kind
	sampleRate int
	channels   int
}

func (d *g711Decoder) SampleRate() int { return d.sampleRate }
func (d *g711Decoder) Channels() int   { return d.channels }

// Decode chuyển mỗi byte payload thành một int16 PCM sample.
// Với G.711, 1 byte = 1 sample; payload 160 bytes = 20ms @ 8kHz.
func (d *g711Decoder) Decode(payload []byte) ([]int16, error) {
	if len(payload) == 0 {
		return nil, nil
	}
	out := make([]int16, len(payload))
	if d.kind == kindPCMU {
		for i, b := range payload {
			out[i] = ulawTable[b]
		}
	} else {
		for i, b := range payload {
			out[i] = alawTable[b]
		}
	}
	return out, nil
}
