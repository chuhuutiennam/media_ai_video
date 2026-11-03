package rawrtp

import (
	"time"

	pionrtp "github.com/pion/rtp"

	"media-ai-gateway/internal/pipeline"
	"media-ai-gateway/internal/session"
)

// CodecInfo giữ metadata codec cần để xây dựng MediaPacket.
type CodecInfo struct {
	SessionID  string
	SourceType string
	Codec      string
	SampleRate int
	Channels   int
}

// SessionRouter map packet → PacketQueue + CodecInfo.
// Routing order: SSRC first (canonical RTP demuxer), then remote addr (fallback when SSRC=0 or unknown).
type SessionRouter interface {
	RouteBySSRC(ssrc uint32) (queue chan<- pipeline.MediaPacket, info CodecInfo, ok bool)
	RouteByAddr(addr string) (queue chan<- pipeline.MediaPacket, info CodecInfo, ok bool)
}

func (g *Ingress) handle(b []byte, remoteAddr string) {
	var hdr pionrtp.Header
	payloadOffset, err := hdr.Unmarshal(b)
	if err != nil || hdr.Version != 2 {
		g.droppedParse.Add(1)
		return
	}

	var queue chan<- pipeline.MediaPacket
	var info CodecInfo
	var ok bool

	if hdr.SSRC != 0 {
		queue, info, ok = g.router.RouteBySSRC(hdr.SSRC)
	}
	if !ok {
		queue, info, ok = g.router.RouteByAddr(remoteAddr)
	}
	if !ok {
		g.droppedUnknownSSRC.Add(1)
		return
	}

	payload := make([]byte, len(b)-payloadOffset)
	copy(payload, b[payloadOffset:])

	pkt := pipeline.MediaPacket{
		SessionID:    info.SessionID,
		SourceType:   info.SourceType,
		SSRC:         hdr.SSRC,
		PayloadType:  hdr.PayloadType,
		Sequence:     hdr.SequenceNumber,
		Timestamp:    hdr.Timestamp,
		Marker:       hdr.Marker,
		Payload:      payload,
		ReceivedAtMs: time.Now().UnixMilli(),
		Codec:        info.Codec,
		SampleRate:   info.SampleRate,
		Channels:     info.Channels,
	}

	select {
	case queue <- pkt:
		g.routed.Add(1)
	default:
		g.droppedQueueFull.Add(1)
	}
}

// ManagerRouter adapt session.Manager thành SessionRouter.
type ManagerRouter struct {
	mgr *session.Manager
}

// NewManagerRouter tạo SessionRouter backed bởi session.Manager.
func NewManagerRouter(mgr *session.Manager) *ManagerRouter {
	return &ManagerRouter{mgr: mgr}
}

func (r *ManagerRouter) RouteBySSRC(ssrc uint32) (chan<- pipeline.MediaPacket, CodecInfo, bool) {
	sess, ok := r.mgr.GetBySSRC(ssrc)
	if !ok {
		return nil, CodecInfo{}, false
	}
	return sess.PacketQueue, sessionCodecInfo(sess), true
}

func (r *ManagerRouter) RouteByAddr(addr string) (chan<- pipeline.MediaPacket, CodecInfo, bool) {
	sess, ok := r.mgr.GetByAddr(addr)
	if !ok {
		return nil, CodecInfo{}, false
	}
	return sess.PacketQueue, sessionCodecInfo(sess), true
}

func sessionCodecInfo(s *session.Session) CodecInfo {
	return CodecInfo{
		SessionID:  s.ID,
		SourceType: s.SourceType,
		Codec:      s.Codec,
		SampleRate: s.SampleRate,
		Channels:   s.Channels,
	}
}
