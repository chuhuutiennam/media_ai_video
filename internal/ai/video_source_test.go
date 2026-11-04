package ai

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pb "media-ai-gateway/internal/ai/proto"
	"media-ai-gateway/internal/pipeline"
	"media-ai-gateway/internal/videoengine"
	"media-ai-gateway/workers/mockai"
	"sync"
)

func sourceTestEngine(t *testing.T, custom grpc.StreamHandler) (*SourceDialer, *videoengine.Engine) {
	t.Helper()
	backend := newSourceMemoryListener()
	bserver := grpc.NewServer()
	if custom == nil {
		mockai.RegisterVideo(bserver)
	} else {
		bserver.RegisterService(&grpc.ServiceDesc{ServiceName: "video.VideoStream", HandlerType: (*any)(nil),
			Streams: []grpc.StreamDesc{{StreamName: "Segment", Handler: custom, ClientStreams: true, ServerStreams: true}}}, struct{}{})
	}
	go bserver.Serve(backend)
	cfg := videoengine.DefaultConfig()
	cfg.SegmentationTarget, cfg.GestureTarget = "passthrough:///backend", "passthrough:///backend"
	cfg.TaskTimeout, cfg.MaxFrameAge = 3*time.Second, 3*time.Second
	cfg.WorkerDialOptions = []grpc.DialOption{grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return backend.DialContext(ctx) })}
	engine, err := videoengine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	front := newSourceMemoryListener()
	server := grpc.NewServer()
	engine.Register(server)
	go server.Serve(front)
	pool := NewSharedConnPool(0, 0)
	pool.opts = append(pool.opts, grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return front.DialContext(ctx) }))
	t.Cleanup(func() {
		server.Stop()
		engine.Close()
		bserver.Stop()
		front.Close()
		backend.Close()
		for _, c := range pool.conns {
			c.Close()
		}
	})
	return NewSourceDialer(pool, "passthrough:///engine", 3*time.Second), engine
}

func sourceTestConfig() VideoStreamConfig {
	return VideoStreamConfig{MediaEpoch: "media-1", SelectedService: "change_background", Source: &SourceStreamConfig{InputFPS: 15, InputMaxDimension: 32,
		Segmentation: &VideoStreamConfig{EffectType: "bg_replace", InferWidth: 4, InferHeight: 4, InferFPS: 5},
		Gesture:      &VideoStreamConfig{InferWidth: 32, InferFPS: 15, MaxHands: 2}}}
}

func sourceTestFrame(t *testing.T, id int64) pipeline.VideoFrame {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 8, 4)), nil); err != nil {
		t.Fatal(err)
	}
	return pipeline.VideoFrame{FrameID: id, RTPTimestamp: uint32(id * 9000), ExtendedRTPTimestamp: uint64(id * 9000), TimestampMs: id * 100,
		OriginalWidth: 8, OriginalHeight: 4, Width: 8, Height: 4, JPEG: b.Bytes()}
}

func engineMetric(engine *videoengine.Engine, name string) bool {
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	return strings.Contains(w.Body.String(), name+" 0\n")
}

