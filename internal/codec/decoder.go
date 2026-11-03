// Package codec chuyển đổi RTP payload sang PCM int16.
//
// Pure Go (không cần CGO): PCMU, PCMA.
// Stub — cần link thư viện ngoài: Opus (xem opus.go), AMR-NB/WB (xem amr.go), EVS (xem evs.go).
package codec

import (
	"fmt"
	"strings"
)

// Decoder decode RTP payload bytes thành PCM signed 16-bit samples.
type Decoder interface {
	// Decode chuyển đổi một RTP payload thành PCM int16.
	// Trả về slice mới mỗi lần gọi; caller không cần reuse buffer.
	Decode(payload []byte) ([]int16, error)

	// SampleRate trả về sample rate đầu ra (Hz).
	SampleRate() int

	// Channels trả về số kênh audio (thường là 1 = mono).
	Channels() int
}

// New tạo Decoder phù hợp với codec string.
//
// Hỗ trợ (case-insensitive): "PCMU", "PCMA", "opus".
// sampleRate và channels được dùng để override default khi cần.
// G.711 mặc định 8000 Hz mono; Opus mặc định 48000 Hz.
func New(codec string, sampleRate, channels int) (Decoder, error) {
	switch strings.ToUpper(codec) {
	case "PCMU":
		if sampleRate == 0 {
			sampleRate = 8000
		}
		if channels == 0 {
			channels = 1
		}
		return &g711Decoder{kind: kindPCMU, sampleRate: sampleRate, channels: channels}, nil

	case "PCMA":
		if sampleRate == 0 {
			sampleRate = 8000
		}
		if channels == 0 {
			channels = 1
		}
		return &g711Decoder{kind: kindPCMA, sampleRate: sampleRate, channels: channels}, nil

	case "OPUS":
		if sampleRate == 0 {
			sampleRate = 48000
		}
		if channels == 0 {
			channels = 1
		}
		return newOpusDecoder(sampleRate, channels)

	case "AMR":
		if sampleRate == 0 {
			sampleRate = 8000
		}
		if channels == 0 {
			channels = 1
		}
		return newAMRDecoder(amrNB, sampleRate, channels)

	case "AMR-WB":
		if sampleRate == 0 {
			sampleRate = 16000
		}
		if channels == 0 {
			channels = 1
		}
		return newAMRDecoder(amrWB, sampleRate, channels)

	case "EVS":
		if sampleRate == 0 {
			sampleRate = 16000 // VoLTE primary mode
		}
		if channels == 0 {
			channels = 1
		}
		return newEVSDecoder(sampleRate, channels)

	default:
		return nil, fmt.Errorf("codec: unsupported codec %q", codec)
	}
}
