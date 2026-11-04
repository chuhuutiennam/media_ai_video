package ai

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pb "media-ai-gateway/internal/ai/proto"
	"media-ai-gateway/internal/pipeline"
	"media-ai-gateway/workers/mockai"
)

// Each server accepts exactly one task, so a routing mistake cannot be hidden by
// the mock supporting both tasks on the same RPC.
type taskBoundStream struct {
	grpc.ServerStream
	expected string
	opened   chan string
	first    bool
}

func (s *taskBoundStream) RecvMsg(v any) error {
	if err := s.ServerStream.RecvMsg(v); err != nil {
		return err
	}
	if s.first {
		s.first = false
		request := v.(*pb.VideoRequest)
		if request.GetConfig().GetAiService() != s.expected {
			return status.Error(codes.FailedPrecondition, "task reached wrong worker")
		}
		s.opened <- s.expected
	}
	return nil
}

func TestVideoRoutingTwoGRPCWorkers(t *testing.T) {
	listeners := make(map[string]*pipeListener)
	opened := make(chan string, 2)
	for address, service := range map[string]string{"segmentation": "segmentation", "gesture": "gesture_recognition"} {
		listener := &pipeListener{connections: make(chan net.Conn), done: make(chan struct{})}
		server := grpc.NewServer(grpc.StreamInterceptor(func(srv any, stream grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			return handler(srv, &taskBoundStream{ServerStream: stream, expected: service, opened: opened, first: true})
		}))
		mockai.RegisterVideo(server)
		go server.Serve(listener)
		t.Cleanup(func() { server.Stop(); listener.Close() })
		listeners[address] = listener
	}
	pool := NewSharedConnPool(0, 0)
	pool.opts = append(pool.opts, grpc.WithContextDialer(func(ctx context.Context, address string) (net.Conn, error) {
		listener, ok := listeners[address]
		if !ok {
			return nil, fmt.Errorf("unexpected worker address %q", address)
		}
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
	registry := NewWorkerRegistry(0)
	for address, task := range map[string]string{"segmentation": "video_segment", "gesture": "gesture_recognition"} {
		worker := videoWorker(address, task)
		worker.Addr = "passthrough:///" + address
		registry.Register(worker)
	}
	router := NewVideoRoutingDialer(registry, pool.VideoDialFunc())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	segmentation, err := router.Dial(ctx, "call-tcore", "call-tcore", VideoStreamConfig{
		EffectType: "bg_blur", InferWidth: 8, InferHeight: 4, InferFPS: 5,
	}) // Exercises the coordinator's legacy empty AIService as well.
	if err != nil {
		t.Fatal(err)
	}
	defer segmentation.(*reservedVideoClient).Close()
	gesture, err := router.Dial(ctx, "call-tcore", "call:tcore:video", VideoStreamConfig{
		AIService: "gesture_recognition", SelectedService: "change_background", MediaEpoch: "source-1", InferFPS: 15, MaxHands: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer gesture.(*reservedVideoClient).Close()
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewGray(image.Rect(0, 0, 8, 4)), nil); err != nil {
		t.Fatal(err)
	}
	frame := pipeline.VideoFrame{FrameID: 77, RTPTimestamp: 9000, ExtendedRTPTimestamp: 9000,
		Width: 8, Height: 4, OriginalWidth: 16, OriginalHeight: 8, JPEG: encoded.Bytes()}
	for _, client := range []VideoStreamClient{segmentation, gesture} {
		if err := client.Send(frame); err != nil {
			t.Fatal(err)
		}
		if err := client.CloseSend(); err != nil {
			t.Fatal(err)
		}
	}
	mask, err := segmentation.Recv()
	if err != nil || !mask.IsVideoMask() || mask.VideoMask.FrameID != 77 || mask.VideoMask.Status != "ok" {
		t.Fatalf("segmentation worker failed: %+v, %v", mask, err)
	}
	hands, err := gesture.Recv()
	if err != nil || !hands.IsVideoGesture() || hands.GestureResult.FrameID != 77 || len(hands.GestureResult.Hands) != 2 {
		t.Fatalf("gesture worker failed: %+v, %v", hands, err)
	}
	for _, client := range []VideoStreamClient{segmentation, gesture} {
		if _, err := client.Recv(); err != io.EOF {
			t.Fatalf("task did not drain: %v", err)
		}
	}
	if len(opened) != 2 || len(pool.Addrs()) != 2 {
		t.Fatal("tasks did not reach two distinct worker connections")
	}
}
