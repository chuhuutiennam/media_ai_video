package ai

import (
	"context"
	"fmt"
	"io"
	"sync"

	"google.golang.org/grpc"
	pb "media-ai-gateway/internal/ai/proto"
	"media-ai-gateway/internal/pipeline"
)

const videoTaskPendingLimit = 128

type videoTaskClient struct {
	stream                      grpc.ClientStream
	cancel                      context.CancelFunc
	session, id, epoch, service string
	maxHands                    int
	mu                          sync.Mutex
	pending                     map[int64]pipeline.VideoFrame
	pendingAvailable            chan struct{}
}

func (p *SharedConnPool) dialVideoTask(ctx context.Context, addr, session, id string, cfg VideoStreamConfig) (VideoStreamClient, error) {
	conn, err := p.getOrCreate(addr)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	stream, err := conn.NewStream(ctx, &grpc.StreamDesc{StreamName: "Segment", ClientStreams: true, ServerStreams: true}, pb.Method,
		grpc.MaxCallSendMsgSize(32<<20), grpc.MaxCallRecvMsgSize(1<<20))
	if err != nil {
		cancel()
		return nil, err
	}
	c := &videoTaskClient{stream: stream, cancel: cancel, session: session, id: id, epoch: cfg.MediaEpoch, service: cfg.AIService, maxHands: cfg.MaxHands, pending: make(map[int64]pipeline.VideoFrame)}
	err = stream.SendMsg(&pb.VideoRequest{SessionId: session, StreamId: id, Payload: &pb.VideoRequest_Config{Config: &pb.VideoConfig{
		SelectedService: cfg.SelectedService, AiService: cfg.AIService, MediaEpoch: cfg.MediaEpoch, InputFormat: "jpeg",
		InferenceWidth: uint32(cfg.InferWidth), InferenceHeight: uint32(cfg.InferHeight),
		InferenceFps: uint32(cfg.InferFPS), MaxHands: uint32(cfg.MaxHands)}}})
	if err != nil {
		cancel()
		return nil, err
	}
	return c, nil
}

func (c *videoTaskClient) Send(f pipeline.VideoFrame) error {
	if f.EndOfStream {
		return c.stream.SendMsg(&pb.VideoRequest{SessionId: c.session, StreamId: c.id, IsEos: true})
	}
	if f.Width <= 0 || f.Height <= 0 || f.Width > 4096 || f.Height > 4096 || f.OriginalWidth <= 0 || f.OriginalHeight <= 0 || len(f.JPEG) == 0 {
		return fmt.Errorf("invalid JPEG frame")
	}
	meta := f
	meta.JPEG = nil
	// Transport buffering can accept frames faster than inference completes.
	// Keep every outstanding frame's metadata until its reply, and push pressure
	// back to the task's bounded input queue instead of invalidating late replies.
	ctx := c.stream.Context()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		if len(c.pending) < videoTaskPendingLimit {
			c.pending[f.FrameID] = meta
			c.mu.Unlock()
			break
		}
		if c.pendingAvailable == nil {
			c.pendingAvailable = make(chan struct{}, 1)
		}
		available := c.pendingAvailable
		c.mu.Unlock()
		select {
		case <-available:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	err := c.stream.SendMsg(&pb.VideoRequest{SessionId: c.session, StreamId: c.id, Payload: &pb.VideoRequest_Frame{Frame: frameProto(f)}})
	if err != nil {
		c.takePending(f.FrameID)
	}
	return err
}

func (c *videoTaskClient) takePending(id int64) (pipeline.VideoFrame, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
		if c.pendingAvailable != nil {
			select {
			case c.pendingAvailable <- struct{}{}:
			default:
			}
		}
	}
	return f, ok
}
func (c *videoTaskClient) Recv() (pipeline.RecognitionResult, error) {
	var wire pb.VideoResponse
	if err := c.stream.RecvMsg(&wire); err != nil {
		return pipeline.RecognitionResult{}, err
	}
	if wire.SessionId != c.session || wire.StreamId != c.id || wire.AiService != c.service {
		return pipeline.RecognitionResult{}, fmt.Errorf("video result identity/service mismatch")
	}
	g := wire.GetGesture()
	if g == nil || c.service != "gesture_recognition" {
		return pipeline.RecognitionResult{}, fmt.Errorf("unsupported video task result")
	}
	f, ok := c.takePending(g.FrameId)
	if !ok || g.MediaEpoch != c.epoch || g.RtpTimestamp != f.RTPTimestamp || g.ExtendedRtpTimestamp != f.ExtendedRTPTimestamp ||
		g.TimestampMs != f.TimestampMs || g.OriginalWidth != uint32(f.OriginalWidth) || g.OriginalHeight != uint32(f.OriginalHeight) || len(g.Hands) > c.maxHands {
		return pipeline.RecognitionResult{}, fmt.Errorf("gesture result does not match source frame: frame_id=%d pending=%t", g.FrameId, ok)
	}
	out := gestureFromProto(g)
	if !out.Valid() {
		return pipeline.RecognitionResult{}, fmt.Errorf("invalid gesture snapshot")
	}
	return pipeline.RecognitionResult{SessionID: c.session, StreamID: c.id, Kind: "video_gesture", AIService: c.service,
		Seq: wire.Seq, TsStart: g.TimestampMs, TsEnd: g.TimestampMs, GestureResult: out}, nil
}
func (c *videoTaskClient) CloseSend() error { return c.stream.CloseSend() }
func (c *videoTaskClient) Close() error     { c.cancel(); return nil }

type reservedVideoClient struct {
	VideoStreamClient
	registry *WorkerRegistry
	worker   string
	once     sync.Once
}

func (c *reservedVideoClient) Close() error {
	var err error
	c.once.Do(func() {
		if closer, ok := c.VideoStreamClient.(io.Closer); ok {
			err = closer.Close()
		}
		c.registry.ReleaseVideoTask(c.worker)
	})
	return err
}
