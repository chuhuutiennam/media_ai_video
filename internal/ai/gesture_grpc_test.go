package ai

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"media-ai-gateway/internal/mockai"
	"media-ai-gateway/internal/pipeline"
)

func newJPEGTestPool(t *testing.T) *SharedConnPool {
	t.Helper()
	listener := &pipeListener{connections: make(chan net.Conn), done: make(chan struct{})}
	server := grpc.NewServer()
	mockai.RegisterVideo(server)
	go server.Serve(listener)
	t.Cleanup(func() { server.Stop(); listener.Close() })
	pool := NewSharedConnPool(0, 0)
	pool.opts = append(pool.opts, grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		client, server := net.Pipe()
		select {
		case listener.connections <- server:
			return client, nil
		case <-ctx.Done():
			client.Close()
			server.Close()
			return nil, ctx.Err()
		}
	}))
	t.Cleanup(func() {
		for _, conn := range pool.conns {
			conn.Close()
		}
	})
	return pool
}

func TestGestureTaskRoundTripAndGracefulDrain(t *testing.T) {
	pool := newJPEGTestPool(t)
	registry := NewWorkerRegistry(0)
	registry.Register(WorkerInfo{ID: "audio-wildcard", Addr: "wrong", MaxStreams: 100})
	registry.Register(WorkerInfo{ID: "video", Addr: "passthrough:///mock", Tasks: []string{"gesture_recognition"}, MediaTypes: []string{"video"}, InputFormats: []string{"jpeg"}, Protocols: []string{"video_segment"}, MaxStreams: 1})
	manager := NewVideoManager(Config{MaxActiveStreams: 2, StreamTimeout: 5 * time.Second}, NewVideoRoutingDialer(registry, pool.VideoDialFunc()))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	in := make(chan pipeline.VideoFrame, 3)
	out := make(chan pipeline.RecognitionResult, 3)
	cfg := VideoStreamConfig{AIService: "gesture_recognition", SelectedService: "change_background", MediaEpoch: "source-1", InferFPS: 15, MaxHands: 4}
	stream, err := manager.Open(ctx, "call-tcore", "call:tcore:video", cfg, in, out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Open(ctx, "another-tcore", "another:tcore:video", cfg, in, out); err == nil {
		t.Fatal("capacity reservation was bypassed")
	}
	var jpegFrame bytes.Buffer
	if err := jpeg.Encode(&jpegFrame, image.NewYCbCr(image.Rect(0, 0, 4, 4), image.YCbCrSubsampleRatio420), nil); err != nil {
		t.Fatal(err)
	}
	for i, ms := range []int64{0, 100, 2600} {
		in <- pipeline.VideoFrame{FrameID: int64(i + 1), RTPTimestamp: uint32(90000 + ms*90), ExtendedRTPTimestamp: uint64(90000 + ms*90), TimestampMs: ms,
			OriginalWidth: 4, OriginalHeight: 4, Width: 4, Height: 4, JPEG: jpegFrame.Bytes()}
	}
	close(in) // Last response must survive client half-close.
	var snapshots []*pipeline.GestureFrameResult
	for range 3 {
		select {
		case r := <-out:
			if r.Kind != "video_gesture" || r.StreamID != "call:tcore:video" || !r.GestureResult.Valid() {
				t.Fatalf("bad gesture: %+v", r)
			}
			snapshots = append(snapshots, r.GestureResult)
		case <-ctx.Done():
			t.Fatal("responses lost during drain")
		}
	}
	stream.Wait()
	if len(snapshots[0].Hands) != 2 || len(snapshots[2].Hands) != 0 {
		t.Fatal("multi-hand/empty snapshot contract failed")
	}
	if snapshots[0].Hands[0].Center[0] == snapshots[1].Hands[0].Center[0] {
		t.Fatal("position snapshots did not update")
	}
	if _, err := registry.SelectVideoTask("gesture_recognition", "gesture_recognition", "jpeg", "video_segment"); err != nil {
		t.Fatal("slot not released after EOF", err)
	}
	registry.ReleaseVideoTask("video")
}

func TestVideoRouterRejectsWildcardAndWrongFormat(t *testing.T) {
	r := NewWorkerRegistry(0)
	r.Register(WorkerInfo{ID: "wildcard", Addr: "audio"})
	r.Register(WorkerInfo{ID: "raw-only", Tasks: []string{"gesture_recognition"}, MediaTypes: []string{"video"}, InputFormats: []string{"RGB24"}, Protocols: []string{"video_segment"}})
	if _, err := r.SelectVideoTask("gesture_recognition", "gesture_recognition", "jpeg", "video_segment"); err == nil {
		t.Fatal("selected incompatible engine")
	}
}

var _ io.Closer = (*reservedVideoClient)(nil)

// In-memory gRPC transport without test-only dependencies absent from vendor.
type pipeListener struct {
	connections chan net.Conn
	done        chan struct{}
	once        sync.Once
}

func (p *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-p.connections:
		return c, nil
	case <-p.done:
		return nil, io.EOF
	}
}
func (p *pipeListener) Close() error   { p.once.Do(func() { close(p.done) }); return nil }
func (p *pipeListener) Addr() net.Addr { return &net.UnixAddr{Name: "grpc-pipe", Net: "unix"} }

func TestSegmentationTaskUsesSharedJPEGTransport(t *testing.T) {
	pool := newJPEGTestPool(t)
	registry := NewWorkerRegistry(0)
	registry.Register(WorkerInfo{ID: "video", Addr: "passthrough:///mock", Tasks: []string{"video_segment"}, MediaTypes: []string{"video"}, InputFormats: []string{"jpeg"}, Protocols: []string{"video_segment"}, MaxStreams: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg := VideoStreamConfig{AIService: "segmentation", EffectType: "bg_blur", InferWidth: 8, InferHeight: 4, InferFPS: 5}
	client, err := NewVideoRoutingDialer(registry, pool.VideoDialFunc()).Dial(ctx, "call", "video", cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.(*reservedVideoClient).Close()
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewGray(image.Rect(0, 0, 8, 4)), nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Send(pipeline.VideoFrame{FrameID: 42, Width: 8, Height: 4, OriginalWidth: 16, OriginalHeight: 8, JPEG: encoded.Bytes()}); err != nil {
		t.Fatal(err)
	}
	if err := client.CloseSend(); err != nil {
		t.Fatal(err)
	}
	result, err := client.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsVideoMask() || result.VideoMask.FrameID != 42 || result.VideoMask.MaskWidth != 8 || result.VideoMask.MaskHeight != 4 {
		t.Fatalf("bad JPEG mask: %+v", result)
	}
	if _, err := client.Recv(); err != io.EOF {
		t.Fatalf("stream did not drain: %v", err)
	}
}
