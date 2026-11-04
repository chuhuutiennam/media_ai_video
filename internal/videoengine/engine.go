// Package videoengine admits one source per video leg and fans immutable decoded
// JPEGs out to isolated task queues. Model workers keep their legacy Segment RPC.
package videoengine

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	pb "media-ai-gateway/internal/ai/proto"
)

type Config struct {
	WorkerDialOptions                       []grpc.DialOption // optional transport options (also used by in-memory tests)
	SegmentationTarget, GestureTarget       string
	MaxSources, MaxSegmentation, MaxGesture int
	QueueSize, MaxInflight, JPEGQuality     int
	OpenTimeout, TaskTimeout, MaxFrameAge   time.Duration
}

func DefaultConfig() Config {
	return Config{MaxSources: 64, MaxSegmentation: 4, MaxGesture: 4, QueueSize: 2, MaxInflight: 4,
		JPEGQuality: 80, OpenTimeout: 5 * time.Second, TaskTimeout: 5 * time.Second, MaxFrameAge: 500 * time.Millisecond}
}

type Engine struct {
	cfg                                                   Config
	conns                                                 map[string]*grpc.ClientConn
	mu                                                    sync.Mutex
	sources                                               map[string]bool
	active                                                map[string]int
	frames, dropped, resultDrops, masks, gestures, errors atomic.Uint64
	segFrames, gestureFrames                              atomic.Uint64
}

func New(cfg Config) (*Engine, error) {
	if cfg.MaxSources < 1 || cfg.MaxSegmentation < 1 || cfg.MaxGesture < 1 || cfg.QueueSize < 1 || cfg.QueueSize > 32 ||
		cfg.MaxInflight < 1 || cfg.MaxInflight > 64 || cfg.JPEGQuality < 1 || cfg.JPEGQuality > 100 ||
		cfg.OpenTimeout <= 0 || cfg.TaskTimeout <= 0 || cfg.MaxFrameAge <= 0 {
		return nil, fmt.Errorf("invalid video engine limits")
	}
	e := &Engine{cfg: cfg, conns: make(map[string]*grpc.ClientConn), sources: make(map[string]bool), active: make(map[string]int)}
	for _, addr := range []string{cfg.SegmentationTarget, cfg.GestureTarget} {
		if addr == "" || e.conns[addr] != nil {
			continue
		}
		opts := append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, cfg.WorkerDialOptions...)
		c, err := grpc.NewClient(addr, opts...)
		if err != nil {
			e.Close()
			return nil, err
		}
		e.conns[addr] = c
	}
	return e, nil
}

func (e *Engine) Close() {
	for _, c := range e.conns {
		_ = c.Close()
	}
}

func (e *Engine) Register(s *grpc.Server) {
	s.RegisterService(&grpc.ServiceDesc{ServiceName: "video.VideoAIService", HandlerType: (*any)(nil),
		Streams: []grpc.StreamDesc{{StreamName: "Process", ClientStreams: true, ServerStreams: true,
			Handler: func(_ any, stream grpc.ServerStream) error { return e.process(stream) }}}}, e)
}

func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/metrics" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	e.mu.Lock()
	sources, seg, gesture := len(e.sources), e.active["segmentation"], e.active["gesture_recognition"]
	e.mu.Unlock()
	for name, value := range map[string]uint64{"sources_active": uint64(sources), "segmentation_active": uint64(seg),
		"gesture_active": uint64(gesture), "source_frames_total": e.frames.Load(), "task_frames_dropped_total": e.dropped.Load(),
		"result_drops_total": e.resultDrops.Load(), "mask_results_total": e.masks.Load(), "gesture_results_total": e.gestures.Load(),
		"task_errors_total": e.errors.Load(), "segmentation_frames_total": e.segFrames.Load(), "gesture_frames_total": e.gestureFrames.Load()} {
		fmt.Fprintf(w, "video_engine_%s %d\n", name, value)
	}
}

