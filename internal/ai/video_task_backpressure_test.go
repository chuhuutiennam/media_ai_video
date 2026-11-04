package ai

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	pb "media-ai-gateway/internal/ai/proto"
	"media-ai-gateway/internal/pipeline"
)

// A backend that accepts sends immediately but lets the test delay replies.
// This models gRPC buffering without CPU/model timing or network dependencies.
type pendingGestureStream struct {
	ctx     context.Context
	replies chan *pb.VideoResponse
	sent    chan int64
	sendErr error
}

func (*pendingGestureStream) Header() (metadata.MD, error) { return nil, nil }
func (*pendingGestureStream) Trailer() metadata.MD         { return nil }
func (*pendingGestureStream) CloseSend() error             { return nil }
func (s *pendingGestureStream) Context() context.Context   { return s.ctx }
func (s *pendingGestureStream) SendMsg(value any) error {
	if s.sendErr != nil {
		return s.sendErr
	}
	if f := value.(*pb.VideoRequest).GetFrame(); f != nil {
		s.sent <- f.FrameId
	}
	return nil
}
func (s *pendingGestureStream) RecvMsg(value any) error {
	select {
	case reply := <-s.replies:
		proto.Merge(value.(proto.Message), reply)
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

func pendingGestureClient(t *testing.T) (*videoTaskClient, *pendingGestureStream) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stream := &pendingGestureStream{ctx: ctx, replies: make(chan *pb.VideoResponse, 1), sent: make(chan int64, 256)}
	return &videoTaskClient{stream: stream, cancel: cancel, session: "s", id: "s:video", epoch: "media",
		service: "gesture_recognition", maxHands: 1, pending: make(map[int64]pipeline.VideoFrame)}, stream
}

func pendingGestureFrame(id int64) pipeline.VideoFrame {
	return pipeline.VideoFrame{FrameID: id, RTPTimestamp: uint32(id * 6000), ExtendedRTPTimestamp: uint64(id * 6000),
		TimestampMs: id * 66, Width: 4, Height: 4, OriginalWidth: 4, OriginalHeight: 4, JPEG: []byte{1}}
}

func pendingGestureReply(id int64) *pb.VideoResponse {
	f := pendingGestureFrame(id)
	return &pb.VideoResponse{SessionId: "s", StreamId: "s:video", AiService: "gesture_recognition",
		Result: &pb.VideoResponse_Gesture{Gesture: &pb.GestureResult{MediaEpoch: "media", RecognizerEpoch: "task",
			ResponseId: fmt.Sprintf("task/%d", id), FrameId: id, RtpTimestamp: f.RTPTimestamp,
			ExtendedRtpTimestamp: f.ExtendedRTPTimestamp, TimestampMs: f.TimestampMs,
			OriginalWidth: 4, OriginalHeight: 4, Status: "ok"}}}
}

func fillPendingGesture(t *testing.T, client *videoTaskClient, stream *pendingGestureStream) {
	t.Helper()
	for id := int64(1); id <= 128; id++ {
		if err := client.Send(pendingGestureFrame(id)); err != nil {
			t.Fatal(err)
		}
		<-stream.sent
	}
}

func TestGesturePendingWindowPreservesUnansweredFrames(t *testing.T) {
	client, stream := pendingGestureClient(t)
	fillPendingGesture(t, client, stream)
	done := make(chan error, 1)
	go func() { done <- client.Send(pendingGestureFrame(129)) }()
	select {
	case err := <-done:
		client.mu.Lock()
		_, retained := client.pending[1]
		client.mu.Unlock()
		stream.replies <- pendingGestureReply(1)
		_, replyErr := client.Recv()
		t.Fatalf("frame 129 bypassed pending window: send=%v oldest_retained=%v delayed_reply=%v", err, retained, replyErr)
	case <-time.After(100 * time.Millisecond):
	}
	client.mu.Lock()
	_, oldestRetained := client.pending[1]
	count := len(client.pending)
	client.mu.Unlock()
	if !oldestRetained || count != 128 {
		t.Fatalf("pending metadata lost while waiting: oldest=%v count=%d", oldestRetained, count)
	}
	stream.replies <- pendingGestureReply(1)
	result, err := client.Recv()
	if err != nil || result.GestureResult == nil || result.GestureResult.FrameID != 1 {
		t.Fatalf("valid delayed reply was rejected: result=%+v err=%v", result, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("reply did not release capacity for frame 129")
	}
	if got := <-stream.sent; got != 129 {
		t.Fatal(got)
	}
	// Drain all frames, proving the capacity bound does not discard results.
	for id := int64(2); id <= 129; id++ {
		stream.replies <- pendingGestureReply(id)
		if _, err := client.Recv(); err != nil {
			t.Fatalf("frame %d: %v", id, err)
		}
	}
	if len(client.pending) != 0 {
		t.Fatal("metadata retained after drain")
	}
}

func TestGesturePendingWindowCancellationUnblocksSend(t *testing.T) {
	client, stream := pendingGestureClient(t)
	fillPendingGesture(t, client, stream)
	done := make(chan error, 1)
	go func() { done <- client.Send(pendingGestureFrame(129)) }()
	select {
	case err := <-done:
		t.Fatalf("send did not wait for pending capacity: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	client.Close()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("closing the task left a sender blocked")
	}
}

func TestGestureFailedSendReleasesPendingMetadata(t *testing.T) {
	client, stream := pendingGestureClient(t)
	stream.sendErr = errors.New("send failed")
	if err := client.Send(pendingGestureFrame(1)); err == nil {
		t.Fatal("send failure hidden")
	}
	if len(client.pending) != 0 {
		t.Fatal("failed send consumed pending capacity")
	}
}
