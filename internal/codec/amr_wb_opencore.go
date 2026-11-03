//go:build opencore_amrwb

package codec

/*
#cgo LDFLAGS: -lopencore-amrwb
#include <opencore-amrwb/dec_if.h>
*/
import "C"
import (
	"fmt"
	"runtime"
	"unsafe"
)

// amrWBDecoder wraps the opencore-amrwb D_IF decoder.
// One instance per RTP session; D_IF_decode is stateful (maintains internal ACELP state).
type amrWBDecoder struct {
	state      unsafe.Pointer
	sampleRate int
	channels   int
}

func newAMRWBDecoder(sampleRate, channels int) (Decoder, error) {
	state := C.D_IF_init()
	if state == nil {
		return nil, fmt.Errorf("opencore-amrwb: D_IF_init returned nil")
	}
	d := &amrWBDecoder{
		state:      state,
		sampleRate: sampleRate,
		channels:   channels,
	}
	runtime.SetFinalizer(d, (*amrWBDecoder).release)
	return d, nil
}

func (d *amrWBDecoder) release() {
	if d.state != nil {
		C.D_IF_exit(d.state)
		d.state = nil
	}
}

func (d *amrWBDecoder) SampleRate() int { return d.sampleRate }
func (d *amrWBDecoder) Channels() int   { return d.channels }

// Decode nhận RFC 4867 octet-aligned AMR-WB payload và trả về PCM int16 @ 16kHz mono.
// Mỗi speech/SID frame (FT 0-9) tạo ra 320 int16 samples (20ms @ 16kHz).
// FT 14 (SPEECH_LOST) và FT 15 (NO_DATA) bị bỏ qua — không tạo ra samples.
func (d *amrWBDecoder) Decode(payload []byte) ([]int16, error) {
	if d.state == nil {
		return nil, fmt.Errorf("opencore-amrwb: decoder already released")
	}

	frames, err := ParseAMRFrames(payload, amrWB)
	if len(frames) == 0 {
		return nil, err
	}

	var out []int16
	var synth [320]C.short

	for _, f := range frames {
		if f.FT >= 14 { // SPEECH_LOST or NO_DATA — no samples generated
			continue
		}

		// D_IF_decode serial: serial[0] = FT, serial[1:] = compressed frame bytes.
		// This maps directly from RFC 4867 frame bytes (AMR-WB payload after TOC).
		serial := make([]byte, 1+len(f.Payload))
		serial[0] = f.FT
		copy(serial[1:], f.Payload)

		bfi := C.int(0)
		if !f.Quality {
			bfi = C.int(1)
		}

		C.D_IF_decode(
			d.state,
			(*C.uchar)(unsafe.Pointer(&serial[0])),
			&synth[0],
			bfi,
		)
		for _, s := range synth {
			out = append(out, int16(s))
		}
	}
	return out, nil
}