func TestSourceOneFrameProducesTwoIndependentResults(t *testing.T) {
	d, engine := sourceTestEngine(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := d.Dial(ctx, "call-tcore", "call:tcore:video", sourceTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer c.(io.Closer).Close()
	if err = c.Send(sourceTestFrame(t, 1)); err != nil {
		t.Fatal(err)
	}
	if err = c.CloseSend(); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for {
		r, err := c.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if seen[r.AIService] {
			t.Fatal("duplicate task result")
		}
		seen[r.AIService] = true
		if r.VideoMask != nil && (r.VideoMask.FrameID != 1 || r.StreamID != "call-tcore") {
			t.Fatalf("mask callback identity: %+v", r)
		}
		if r.GestureResult != nil && (r.GestureResult.FrameID != 1 || r.StreamID != "call:tcore:video") {
			t.Fatalf("gesture callback identity: %+v", r)
		}
	}
	if !seen["segmentation"] || !seen["gesture_recognition"] {
		t.Fatalf("one input did not produce both results: %v", seen)
	}
	if !engineMetric(engine, "video_engine_sources_active") {
		t.Fatal("source reservation leaked after half-close")
	}
}

func TestSourceOnlyEnabledTasksAndCancellation(t *testing.T) {
	for _, name := range []string{"segmentation", "gesture_recognition"} {
		t.Run(name, func(t *testing.T) {
			d, engine := sourceTestEngine(t, nil)
			cfg := sourceTestConfig()
			if name == "segmentation" {
				cfg.Source.Gesture = nil
			} else {
				cfg.Source.Segmentation = nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c, err := d.Dial(ctx, "call-tcore", "call:tcore:video", cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err = c.Send(sourceTestFrame(t, 1)); err != nil {
				t.Fatal(err)
			}
			r, err := c.Recv()
			if err != nil || r.AIService != name {
				t.Fatalf("disabled task ran: %+v %v", r, err)
			}
			c.(io.Closer).Close()
			deadline := time.Now().Add(time.Second)
			for !engineMetric(engine, "video_engine_sources_active") && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if !engineMetric(engine, "video_engine_sources_active") || !engineMetric(engine, "video_engine_segmentation_active") || !engineMetric(engine, "video_engine_gesture_active") {
				t.Fatal("reservations leaked on cancellation")
			}
		})
	}
}

func controlledSourceBackend(gate <-chan struct{}, failMask bool) grpc.StreamHandler {
	return func(_ any, s grpc.ServerStream) error {
		var first pb.VideoRequest
		if err := s.RecvMsg(&first); err != nil {
			return err
		}
		cfg := first.GetConfig()
		for {
			var req pb.VideoRequest
			if err := s.RecvMsg(&req); err != nil {
				if err == io.EOF {
					return nil
				}
				return err
			}
			f := req.GetFrame()
			if f == nil {
				return nil
			}
			r := &pb.VideoResponse{SessionId: req.SessionId, StreamId: req.StreamId, AiService: cfg.AiService}
			if cfg.AiService == "segmentation" {
				if failMask {
					return status.Error(codes.Unavailable, "simulated segmentation failure")
				}
				select {
				case <-gate:
				case <-s.Context().Done():
					return s.Context().Err()
				}
				r.Result = &pb.VideoResponse_Mask{Mask: &pb.VideoMask{FrameId: f.FrameId, RtpTimestamp: f.RtpTimestamp, MaskWidth: int32(f.Width), MaskHeight: int32(f.Height), RleCounts: []uint32{f.Width * f.Height}, Status: "ok"}}
			} else {
				r.Result = &pb.VideoResponse_Gesture{Gesture: mockai.Snapshot(cfg, f, "recognizer-1", uint64(f.FrameId))}
			}
			if err := s.SendMsg(r); err != nil {
				return err
			}
		}
	}
}

func TestSourceSlowOrFailedMaskDoesNotBlockGesture(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "slow", true: "failed"}[fail], func(t *testing.T) {
			gate := make(chan struct{})
			defer close(gate)
			d, _ := sourceTestEngine(t, controlledSourceBackend(gate, fail))
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			c, err := d.Dial(ctx, "call-tcore", "call:tcore:video", sourceTestConfig())
			if err != nil {
				t.Fatal(err)
			}
			defer c.(io.Closer).Close()
			if err = c.Send(sourceTestFrame(t, 1)); err != nil {
				t.Fatal(err)
			}
			r, err := c.Recv()
			if err != nil || r.GestureResult == nil {
				t.Fatalf("gesture waited for segmentation: %+v %v", r, err)
			}
			// More than the ingress credit window. Recv must consume ACKs even
			// while the other model is stalled or no longer returns results.
			done := make(chan error, 1)
			go func() {
				for {
					_, err := c.Recv()
					if err != nil {
						done <- err
						return
					}
				}
			}()
			for id := int64(2); id <= 12; id++ {
				if err = c.Send(sourceTestFrame(t, id)); err != nil {
					t.Fatalf("credits stalled: %v", err)
				}
			}
			c.(io.Closer).Close()
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("source receive did not cancel")
			}
		})
	}
}

