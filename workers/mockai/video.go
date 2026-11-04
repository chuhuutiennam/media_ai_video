// Package mockai implements deterministic video tasks for transport/render tests.
// Results are scripted by media time; they are NOT gesture model predictions.
package mockai

import (
	"bytes"
	"fmt"
	"image/jpeg"
	"io"
	"math"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pb "media-ai-gateway/internal/ai/proto"
)

var generation atomic.Uint64

// RegisterVideo serves both tasks on the canonical JPEG contract and RPC.
func RegisterVideo(server *grpc.Server) {
	server.RegisterService(&grpc.ServiceDesc{ServiceName: "video.VideoStream", HandlerType: (*any)(nil),
		Streams: []grpc.StreamDesc{{StreamName: "Segment", Handler: process, ClientStreams: true, ServerStreams: true}}}, struct{}{})
}
func process(_ any, stream grpc.ServerStream) error {
	var first pb.VideoRequest
	if err := stream.RecvMsg(&first); err != nil {
		return err
	}
	config := first.GetConfig()
	if config == nil || first.SessionId == "" || first.IsEos {
		return status.Error(codes.InvalidArgument, "first message must contain config and session_id")
	}
	if config.AiService == "" || config.AiService == "segmentation" {
		return segment(stream, &first)
	}
	if config.AiService != "gesture_recognition" || first.StreamId == "" || config.MediaEpoch == "" ||
		config.InputFormat != "jpeg" || config.MaxHands < 1 || config.MaxHands > 8 || config.InferenceFps < 1 || config.InferenceFps > 60 {
		return status.Error(codes.InvalidArgument, "expected gesture config with JPEG profile, identity and max_hands 1..8")
	}
	epoch := fmt.Sprintf("mock-%d-%d", time.Now().UnixNano(), generation.Add(1))
	var seq uint64
	var lastID, lastTime int64 = -1, -1
	for {
		var request pb.VideoRequest
		if err := stream.RecvMsg(&request); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if request.SessionId != first.SessionId || request.StreamId != first.StreamId {
			return status.Error(codes.InvalidArgument, "session_id/stream_id changed within stream")
		}
		if request.IsEos && request.Payload == nil {
			return nil
		}
		f := request.GetFrame()
		if f == nil || f.FrameId <= lastID || f.TimestampMs <= lastTime || f.Width == 0 || f.Height == 0 || f.Width > 4096 || f.Height > 4096 ||
			f.OriginalWidth <= 0 || f.OriginalHeight <= 0 || f.RtpTimestamp < 0 || f.RtpTimestamp > math.MaxUint32 || f.Encoding != "jpeg" {
			return status.Error(codes.InvalidArgument, "expected monotonic JPEG frame")
		}
		size, err := jpeg.DecodeConfig(bytes.NewReader(f.ImageData))
		if err != nil || size.Width != int(f.Width) || size.Height != int(f.Height) {
			return status.Error(codes.InvalidArgument, "JPEG dimensions do not match frame")
		}
		if _, err := jpeg.Decode(bytes.NewReader(f.ImageData)); err != nil {
			return status.Error(codes.InvalidArgument, "invalid JPEG payload")
		}
		lastID, lastTime = f.FrameId, f.TimestampMs
		seq++
		g := Snapshot(config, f, epoch, seq)
		if err := stream.SendMsg(&pb.VideoResponse{SessionId: first.SessionId, StreamId: first.StreamId, AiService: config.AiService, Seq: seq,
			Result: &pb.VideoResponse_Gesture{Gesture: g}}); err != nil {
			return err
		}
		if request.IsEos {
			return nil
		}
	}
}

// Snapshot moves two independent tracks, then emits empty hands to test clearing.
func Snapshot(open *pb.VideoConfig, f *pb.VideoFrame, epoch string, seq uint64) *pb.GestureResult {
	g := &pb.GestureResult{MediaEpoch: open.MediaEpoch, RecognizerEpoch: epoch, ResponseId: fmt.Sprintf("%s/%d", epoch, seq),
		FrameId: f.FrameId, RtpTimestamp: uint32(f.RtpTimestamp), ExtendedRtpTimestamp: f.ExtendedRtpTimestamp, TimestampMs: f.TimestampMs,
		OriginalWidth: uint32(f.OriginalWidth), OriginalHeight: uint32(f.OriginalHeight), Status: "ok", Hands: []*pb.Hand{}}
	t := f.TimestampMs
	within := t % 3000
	if within >= 2500 {
		return g
	}
	for i := 0; i < min(2, int(open.MaxHands)); i++ {
		x := 0.27 + 0.1*math.Sin(float64(t)/400)
		label, key, side := "Thumb_Up", "thumb_up", "Left"
		if i == 1 {
			x = 0.72 + 0.07*math.Cos(float64(t)/350)
			label, key, side = "Wave", "wave", "Right"
		}
		if within >= 1200 && i == 0 {
			label, key = "Victory", "victory"
		}
		y := 0.6 + float64(i)*0.05
		g.Hands = append(g.Hands, &pb.Hand{TrackId: uint64(i + 1), Handedness: side, RawLabel: label, RawScore: 0.99, StableLabel: label,
			Center: []float64{x, y}, Bbox: []float64{x - 0.07, y - 0.13, x + 0.07, y + 0.13},
			ActiveReaction: &pb.Reaction{EventId: fmt.Sprintf("%s/%d/%d/%s", epoch, i, t/3000, label), Gesture: label, EmojiKey: key,
				StartedAtMs: t - within, ValidUntilMs: min(t+400, t-within+2500)}})
	}
	return g
}
