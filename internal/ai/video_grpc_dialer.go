package ai

import (
	"context"
	"fmt"
	"sync/atomic"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	pb "media-ai-gateway/internal/ai/proto"
	"media-ai-gateway/internal/pipeline"
)

const videoSegmentRPC = pb.Method

// VideoDialFunc trả về VideoGRPCDialFunc mở grpc.ClientStream mới trên shared
// conn cho video segmentation RPC. Cùng SharedConnPool với audio — HTTP/2
// multiplexing tái dùng 1 TCP connection cho cả 2 loại stream tới cùng worker
// addr (thực tế audio/video worker thường là 2 địa chỉ khác nhau — GPU-bound
// video worker tách biệt khỏi CPU/ASR worker để không đấu capacity).
func (p *SharedConnPool) VideoDialFunc() VideoGRPCDialFunc {
	return func(ctx context.Context, workerAddr, sessionID, streamID string, streamCfg VideoStreamConfig) (VideoStreamClient, error) {
		if streamCfg.AIService == "gesture_recognition" {
			return p.dialVideoTask(ctx, workerAddr, sessionID, streamID, streamCfg)
		}
		if streamCfg.AIService != "" && streamCfg.AIService != "segmentation" {
			return nil, fmt.Errorf("unsupported video task %q", streamCfg.AIService)
		}
		conn, err := p.getOrCreate(workerAddr)
		if err != nil {
			return nil, fmt.Errorf("grpc: connect %s: %w", workerAddr, err)
		}
		desc := &grpc.StreamDesc{
			StreamName:    "Segment",
			ClientStreams: true,
			ServerStreams: true,
		}
		ctx, cancel := context.WithCancel(ctx)
		cs, err := conn.NewStream(ctx, desc, videoSegmentRPC)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("grpc: open video stream to %s: %w", workerAddr, err)
		}
		client := &grpcVideoStreamClient{cs: cs, sessionID: sessionID, streamID: streamID, cancel: cancel}
		// Frame đầu tiên trên stream luôn là VideoConfig (oneof config) —
		// AI worker dùng nó để khởi tạo model + tham số effect trước khi
		// nhận VideoFrame thực.
		if err := cs.SendMsg(&pb.VideoRequest{SessionId: sessionID, StreamId: streamID,
			Payload: &pb.VideoRequest_Config{Config: &pb.VideoConfig{
				EffectType: streamCfg.EffectType, InferenceWidth: uint32(streamCfg.InferWidth),
				InferenceHeight: uint32(streamCfg.InferHeight), InferenceFps: uint32(streamCfg.InferFPS),
				Threshold: streamCfg.Threshold, AiService: "segmentation", InputFormat: "jpeg",
			}},
		}); err != nil {
			cancel()
			return nil, fmt.Errorf("grpc: send video config to %s: %w", workerAddr, err)
		}
		return client, nil
	}
}

// VideoGRPCDialFunc mở một bidirectional gRPC video stream tới AI worker tại workerAddr.
type VideoGRPCDialFunc func(ctx context.Context, workerAddr, sessionID, streamID string, streamCfg VideoStreamConfig) (VideoStreamClient, error)

// VideoRoutingDialer implements VideoDialer bằng cách chọn AI worker phù hợp
// (task="video_segment") từ WorkerRegistry rồi mở stream qua VideoGRPCDialFunc.
// Đối xứng với RoutingDialer (audio) — xem routing_dialer.go.
type VideoRoutingDialer struct {
	registry *WorkerRegistry
	dialFn   VideoGRPCDialFunc
	Task     string
}

// NewVideoRoutingDialer tạo VideoRoutingDialer.
func NewVideoRoutingDialer(registry *WorkerRegistry, dialFn VideoGRPCDialFunc) *VideoRoutingDialer {
	return &VideoRoutingDialer{registry: registry, dialFn: dialFn, Task: "video_segment"}
}

