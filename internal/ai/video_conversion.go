package ai

import (
	pb "media-ai-gateway/internal/ai/proto"
	"media-ai-gateway/internal/pipeline"
)

// Shared mappings keep direct-worker and engine transports on the same contract.
func frameProto(f pipeline.VideoFrame) *pb.VideoFrame {
	return &pb.VideoFrame{FrameId: f.FrameID, RtpTimestamp: int64(f.RTPTimestamp), ExtendedRtpTimestamp: f.ExtendedRTPTimestamp,
		TimestampMs: f.TimestampMs, OriginalWidth: f.OriginalWidth, OriginalHeight: f.OriginalHeight,
		Width: uint32(f.Width), Height: uint32(f.Height), Encoding: "jpeg", ImageData: f.JPEG}
}

func maskFromProto(m *pb.VideoMask) *pipeline.VideoMask {
	return &pipeline.VideoMask{FrameID: m.FrameId, RTPTimestamp: uint32(m.RtpTimestamp), MaskWidth: m.MaskWidth,
		MaskHeight: m.MaskHeight, RLECounts: m.RleCounts, LatencyMs: m.LatencyMs, Status: m.Status, ErrorMessage: m.ErrorMessage}
}

func gestureFromProto(g *pb.GestureResult) *pipeline.GestureFrameResult {
	out := &pipeline.GestureFrameResult{MediaEpoch: g.MediaEpoch, RecognizerEpoch: g.RecognizerEpoch, ResponseID: g.ResponseId,
		FrameID: g.FrameId, RTPTimestamp: g.RtpTimestamp, ExtendedRTPTimestamp: g.ExtendedRtpTimestamp, TimestampMs: g.TimestampMs,
		OriginalWidth: g.OriginalWidth, OriginalHeight: g.OriginalHeight, Status: g.Status, Hands: make([]pipeline.GestureHand, 0, len(g.Hands))}
	for _, h := range g.Hands {
		hand := pipeline.GestureHand{TrackID: h.TrackId, Handedness: h.Handedness, RawLabel: h.RawLabel, RawScore: h.RawScore, StableLabel: h.StableLabel, Center: h.Center, BBox: h.Bbox}
		if a := h.ActiveReaction; a != nil {
			hand.ActiveReaction = &pipeline.Reaction{EventID: a.EventId, Gesture: a.Gesture, EmojiKey: a.EmojiKey, StartedAtMs: a.StartedAtMs, ValidUntilMs: a.ValidUntilMs}
		}
		out.Hands = append(out.Hands, hand)
	}
	return out
}