func validateOpen(o *pb.SourceOpen) error {
	if o == nil || o.SessionId == "" || o.StreamId == "" || o.MediaEpoch == "" || o.InputFps < 1 || o.InputFps > 60 ||
		o.InputMaxDimension < 32 || o.InputMaxDimension > 1920 || (o.Segmentation == nil && o.Gesture == nil) {
		return status.Error(codes.InvalidArgument, "expected source open with identity, JPEG input profile and tasks")
	}
	for _, c := range []*pb.VideoConfig{o.Segmentation, o.Gesture} {
		if c == nil {
			continue
		}
		if c.InferenceFps < 1 || c.InferenceFps > o.InputFps || c.InputFormat != "jpeg" || c.MediaEpoch != o.MediaEpoch ||
			math.IsNaN(float64(c.Threshold)) || c.Threshold < 0 || c.Threshold > 1 {
			return status.Error(codes.InvalidArgument, "invalid task profile")
		}
	}
	if c := o.Segmentation; c != nil && (c.AiService != "segmentation" || c.InferenceWidth < 1 || c.InferenceHeight < 1 ||
		max(c.InferenceWidth, c.InferenceHeight) > o.InputMaxDimension || (c.EffectType != "bg_blur" && c.EffectType != "bg_replace" && c.EffectType != "bg_remove")) {
		return status.Error(codes.InvalidArgument, "invalid segmentation profile")
	}
	if c := o.Gesture; c != nil && (c.AiService != "gesture_recognition" || c.MaxHands < 1 || c.MaxHands > 8 ||
		c.InferenceWidth < 32 || c.InferenceWidth > o.InputMaxDimension || c.InferenceHeight != 0) {
		return status.Error(codes.InvalidArgument, "gesture inference_width is the maximum edge; inference_height must be zero")
	}
	return nil
}

// Admission is atomic across all requested tasks, before any worker network I/O.
func (e *Engine) reserve(o *pb.SourceOpen) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	key := o.SessionId + "/" + o.StreamId
	if e.sources[key] {
		return status.Error(codes.AlreadyExists, "source already open")
	}
	if len(e.sources) >= e.cfg.MaxSources || (o.Segmentation != nil && e.active["segmentation"] >= e.cfg.MaxSegmentation) ||
		(o.Gesture != nil && e.active["gesture_recognition"] >= e.cfg.MaxGesture) {
		return status.Error(codes.ResourceExhausted, "engine capacity exhausted")
	}
	if (o.Segmentation != nil && e.cfg.SegmentationTarget == "") || (o.Gesture != nil && e.cfg.GestureTarget == "") {
		return status.Error(codes.Unavailable, "requested task is not configured")
	}
	e.sources[key] = true
	if o.Segmentation != nil {
		e.active["segmentation"]++
	}
	if o.Gesture != nil {
		e.active["gesture_recognition"]++
	}
	return nil
}

type sourceFrame struct {
	wire    *pb.VideoFrame
	image   image.Image
	arrived time.Time
}
type task struct {
	service    string
	profile    *pb.VideoConfig
	ctx        context.Context
	cancel     context.CancelFunc
	stream     grpc.ClientStream
	input      chan *sourceFrame
	output     chan *pb.VideoResponse
	lastSample int64
	sampled    bool
}

func latest[T any](q chan T, v T) bool {
	select {
	case q <- v:
		return false
	default:
	}
	dropped := false
	select {
	case <-q:
		dropped = true
	default:
	}
	select {
	case q <- v:
	default:
		dropped = true
	}
	return dropped
}

