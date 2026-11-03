package codec

import (
	"fmt"

	"github.com/pion/opus"
)

// ErrOpusNotAvailable được trả về khi Opus decoder không khởi tạo được.
var ErrOpusNotAvailable = fmt.Errorf("opus decoder not available")

type opusDecoder struct {
	d          opus.Decoder // pion/opus pure-Go decoder (value type, pointer receiver methods)
	sampleRate int
	channels   int
	buf        []int16 // reuse buffer; max 120ms frame
}

func newOpusDecoder(sampleRate, channels int) (Decoder, error) {
	d, err := opus.NewDecoderWithOutput(sampleRate, channels)
	if err != nil {
		return nil, fmt.Errorf("opus: NewDecoderWithOutput(%d, %d): %w", sampleRate, channels, err)
	}
	// Pre-allocate for the maximum Opus frame duration (120 ms).
	maxSamples := sampleRate * 120 / 1000 * channels
	return &opusDecoder{
		d:          d,
		sampleRate: sampleRate,
		channels:   channels,
		buf:        make([]int16, maxSamples),
	}, nil
}

func (d *opusDecoder) SampleRate() int { return d.sampleRate }
func (d *opusDecoder) Channels() int   { return d.channels }

// Decode decodes one Opus RTP payload into PCM int16 samples.
// The returned slice is a fresh copy; caller owns it.
func (d *opusDecoder) Decode(payload []byte) ([]int16, error) {
	if len(payload) == 0 {
		return nil, nil
	}
	// DecodeToInt16 returns samples per channel; writes n*channels values into d.buf.
	n, err := d.d.DecodeToInt16(payload, d.buf)
	if err != nil {
		return nil, fmt.Errorf("opus: decode: %w", err)
	}
	out := make([]int16, n*d.channels)
	copy(out, d.buf[:n*d.channels])
	return out, nil
}