func TestSourceManagerDrainsIngressCreditsWithFullCallbackQueue(t *testing.T) {
	d, _ := sourceTestEngine(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m := NewVideoManager(Config{MaxActiveStreams: 2, SendTimeout: time.Second}, d)
	in := make(chan pipeline.VideoFrame, 16)
	for id := int64(1); id <= 12; id++ {
		in <- sourceTestFrame(t, id)
	}
	close(in)
	s, err := m.Open(ctx, "call-tcore", "call:tcore:video", sourceTestConfig(), in, make(chan pipeline.RecognitionResult))
	if err != nil {
		t.Fatal(err)
	}
	s.Wait()
	stats := m.Stats()
	if s.Err() != nil || stats.FramesSent != 12 || stats.GestureDropped+stats.MaskDropped == 0 {
		t.Fatalf("callback queue blocked source progress: err=%v stats=%+v", s.Err(), stats)
	}
}

func TestSourceInvalidJPEGReleasesReservations(t *testing.T) {
	d, engine := sourceTestEngine(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := d.Dial(ctx, "call-tcore", "call:tcore:video", sourceTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer c.(io.Closer).Close()
	f := sourceTestFrame(t, 1)
	f.JPEG = []byte("not a JPEG")
	if err = c.Send(f); err != nil {
		t.Fatal(err)
	}
	_, err = c.Recv()
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("corrupt source accepted: %v", err)
	}
	if !engineMetric(engine, "video_engine_sources_active") || !engineMetric(engine, "video_engine_segmentation_active") || !engineMetric(engine, "video_engine_gesture_active") {
		t.Fatal("invalid source leaked model reservations")
	}
	// A failed source must not leave a duplicate-source tombstone.
	c2, err := d.Dial(ctx, "call-tcore", "call:tcore:video", sourceTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	c2.(io.Closer).Close()
}

func TestSourceDuplicateOpenDoesNotReleaseLiveSource(t *testing.T) {
	d, _ := sourceTestEngine(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := d.Dial(ctx, "call-tcore", "call:tcore:video", sourceTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer c.(io.Closer).Close()
	if _, err = d.Dial(ctx, "call-tcore", "call:tcore:video", sourceTestConfig()); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("duplicate source was admitted: %v", err)
	}
	if err = c.Send(sourceTestFrame(t, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Recv(); err != nil {
		t.Fatalf("duplicate open damaged original source: %v", err)
	}
}

// net.Pipe keeps these transport tests self-contained without extra vendored
// grpc test packages or a permission to bind localhost ports.
type sourceMemoryListener struct {
	incoming chan net.Conn
	done     chan struct{}
	once     sync.Once
}

func newSourceMemoryListener() *sourceMemoryListener {
	return &sourceMemoryListener{incoming: make(chan net.Conn), done: make(chan struct{})}
}
func (l *sourceMemoryListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.incoming:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *sourceMemoryListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *sourceMemoryListener) Addr() net.Addr { return sourceMemoryAddr("source-test") }
func (l *sourceMemoryListener) DialContext(ctx context.Context) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.incoming <- server:
		return client, nil
	case <-ctx.Done():
		client.Close()
		server.Close()
		return nil, ctx.Err()
	case <-l.done:
		client.Close()
		server.Close()
		return nil, net.ErrClosed
	}
}

type sourceMemoryAddr string

func (a sourceMemoryAddr) Network() string { return "memory" }
func (a sourceMemoryAddr) String() string  { return string(a) }
