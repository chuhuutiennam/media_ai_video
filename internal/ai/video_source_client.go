package ai

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	pb "media-ai-gateway/internal/ai/proto"
	"media-ai-gateway/internal/pipeline"
)

const SourceRPC = "/video.VideoAIService/Process"

// SourceStreamConfig separates the transport input profile from model profiles.
// Gesture.InferWidth specifies the task's longest edge, not a fixed width.
type SourceStreamConfig struct {
	InputFPS, InputMaxDimension int
	Segmentation, Gesture       *VideoStreamConfig
}

type SourceDialer struct {
	pool        *SharedConnPool
	target      string
	openTimeout time.Duration
}

func NewSourceDialer(pool *SharedConnPool, target string, openTimeout time.Duration) *SourceDialer {
	return &SourceDialer{pool: pool, target: target, openTimeout: openTimeout}
}

func taskConfig(c *VideoStreamConfig, service, epoch, selected string) *pb.VideoConfig {
	if c == nil {
		return nil
	}
	return &pb.VideoConfig{AiService: service, InputFormat: "jpeg", MediaEpoch: epoch, SelectedService: selected,
		EffectType: c.EffectType, InferenceWidth: uint32(c.InferWidth), InferenceHeight: uint32(c.InferHeight),
		InferenceFps: uint32(c.InferFPS), MaxHands: uint32(c.MaxHands), Threshold: c.Threshold}
}

func (d *SourceDialer) Dial(ctx context.Context, session, streamID string, cfg VideoStreamConfig) (VideoStreamClient, error) {
	if cfg.Source == nil || cfg.MediaEpoch == "" || (cfg.Source.Segmentation == nil && cfg.Source.Gesture == nil) {
		return nil, fmt.Errorf("source stream requires epoch and at least one task")
	}
	conn, err := d.pool.getOrCreate(d.target)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(d.openTimeout, cancel)
	defer timer.Stop()
	cs, err := conn.NewStream(ctx, &grpc.StreamDesc{StreamName: "Process", ClientStreams: true, ServerStreams: true}, SourceRPC,
		grpc.MaxCallSendMsgSize(8<<20), grpc.MaxCallRecvMsgSize(8<<20))
	if err != nil {
		cancel()
		return nil, err
	}
	open := &pb.SourceOpen{SessionId: session, StreamId: streamID, MediaEpoch: cfg.MediaEpoch, SelectedService: cfg.SelectedService,
		InputFps: uint32(cfg.Source.InputFPS), InputMaxDimension: uint32(cfg.Source.InputMaxDimension),
		Segmentation: taskConfig(cfg.Source.Segmentation, "segmentation", cfg.MediaEpoch, cfg.SelectedService),
		Gesture:      taskConfig(cfg.Source.Gesture, "gesture_recognition", cfg.MediaEpoch, cfg.SelectedService)}
	if err = cs.SendMsg(&pb.SourceRequest{Payload: &pb.SourceRequest_Open{Open: open}}); err != nil {
		cancel()
		return nil, err
	}
	var ready pb.SourceResponse
	if err = cs.RecvMsg(&ready); err != nil {
		cancel()
		return nil, err
	}
	c := &sourceClient{stream: cs, ctx: ctx, cancel: cancel, open: open, engineEpoch: ready.EngineEpoch,
		pending: make(map[int64]*sourcePending), awaiting: make(map[int64]bool), seq: make(map[string]uint64)}
	if err = c.checkEnvelope(&ready); err != nil {
		cancel()
		return nil, err
	}
	accepted := ready.GetOpened()
	if accepted == nil || accepted.MaxInflight < 1 || accepted.MaxInflight > 64 {
		cancel()
		return nil, fmt.Errorf("invalid source admission")
	}
	tasks := make(map[string]bool)
	for _, name := range accepted.Tasks {
		tasks[name] = true
	}
	if len(accepted.Tasks) != len(tasks) || len(tasks) != boolCount(open.Segmentation != nil)+boolCount(open.Gesture != nil) ||
		tasks["segmentation"] != (open.Segmentation != nil) || tasks["gesture_recognition"] != (open.Gesture != nil) {
		cancel()
		return nil, fmt.Errorf("source admission task mismatch")
	}
	c.credits = make(chan struct{}, accepted.MaxInflight)
	for range accepted.MaxInflight {
		c.credits <- struct{}{}
	}
	return c, nil
}

func boolCount(v bool) int {
	if v {
		return 1
	}
	return 0
}

type sourcePending struct {
	frame pipeline.VideoFrame
	sent  time.Time
	seen  map[string]bool
}
type sourceClient struct {
	stream      grpc.ClientStream
	ctx         context.Context
	cancel      context.CancelFunc
	open        *pb.SourceOpen
	engineEpoch string
	credits     chan struct{}
	mu          sync.Mutex
	pending     map[int64]*sourcePending
	awaiting    map[int64]bool
	seq         map[string]uint64
}

func (c *sourceClient) checkEnvelope(w *pb.SourceResponse) error {
	if w.SessionId != c.open.SessionId || w.StreamId != c.open.StreamId || w.MediaEpoch != c.open.MediaEpoch ||
		w.EngineEpoch == "" || w.EngineEpoch != c.engineEpoch {
		return fmt.Errorf("source response identity/epoch mismatch")
	}
	return nil
}

