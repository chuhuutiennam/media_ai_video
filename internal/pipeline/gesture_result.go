package pipeline

import "math"

type Reaction struct {
	EventID      string `json:"event_id"`
	Gesture      string `json:"gesture"`
	EmojiKey     string `json:"emoji_key"`
	StartedAtMs  int64  `json:"started_at_ms"`
	ValidUntilMs int64  `json:"valid_until_ms"`
}
type GestureHand struct {
	TrackID        uint64    `json:"track_id"`
	Handedness     string    `json:"handedness"`
	RawLabel       string    `json:"raw_label"`
	RawScore       float64   `json:"raw_score"`
	StableLabel    string    `json:"stable_label"`
	Center         []float64 `json:"center"`
	BBox           []float64 `json:"bbox"`
	ActiveReaction *Reaction `json:"active_reaction,omitempty"`
}
type GestureFrameResult struct {
	MediaEpoch           string        `json:"media_epoch"`
	RecognizerEpoch      string        `json:"recognizer_epoch"`
	ResponseID           string        `json:"response_id"`
	FrameID              int64         `json:"frame_id"`
	RTPTimestamp         uint32        `json:"rtp_timestamp"`
	ExtendedRTPTimestamp uint64        `json:"extended_rtp_timestamp"`
	TimestampMs          int64         `json:"timestamp_ms"`
	OriginalWidth        uint32        `json:"original_width"`
	OriginalHeight       uint32        `json:"original_height"`
	Status               string        `json:"status"`
	Hands                []GestureHand `json:"hands"`
}

func (g *GestureFrameResult) Valid() bool {
	if g == nil || g.Status != "ok" || g.FrameID < 1 || g.TimestampMs < 0 || g.MediaEpoch == "" ||
		g.RecognizerEpoch == "" || g.ResponseID == "" || g.OriginalWidth == 0 || g.OriginalHeight == 0 || len(g.Hands) > 8 {
		return false
	}
	ids := make(map[uint64]bool)
	for _, h := range g.Hands {
		if h.TrackID == 0 || ids[h.TrackID] || len(h.Center) != 2 || len(h.BBox) != 4 || math.IsNaN(h.RawScore) || h.RawScore < 0 || h.RawScore > 1 {
			return false
		}
		ids[h.TrackID] = true
		for _, x := range append(append([]float64{}, h.Center...), h.BBox...) {
			if math.IsNaN(x) || math.IsInf(x, 0) || x < 0 || x > 1 {
				return false
			}
		}
		if h.BBox[0] >= h.BBox[2] || h.BBox[1] >= h.BBox[3] {
			return false
		}
		if a := h.ActiveReaction; a != nil && (a.EventID == "" || a.EmojiKey == "" || a.StartedAtMs < 0 || a.StartedAtMs > g.TimestampMs || a.ValidUntilMs <= g.TimestampMs) {
			return false
		}
	}
	return true
}
