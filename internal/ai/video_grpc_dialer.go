package ai

import (
	"context"
	"fmt"
	"sync/atomic"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	"media-ai-gateway/internal/pipeline"
)

// VideoDialFunc trả về VideoGRPCDialFunc mở grpc.ClientStream mới trên shared
// conn cho video segmentation RPC. Cùng SharedConnPool với audio — HTTP/2
// multiplexing tái dùng 1 TCP connection cho cả 2 loại stream tới cùng worker
// addr (thực tế audio/video worker thường là 2 địa chỉ khác nhau — GPU-bound
// video worker tách biệt khỏi CPU/ASR worker để không đấu capacity).
func (p *SharedConnPool) VideoDialFunc() VideoGRPCDialFunc {
	return func(ctx context.Context, workerAddr, sessionID, streamID string, streamCfg VideoStreamConfig) (VideoStreamClient, error) {
		conn, err := p.getOrCreate(workerAddr)
		if err != nil {
			return nil, fmt.Errorf("grpc: connect %s: %w", workerAddr, err)
		}
		desc := &grpc.StreamDesc{
			StreamName:    "Segment",
			ClientStreams: true,
			ServerStreams: true,
		}
		cs, err := conn.NewStream(ctx, desc, videoSegmentRPC, grpc.ForceCodec(videoProtoCodec{}))
		if err != nil {
			return nil, fmt.Errorf("grpc: open video stream to %s: %w", workerAddr, err)
		}
		client := &grpcVideoStreamClient{cs: cs, sessionID: sessionID, streamID: streamID}
		// Frame đầu tiên trên stream luôn là VideoConfig (oneof config) —
		// AI worker dùng nó để khởi tạo model + tham số effect trước khi
		// nhận VideoFrame thực.
		if err := cs.SendMsg(&videoRequestWire{
			SessionID:  sessionID,
			StreamID:   streamID,
			HasConfig:  true,
			EffectType: streamCfg.EffectType,
			InferWidth: uint32(streamCfg.InferWidth), InferHeight: uint32(streamCfg.InferHeight), InferFPS: uint32(streamCfg.InferFPS),
			Threshold: streamCfg.Threshold,
		}); err != nil {
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
	Task string
}

// NewVideoRoutingDialer tạo VideoRoutingDialer.
func NewVideoRoutingDialer(registry *WorkerRegistry, dialFn VideoGRPCDialFunc) *VideoRoutingDialer {
	return &VideoRoutingDialer{registry: registry, dialFn: dialFn, Task: "video_segment"}
}

func (d *VideoRoutingDialer) Dial(ctx context.Context, sessionID, streamID string, streamCfg VideoStreamConfig) (VideoStreamClient, error) {
	task := d.Task
	if task == "" {
		task = "video_segment"
	}
	worker, err := d.registry.Select("*", task)
	if err != nil {
		zap.L().Debug("ai: no video worker available", zap.String("session_id", sessionID), zap.Error(err))
		return nil, fmt.Errorf("ai: route video/%s: %w", task, err)
	}
	zap.L().Debug("ai: dialing video worker", zap.String("session_id", sessionID), zap.String("worker_id", worker.ID), zap.String("worker_addr", worker.Addr))
	client, err := d.dialFn(ctx, worker.Addr, sessionID, streamID, streamCfg)
	if err != nil {
		return nil, fmt.Errorf("ai: dial video worker %s (%s): %w", worker.ID, worker.Addr, err)
	}
	return client, nil
}

// grpcVideoStreamClient wraps gRPC ClientStream thành VideoStreamClient.
type grpcVideoStreamClient struct {
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
	return c.cs.SendMsg(&videoRequestWire{
		SessionID:      c.sessionID,
		StreamID:       c.streamID,
		HasFrame:       true,
		FrameID:        frame.FrameID,
		RTPTimestamp:   int64(frame.RTPTimestamp),
		TimestampMs:    frame.TimestampMs,
		OriginalWidth:  frame.OriginalWidth,
		OriginalHeight: frame.OriginalHeight,
		Encoding:       "jpeg",
		ImageData:      frame.JPEG,
		IsEOS:          frame.EndOfStream,
	})
}

func (c *grpcVideoStreamClient) Recv() (pipeline.RecognitionResult, error) {
	var w videoResponseWire
	if err := c.cs.RecvMsg(&w); err != nil {
		return pipeline.RecognitionResult{}, err
	}
	r := pipeline.RecognitionResult{
		SessionID: w.SessionID,
		StreamID:  w.StreamID,
		Kind:      "video_mask",
		IsFinal:   w.IsFinal,
	}
	if w.HasMask {
		r.VideoMask = &pipeline.VideoMask{
			FrameID:      w.FrameID,
			RTPTimestamp: uint32(w.RTPTimestamp),
			MaskWidth:    w.MaskWidth,
			MaskHeight:   w.MaskHeight,
			RLECounts:    w.RLECounts,
			LatencyMs:    w.LatencyMs,
			Status:       w.Status,
			ErrorMessage: w.ErrorMessage,
		}
	}
	return r, nil
}

func (c *grpcVideoStreamClient) CloseSend() error { return c.cs.CloseSend() }
func (c *grpcVideoStreamClient) Close() error     { return nil }