func (c *sourceClient) Send(f pipeline.VideoFrame) error {
	if f.EndOfStream {
		return c.stream.SendMsg(&pb.SourceRequest{Payload: &pb.SourceRequest_Close{Close: true}})
	}
	select {
	case <-c.ctx.Done():
		return c.ctx.Err()
	case <-c.credits:
	}
	meta := f
	meta.JPEG = nil
	c.mu.Lock()
	// Metadata retention is independent of ingress credits and task completion.
	for id, p := range c.pending {
		if time.Since(p.sent) > 10*time.Second {
			delete(c.pending, id)
		}
	}
	if len(c.pending) >= 256 {
		var oldest int64 = 1<<63 - 1
		for id := range c.pending {
			if id < oldest {
				oldest = id
			}
		}
		delete(c.pending, oldest)
	}
	c.pending[f.FrameID] = &sourcePending{frame: meta, sent: time.Now(), seen: make(map[string]bool)}
	c.awaiting[f.FrameID] = true
	c.mu.Unlock()
	return c.stream.SendMsg(&pb.SourceRequest{Payload: &pb.SourceRequest_Frame{Frame: frameProto(f)}})
}

func (c *sourceClient) Recv() (pipeline.RecognitionResult, error) {
	for {
		var w pb.SourceResponse
		if err := c.stream.RecvMsg(&w); err != nil {
			return pipeline.RecognitionResult{}, err
		}
		if err := c.checkEnvelope(&w); err != nil {
			return pipeline.RecognitionResult{}, err
		}
		if a := w.GetAccepted(); a != nil {
			c.mu.Lock()
			ok := c.awaiting[a.FrameId]
			delete(c.awaiting, a.FrameId)
			c.mu.Unlock()
			if !ok {
				return pipeline.RecognitionResult{}, fmt.Errorf("unknown/duplicate ingress acknowledgment")
			}
			select {
			case c.credits <- struct{}{}:
			default:
				return pipeline.RecognitionResult{}, fmt.Errorf("ingress credit overflow")
			}
			continue
		}
		if state := w.GetTaskStatus(); state != nil {
			if (state.AiService != "segmentation" && state.AiService != "gesture_recognition") ||
				(state.AiService == "segmentation" && c.open.Segmentation == nil) || (state.AiService == "gesture_recognition" && c.open.Gesture == nil) ||
				(state.Status != "failed" && state.Status != "closed") {
				return pipeline.RecognitionResult{}, fmt.Errorf("invalid task status")
			}
			if state.Status == "failed" {
				zap.L().Warn("ai: engine task failed", zap.String("session_id", c.open.SessionId),
					zap.String("ai_service", state.AiService), zap.String("error", state.ErrorMessage))
			}
			continue
		}
		r := w.GetResult()
		if r == nil || r.SessionId != c.open.SessionId || r.StreamId != c.open.StreamId {
			return pipeline.RecognitionResult{}, fmt.Errorf("invalid multiplexed result")
		}
		var id int64
		switch r.AiService {
		case "segmentation":
			if c.open.Segmentation == nil || r.GetMask() == nil {
				return pipeline.RecognitionResult{}, fmt.Errorf("unexpected mask")
			}
			id = r.GetMask().FrameId
		case "gesture_recognition":
			if c.open.Gesture == nil || r.GetGesture() == nil {
				return pipeline.RecognitionResult{}, fmt.Errorf("unexpected gesture")
			}
			id = r.GetGesture().FrameId
		default:
			return pipeline.RecognitionResult{}, fmt.Errorf("unknown result task")
		}
		c.mu.Lock()
		p, ok := c.pending[id]
		if !ok || time.Since(p.sent) > 10*time.Second {
			c.mu.Unlock()
			continue
		} // stale, not a wrong-call error
		if p.seen[r.AiService] || r.Seq <= c.seq[r.AiService] {
			c.mu.Unlock()
			return pipeline.RecognitionResult{}, fmt.Errorf("duplicate/nonmonotonic task result")
		}
		p.seen[r.AiService], c.seq[r.AiService] = true, r.Seq
		f := p.frame
		c.mu.Unlock()
		out := pipeline.RecognitionResult{SessionID: c.open.SessionId, StreamID: c.open.StreamId, AIService: r.AiService, Seq: r.Seq}
		if m := r.GetMask(); m != nil {
			if m.RtpTimestamp != int64(f.RTPTimestamp) || m.Status != "ok" || m.MaskWidth != int32(c.open.Segmentation.InferenceWidth) || m.MaskHeight != int32(c.open.Segmentation.InferenceHeight) {
				return out, fmt.Errorf("mask does not match source/profile")
			}
			var total uint64
			for _, n := range m.RleCounts {
				total += uint64(n)
			}
			if total != uint64(m.MaskWidth)*uint64(m.MaskHeight) {
				return out, fmt.Errorf("invalid mask RLE")
			}
			// Preserve the existing MF callback identity for masks.
			out.StreamID, out.Kind, out.IsFinal = c.open.SessionId, "video_mask", true
			out.VideoMask = maskFromProto(m)
		} else {
			g := r.GetGesture()
			if g.MediaEpoch != c.open.MediaEpoch || g.RtpTimestamp != f.RTPTimestamp || g.ExtendedRtpTimestamp != f.ExtendedRTPTimestamp ||
				g.TimestampMs != f.TimestampMs || g.OriginalWidth != uint32(f.OriginalWidth) || g.OriginalHeight != uint32(f.OriginalHeight) || len(g.Hands) > int(c.open.Gesture.MaxHands) {
				return out, fmt.Errorf("gesture does not match source")
			}
			out.Kind, out.TsStart, out.TsEnd = "video_gesture", g.TimestampMs, g.TimestampMs
			out.GestureResult = gestureFromProto(g)
			if !out.GestureResult.Valid() {
				return out, fmt.Errorf("invalid gesture snapshot")
			}
		}
		return out, nil
	}
}

func (c *sourceClient) CloseSend() error { return c.stream.CloseSend() }
func (c *sourceClient) Close() error     { c.cancel(); return nil }
