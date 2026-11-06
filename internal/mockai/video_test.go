package mockai

import (
	"bytes"
	"image"
	"image/jpeg"
	"io"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	protobuf "google.golang.org/protobuf/proto"
	pb "media-ai-gateway/internal/ai/proto"
)

type messages struct {
	grpc.ServerStream
	input  []*pb.VideoRequest
	output []*pb.VideoResponse
}

func (s *messages) RecvMsg(v any) error {
	if len(s.input) == 0 {
		return io.EOF
	}
	protobuf.Merge(v.(*pb.VideoRequest), s.input[0])
	s.input = s.input[1:]
	return nil
}
func (s *messages) SendMsg(v any) error {
	s.output = append(s.output, protobuf.Clone(v.(*pb.VideoResponse)).(*pb.VideoResponse))
	return nil
}

func TestSharedJPEGContractDispatchAndValidation(t *testing.T) {
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewGray(image.Rect(0, 0, 8, 4)), nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		mutate  func(*pb.VideoConfig, *pb.VideoRequest)
		invalid bool
	}{
		{name: "gesture"},
		{name: "legacy segmentation", mutate: func(c *pb.VideoConfig, _ *pb.VideoRequest) { c.AiService = "" }},
		{name: "explicit segmentation", mutate: func(c *pb.VideoConfig, _ *pb.VideoRequest) { c.AiService = "segmentation" }},
		{name: "wrong task", invalid: true, mutate: func(c *pb.VideoConfig, _ *pb.VideoRequest) { c.AiService = "asr" }},
		{name: "raw profile", invalid: true, mutate: func(c *pb.VideoConfig, _ *pb.VideoRequest) { c.InputFormat = "RGB24" }},
		{name: "raw frame", invalid: true, mutate: func(_ *pb.VideoConfig, r *pb.VideoRequest) { r.GetFrame().Encoding = "RGB24" }},
		{name: "fake JPEG", invalid: true, mutate: func(_ *pb.VideoConfig, r *pb.VideoRequest) { r.GetFrame().ImageData = make([]byte, 8*4*3) }},
		{name: "wrong JPEG dimensions", invalid: true, mutate: func(_ *pb.VideoConfig, r *pb.VideoRequest) { r.GetFrame().Width = 4 }},
		{name: "truncated JPEG", invalid: true, mutate: func(_ *pb.VideoConfig, r *pb.VideoRequest) {
			r.GetFrame().ImageData = encoded.Bytes()[:encoded.Len()-20]
		}},
		{name: "cross stream", invalid: true, mutate: func(_ *pb.VideoConfig, r *pb.VideoRequest) { r.StreamId = "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := &pb.VideoConfig{AiService: "gesture_recognition", MediaEpoch: "source-1", InputFormat: "jpeg", MaxHands: 4,
				InferenceFps: 15, InferenceWidth: 8, InferenceHeight: 4, EffectType: "bg_blur"}
			frame := &pb.VideoRequest{SessionId: "call", StreamId: "video", Payload: &pb.VideoRequest_Frame{Frame: &pb.VideoFrame{
				FrameId: 1, RtpTimestamp: 9000, ExtendedRtpTimestamp: 9000, TimestampMs: 0, OriginalWidth: 16, OriginalHeight: 8,
				Width: 8, Height: 4, Encoding: "jpeg", ImageData: encoded.Bytes(),
			}}}
			if tc.mutate != nil {
				tc.mutate(config, frame)
			}
			stream := &messages{input: []*pb.VideoRequest{
				{SessionId: "call", StreamId: "video", Payload: &pb.VideoRequest_Config{Config: config}}, frame,
				{SessionId: "call", StreamId: "video", IsEos: true},
			}}
			err := process(nil, stream)
			if tc.invalid {
				if status.Code(err) != codes.InvalidArgument || len(stream.output) != 0 {
					t.Fatalf("invalid input accepted: err=%v, results=%d", err, len(stream.output))
				}
				return
			}
			if err != nil || len(stream.output) != 1 {
				t.Fatalf("JPEG stream failed: err=%v, results=%d", err, len(stream.output))
			}
			response := stream.output[0]
			if response.SessionId != "call" || response.StreamId != "video" {
				t.Fatal("result lost identity")
			}
			if config.AiService == "gesture_recognition" {
				g := response.GetGesture()
				if g == nil || len(g.Hands) != 2 || g.MediaEpoch != "source-1" || g.OriginalWidth != 16 || g.TimestampMs != 0 || response.GetMask() != nil {
					t.Fatalf("bad gesture response: %v", response)
				}
			} else {
				mask := response.GetMask()
				if mask == nil || mask.Status != "ok" || mask.MaskWidth != 8 || mask.MaskHeight != 4 || response.GetGesture() != nil {
					t.Fatalf("bad mask response: %v", response)
				}
			}
		})
	}
}
