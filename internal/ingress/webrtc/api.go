package webrtc

import (
	pion "github.com/pion/webrtc/v4"
	"github.com/pion/interceptor"
)

// NewAPI builds a pion WebRTC API configured for audio-only receive.
//
// MediaEngine: registers Opus PT 111 (48 kHz, 2-ch) as the sole audio codec.
// InterceptorRegistry: default interceptors (NACK, RTCP SR/RR, TWCC).
// SettingEngine: optional ICE port range and NAT1To1 IP override.
func NewAPI(cfg Config) (*pion.API, error) {
	m := &pion.MediaEngine{}
	if err := m.RegisterCodec(pion.RTPCodecParameters{
		RTPCodecCapability: pion.RTPCodecCapability{
			MimeType:    pion.MimeTypeOpus,
			ClockRate:   48000,
			Channels:    2,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
		PayloadType: 111,
	}, pion.RTPCodecTypeAudio); err != nil {
		return nil, err
	}

	i := &interceptor.Registry{}
	if err := pion.RegisterDefaultInterceptors(m, i); err != nil {
		return nil, err
	}

	s := pion.SettingEngine{}
	if cfg.ICEPortMin > 0 {
		if err := s.SetEphemeralUDPPortRange(cfg.ICEPortMin, cfg.ICEPortMax); err != nil {
			return nil, err
		}
	}
	if len(cfg.NAT1To1IPs) > 0 {
		s.SetNAT1To1IPs(cfg.NAT1To1IPs, pion.ICECandidateTypeHost)
	}

	return pion.NewAPI(
		pion.WithMediaEngine(m),
		pion.WithInterceptorRegistry(i),
		pion.WithSettingEngine(s),
	), nil
}