func (d *VideoRoutingDialer) Dial(ctx context.Context, sessionID, streamID string, streamCfg VideoStreamConfig) (VideoStreamClient, error) {
	task := streamCfg.AIService
	switch task {
	case "segmentation":
		task = "video_segment"
	case "":
		task = d.Task
		if task == "" {
			task = "video_segment"
		}
	}
	worker, err := d.registry.SelectVideoTask(task, "jpeg", "video_segment")
	if err != nil {
		return nil, fmt.Errorf("ai: route video/%s: %w", task, err)
	}
	zap.L().Debug("ai: dialing video worker", zap.String("session_id", sessionID),
		zap.String("task", task), zap.String("worker_id", worker.ID), zap.String("worker_addr", worker.Addr))
	client, err := d.dialFn(ctx, worker.Addr, sessionID, streamID, streamCfg)
	if err != nil {
		d.registry.ReleaseVideoTask(worker.ID)
		return nil, fmt.Errorf("ai: dial video worker %s (%s): %w", worker.ID, worker.Addr, err)
	}
	return &reservedVideoClient{VideoStreamClient: client, registry: d.registry, worker: worker.ID}, nil
}

// grpcVideoStreamClient wraps gRPC ClientStream thành VideoStreamClient.
type grpcVideoStreamClient struct {
	cancel    context.CancelFunc
	cs        grpc.ClientStream
	sessionID string
	streamID  string
	firstSent atomic.Bool
}

func (c *grpcVideoStreamClient) Send(frame pipeline.VideoFrame) error {
	if c.firstSent.CompareAndSwap(false, true) {
		zap.L().Debug("ai: first video frame sent to worker",
			zap.String("session_id", frame.SessionID),
			zap.String("stream_id", frame.StreamID),
			zap.Int("jpeg_bytes", len(frame.JPEG)),
		)
	}
	req := &pb.VideoRequest{SessionId: c.sessionID, StreamId: c.streamID, IsEos: frame.EndOfStream}
	if !frame.EndOfStream {
		req.Payload = &pb.VideoRequest_Frame{Frame: &pb.VideoFrame{
			FrameId: frame.FrameID, RtpTimestamp: int64(frame.RTPTimestamp), TimestampMs: frame.TimestampMs,
			ExtendedRtpTimestamp: frame.ExtendedRTPTimestamp,
			OriginalWidth:        frame.OriginalWidth, OriginalHeight: frame.OriginalHeight,
			Width: uint32(frame.Width), Height: uint32(frame.Height), Encoding: "jpeg", ImageData: frame.JPEG,
		}}
	}
	return c.cs.SendMsg(req)
}

func (c *grpcVideoStreamClient) Recv() (pipeline.RecognitionResult, error) {
	var w pb.VideoResponse
	if err := c.cs.RecvMsg(&w); err != nil {
		return pipeline.RecognitionResult{}, err
	}
	r := pipeline.RecognitionResult{
		SessionID: w.SessionId,
		StreamID:  w.StreamId,
		Kind:      "video_mask",
		IsFinal:   w.IsFinal,
	}
	if w.GetGesture() != nil || (w.AiService != "" && w.AiService != "segmentation") {
		return pipeline.RecognitionResult{}, fmt.Errorf("unexpected result on segmentation stream")
	}
	if mask := w.GetMask(); mask != nil {
		r.VideoMask = &pipeline.VideoMask{
			FrameID: mask.FrameId, RTPTimestamp: uint32(mask.RtpTimestamp),
			MaskWidth: mask.MaskWidth, MaskHeight: mask.MaskHeight,
			RLECounts: mask.RleCounts, LatencyMs: mask.LatencyMs,
			Status: mask.Status, ErrorMessage: mask.ErrorMessage,
		}
	}
	return r, nil
}

func (c *grpcVideoStreamClient) CloseSend() error { return c.cs.CloseSend() }
func (c *grpcVideoStreamClient) Close() error {
	if c.cancel != nil {
		c.cancel()
	}
	return nil
}
