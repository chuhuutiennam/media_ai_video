package result

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"media-ai-gateway/internal/pipeline"
)

func TestOrderedGestureLanePreservesSessionAndIsolatesOtherWorkers(t *testing.T) {
	d := NewDispatcher(Config{OrderedGestures: true, Workers: 4, QueueSize: 16, SendTimeout: 10 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unlock := func() { once.Do(func() { close(release) }) }
	defer unlock()
	got := make(chan uint64, 8)
	d.RegisterSink("ordered", NewSinkFunc("capture", func(ctx context.Context, r pipeline.RecognitionResult) error {
		if r.Seq == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		got <- r.Seq
		return nil
	}))
	go d.Run(ctx)
	r := pipeline.RecognitionResult{SessionID: "ordered", Kind: "video_gesture", Seq: 1}
	if err := d.Push(ctx, r); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first event did not start")
	}
	for seq := uint64(2); seq <= 4; seq++ {
		r.Seq = seq
		if err := d.Push(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if d.TaskQueueLen("gesture_recognition") != 3 || d.QueueLen() != 3 {
		t.Fatal("queued session events escaped FIFO")
	}
	// Another session routed to another worker must not wait for this effect.
	other := ""
	for i := 0; i < 100; i++ {
		id := fmt.Sprintf("other-%d", i)
		if d.queueFor(pipeline.RecognitionResult{SessionID: id, Kind: "video_gesture"}) != d.queueFor(r) {
			other = id
			break
		}
	}
	if other == "" {
		t.Fatal("could not find another worker")
	}
	fast := newCaptureSink(1)
	d.RegisterSink(other, fast)
	if err := d.Push(ctx, pipeline.RecognitionResult{SessionID: other, Kind: "video_gesture"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fast.ch:
	case <-time.After(time.Second):
		t.Fatal("blocked session stalled another worker")
	}
	unlock()
	for want := uint64(1); want <= 4; want++ {
		select {
		case seq := <-got:
			if seq != want {
				t.Fatalf("got %d, want %d", seq, want)
			}
		case <-time.After(time.Second):
			t.Fatal("missing ordered event")
		}
	}
}