func (e *Engine) process(stream grpc.ServerStream) error {
	var first pb.SourceRequest
	if err := stream.RecvMsg(&first); err != nil {
		return err
	}
	o := first.GetOpen()
	if err := validateOpen(o); err != nil {
		return err
	}
	if err := e.reserve(o); err != nil {
		return err
	}
	reserved := make(map[string]bool)
	if o.Segmentation != nil {
		reserved["segmentation"] = true
	}
	if o.Gesture != nil {
		reserved["gesture_recognition"] = true
	}
	releaseTask := func(name string) {
		e.mu.Lock()
		if reserved[name] {
			delete(reserved, name)
			e.active[name]--
		}
		e.mu.Unlock()
	}
	defer func() {
		e.mu.Lock()
		delete(e.sources, o.SessionId+"/"+o.StreamId)
		for name := range reserved {
			e.active[name]--
		}
		e.mu.Unlock()
	}()
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	epoch := uuid.NewString()
	wrap := func() *pb.SourceResponse {
		return &pb.SourceResponse{SessionId: o.SessionId, StreamId: o.StreamId, MediaEpoch: o.MediaEpoch, EngineEpoch: epoch}
	}
	tasks := make([]*task, 0, 2)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		for _, t := range tasks {
			t.cancel()
		}
		wg.Wait()
	}()
	for _, entry := range []struct {
		name, addr string
		cfg        *pb.VideoConfig
	}{
		{"segmentation", e.cfg.SegmentationTarget, o.Segmentation}, {"gesture_recognition", e.cfg.GestureTarget, o.Gesture}} {
		if entry.cfg == nil {
			continue
		}
		tctx, tcancel := context.WithCancel(ctx)
		timer := time.AfterFunc(e.cfg.OpenTimeout, tcancel)
		cs, err := e.conns[entry.addr].NewStream(tctx, &grpc.StreamDesc{StreamName: "Segment", ClientStreams: true, ServerStreams: true}, pb.Method,
			grpc.MaxCallSendMsgSize(8<<20), grpc.MaxCallRecvMsgSize(8<<20))
		profile := proto.Clone(entry.cfg).(*pb.VideoConfig)
		if entry.name == "gesture_recognition" {
			profile.InferenceWidth, profile.InferenceHeight = 0, 0
		}
		if err == nil {
			err = cs.SendMsg(&pb.VideoRequest{SessionId: o.SessionId, StreamId: o.StreamId, Payload: &pb.VideoRequest_Config{Config: profile}})
		}
		timer.Stop()
		if err != nil {
			tcancel()
			return status.Errorf(codes.Unavailable, "open %s: %v", entry.name, err)
		}
		tasks = append(tasks, &task{service: entry.name, profile: entry.cfg, ctx: tctx, cancel: tcancel, stream: cs, input: make(chan *sourceFrame, e.cfg.QueueSize), output: make(chan *pb.VideoResponse, 1)})
	}
	ready := wrap()
	names := make([]string, 0, len(tasks))
	for _, t := range tasks {
		names = append(names, t.service)
	}
	ready.Payload = &pb.SourceResponse_Opened{Opened: &pb.SourceOpened{Tasks: names, MaxInflight: uint32(e.cfg.MaxInflight)}}
	// Admitted/config sent is explicit; model failures after admission are TaskStatus.
	if err := stream.SendMsg(ready); err != nil {
		return err
	}
	completed := make(chan *pb.VideoTaskStatus, len(tasks))
	for _, t := range tasks {
		wg.Add(1)
		go func(t *task) {
			defer wg.Done()
			defer releaseTask(t.service)
			defer t.cancel()
			err := e.runTask(t, o)
			state := &pb.VideoTaskStatus{AiService: t.service, Status: "closed"}
			if err != nil && ctx.Err() == nil {
				e.errors.Add(1)
				state.Status, state.ErrorMessage = "failed", err.Error()
			}
			completed <- state
		}(t)
	}
	acks := make(chan int64, e.cfg.MaxInflight)
	inputDone := make(chan error, 1)
	go func() {
		// Recv is released by gRPC when this handler returns; never wait for it
		// while returning on a task failure or client cancellation.
		defer func() {
			for _, t := range tasks {
				close(t.input)
			}
		}()
		inputDone <- e.receive(ctx, stream, o, tasks, acks)
	}()
	var q0, q1 <-chan *pb.VideoResponse
	q0 = tasks[0].output
	if len(tasks) > 1 {
		q1 = tasks[1].output
	}
	finished, failed := 0, 0
	eof := false
	sendResult := func(r *pb.VideoResponse) error {
		w := wrap()
		w.Payload = &pb.SourceResponse_Result{Result: r}
		return stream.SendMsg(w)
	}
	for {
		if eof && finished == len(tasks) {
			// Tasks signal completion only after publishing their final result.
			for {
				select {
				case id := <-acks:
					w := wrap()
					w.Payload = &pb.SourceResponse_Accepted{Accepted: &pb.FrameAccepted{FrameId: id}}
					if err := stream.SendMsg(w); err != nil {
						return err
					}
				case r := <-q0:
					if err := sendResult(r); err != nil {
						return err
					}
				case r := <-q1:
					if err := sendResult(r); err != nil {
						return err
					}
				default:
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-inputDone:
			if err != nil {
				return err
			}
			eof = true
			inputDone = nil
		case state := <-completed:
			finished++
			if state.Status == "failed" {
				failed++
			}
			w := wrap()
			w.Payload = &pb.SourceResponse_TaskStatus{TaskStatus: state}
			if err := stream.SendMsg(w); err != nil {
				return err
			}
			if failed == len(tasks) {
				return status.Error(codes.Unavailable, "all source tasks failed")
			}
		case id := <-acks:
			w := wrap()
			w.Payload = &pb.SourceResponse_Accepted{Accepted: &pb.FrameAccepted{FrameId: id}}
			if err := stream.SendMsg(w); err != nil {
				return err
			}
		case r := <-q0:
			if err := sendResult(r); err != nil {
				return err
			}
		case r := <-q1:
			if err := sendResult(r); err != nil {
				return err
			}
		}
	}
}

func (e *Engine) receive(ctx context.Context, stream grpc.ServerStream, o *pb.SourceOpen, tasks []*task, acks chan<- int64) error {
	var lastID int64
	lastTS := int64(-1)
	for {
		var req pb.SourceRequest
		if err := stream.RecvMsg(&req); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if close, ok := req.Payload.(*pb.SourceRequest_Close); ok && close.Close {
			return nil
		}
		f := req.GetFrame()
		if f == nil || f.FrameId <= lastID || f.TimestampMs <= lastTS || f.Encoding != "jpeg" || f.RtpTimestamp < 0 || f.RtpTimestamp > math.MaxUint32 ||
			f.OriginalWidth < 1 || f.OriginalHeight < 1 || f.OriginalWidth > 16384 || f.OriginalHeight > 16384 ||
			f.Width < 1 || f.Height < 1 || max(f.Width, f.Height) > o.InputMaxDimension || len(f.ImageData) > 8<<20 {
			return status.Error(codes.InvalidArgument, "invalid/nonmonotonic source frame")
		}
		size, err := jpeg.DecodeConfig(bytes.NewReader(f.ImageData))
		if err != nil || size.Width != int(f.Width) || size.Height != int(f.Height) {
			return status.Error(codes.InvalidArgument, "JPEG dimensions do not match source frame")
		}
		arrived := time.Now()
		img, err := jpeg.Decode(bytes.NewReader(f.ImageData))
		if err != nil {
			return status.Error(codes.InvalidArgument, "invalid JPEG")
		}
		item := &sourceFrame{wire: f, image: img, arrived: arrived}
		lastID, lastTS = f.FrameId, f.TimestampMs
		e.frames.Add(1)
		for _, t := range tasks {
			if t.ctx.Err() != nil {
				continue
			}
			if t.sampled && f.TimestampMs-t.lastSample < int64(1000/t.profile.InferenceFps) {
				continue
			}
			t.sampled, t.lastSample = true, f.TimestampMs
			if latest(t.input, item) {
				e.dropped.Add(1)
			}
		}
		select {
		case acks <- f.FrameId:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (e *Engine) runTask(t *task, o *pb.SourceOpen) error {
	var seq uint64
	for {
		select {
		case <-t.ctx.Done():
			return t.ctx.Err()
		case item, ok := <-t.input:
			if !ok {
				return t.stream.CloseSend()
			}
			if time.Since(item.arrived) > e.cfg.MaxFrameAge {
				e.dropped.Add(1)
				continue
			}
			f, err := e.prepare(item, t)
			if err != nil {
				return err
			}
			deadline := time.AfterFunc(e.cfg.TaskTimeout, t.cancel)
			err = t.stream.SendMsg(&pb.VideoRequest{SessionId: o.SessionId, StreamId: o.StreamId, Payload: &pb.VideoRequest_Frame{Frame: f}})
			var result pb.VideoResponse
			if err == nil {
				err = t.stream.RecvMsg(&result)
			}
			deadline.Stop()
			if err != nil {
				return err
			}
			if result.SessionId != o.SessionId || result.StreamId != o.StreamId {
				return fmt.Errorf("worker result identity mismatch")
			}
			if t.service == "segmentation" {
				m := result.GetMask()
				if m == nil || m.FrameId != f.FrameId || m.RtpTimestamp != f.RtpTimestamp || m.Status != "ok" || m.MaskWidth != int32(f.Width) || m.MaskHeight != int32(f.Height) {
					return fmt.Errorf("invalid worker mask")
				}
				e.masks.Add(1)
				e.segFrames.Add(1)
			} else {
				g := result.GetGesture()
				if g == nil || g.FrameId != f.FrameId || g.MediaEpoch != o.MediaEpoch || g.RtpTimestamp != uint32(f.RtpTimestamp) ||
					g.ExtendedRtpTimestamp != f.ExtendedRtpTimestamp || g.TimestampMs != f.TimestampMs || g.OriginalWidth != uint32(f.OriginalWidth) ||
					g.OriginalHeight != uint32(f.OriginalHeight) || g.Status != "ok" || len(g.Hands) > int(t.profile.MaxHands) {
					return fmt.Errorf("invalid worker gesture")
				}
				e.gestures.Add(1)
				e.gestureFrames.Add(1)
			}
			seq++
			result.Seq, result.AiService = seq, t.service
			if latest(t.output, &result) {
				e.resultDrops.Add(1)
			}
		}
	}
}

func (e *Engine) prepare(item *sourceFrame, t *task) (*pb.VideoFrame, error) {
	src := item.wire
	f := &pb.VideoFrame{FrameId: src.FrameId, RtpTimestamp: src.RtpTimestamp, ExtendedRtpTimestamp: src.ExtendedRtpTimestamp,
		TimestampMs: src.TimestampMs, OriginalWidth: src.OriginalWidth, OriginalHeight: src.OriginalHeight,
		Width: src.Width, Height: src.Height, Encoding: src.Encoding, ImageData: src.ImageData}
	w, h := int(f.Width), int(f.Height)
	if t.service == "segmentation" {
		w, h = int(t.profile.InferenceWidth), int(t.profile.InferenceHeight)
	} else if edge := int(t.profile.InferenceWidth); max(w, h) > edge {
		longest := max(w, h)
		w, h = max(1, w*edge/longest), max(1, h*edge/longest)
	}
	if w == int(f.Width) && h == int(f.Height) {
		return f, nil
	}
	// Each worker owns its resized buffer. The decoded input image is read-only.
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	bounds := item.image.Bounds()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dst.Set(x, y, item.image.At(bounds.Min.X+x*bounds.Dx()/w, bounds.Min.Y+y*bounds.Dy()/h))
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, dst, &jpeg.Options{Quality: e.cfg.JPEGQuality}); err != nil {
		return nil, err
	}
	f.Width, f.Height, f.ImageData = uint32(w), uint32(h), b.Bytes()
	return f, nil
}
