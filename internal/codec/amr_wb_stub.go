//go:build !opencore_amrwb

package codec

func newAMRWBDecoder(sampleRate, channels int) (Decoder, error) {
	return &amrDecoder{variant: amrWB, sampleRate: sampleRate, channels: channels}, nil
}
