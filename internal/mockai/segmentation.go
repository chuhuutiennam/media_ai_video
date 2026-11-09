package mockai

import (
	"bytes"
	"image/color"
	"image/jpeg"
	"io"
	"math"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pb "media-ai-gateway/internal/ai/proto"
)

// segment preserves the legacy JPEG luminance mock, including error masks.
func segment(stream grpc.ServerStream, first *pb.VideoRequest, opts VideoOptions) error {
	config := first.GetConfig()
	if config.InputFormat != "" && config.InputFormat != "jpeg" {
		return status.Error(codes.InvalidArgument, "input_format must be jpeg")
	}
	if config.EffectType != "bg_blur" && config.EffectType != "bg_replace" && config.EffectType != "bg_remove" {
		return status.Error(codes.InvalidArgument, "unsupported effect_type")
	}
	threshold := config.Threshold
	if threshold == 0 {
		threshold = 0.5
	}
	if math.IsNaN(float64(threshold)) || threshold < 0 || threshold > 1 {
		return status.Error(codes.InvalidArgument, "threshold must be in [0,1]")
	}
	for sent := 0; ; {
		var req pb.VideoRequest
		if err := stream.RecvMsg(&req); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if req.SessionId != first.SessionId || req.StreamId != first.StreamId {
			return status.Error(codes.InvalidArgument, "session_id/stream_id changed within stream")
		}
		if req.IsEos && req.Payload == nil {
			return nil
		}
		frame := req.GetFrame()
		if frame == nil {
			return status.Error(codes.InvalidArgument, "expected frame after config")
		}
		started := time.Now()
		mask := &pb.VideoMask{FrameId: frame.FrameId, RtpTimestamp: frame.RtpTimestamp, Status: "ok"}
		if err := infer(frame, config, threshold, mask); err != nil {
			mask.Status, mask.ErrorMessage = "error", err.Error()
		}
		mask.LatencyMs = float32(time.Since(started).Seconds() * 1000)
		if err := stream.SendMsg(&pb.VideoResponse{SessionId: req.SessionId, StreamId: req.StreamId, IsFinal: true,
			AiService: first.GetConfig().AiService, Result: &pb.VideoResponse_Mask{Mask: mask}}); err != nil {
			return err
		}
		sent++
		if req.IsEos || opts.endStream(sent) {
			return nil
		}
	}
}

func infer(req *pb.VideoFrame, config *pb.VideoConfig, threshold float32, resp *pb.VideoMask) error {
	if req.Encoding != "jpeg" {
		return status.Error(codes.InvalidArgument, "encoding must be jpeg")
	}
	reader := bytes.NewReader(req.ImageData)
	size, err := jpeg.DecodeConfig(reader)
	if err != nil {
		return err
	}
	if size.Width <= 0 || size.Height <= 0 || size.Width > 4096 || size.Height > 4096 {
		return status.Error(codes.InvalidArgument, "JPEG dimensions exceed lab limit")
	}
	if uint32(size.Width) != config.InferenceWidth || uint32(size.Height) != config.InferenceHeight {
		return status.Error(codes.InvalidArgument, "JPEG dimensions do not match config")
	}
	reader.Reset(req.ImageData)
	img, err := jpeg.Decode(reader)
	if err != nil {
		return err
	}
	resp.MaskWidth, resp.MaskHeight = int32(size.Width), int32(size.Height)
	// Binary RLE, row-major, starting with a background (0) run. A foreground
	// first pixel therefore produces an initial zero count. Sum = width*height.
	foreground := false
	var run uint32
	for y := 0; y < size.Height; y++ {
		for x := 0; x < size.Width; x++ {
			v := color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y
			pixel := float32(v)/255 > threshold
			if pixel != foreground {
				resp.RleCounts = append(resp.RleCounts, run)
				run, foreground = 0, pixel
			}
			run++
		}
	}
	resp.RleCounts = append(resp.RleCounts, run)
	return nil
}
