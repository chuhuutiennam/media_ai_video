package result

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"media-ai-gateway/internal/pipeline"
)

// Saturate one callback lane with a stalled HTTP/2 request. The other task
// and audio must still complete, even with just one worker per lane.
func TestTaskCallbacksIsolateBlockedDelivery(t *testing.T) {
	for _, blocked := range []string{"video_segment", "gesture_recognition"} {
		t.Run(blocked, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered, release := make(chan struct{}, 8), make(chan struct{})
			delivered := make(chan string, 8)
			srv := newH2CServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body pipeline.RecognitionResult
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				task := "default"
				if body.IsVideoGesture() {
					task = "gesture_recognition"
				} else if body.IsVideoMask() {
					task = "video_segment"
				}
				if task == blocked {
					entered <- struct{}{}
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				} else {
					delivered <- task + ":" + r.URL.Path
				}
				w.WriteHeader(200)
			}))
			defer srv.Close()
			defer close(release)
			d := NewDispatcher(Config{Workers: 1, QueueSize: 1, SendTimeout: 5 * time.Second})
			client := NewCallbackHTTPClient(5*time.Second, 0, 0)
			maskClient := client.clientFor(pipeline.RecognitionResult{Kind: "video_mask"})
			gestureClient := client.clientFor(pipeline.RecognitionResult{Kind: "video_gesture"})
			if maskClient == gestureClient || maskClient == client.client || gestureClient == client.client {
				t.Fatal("tasks share an HTTP/2 connection pool")
			}
			if client.clientFor(pipeline.RecognitionResult{Kind: "video_mask"}) != maskClient {
				t.Fatal("task connection pool is not reused across callbacks")
			}
			d.RegisterSink("s", client.NewHTTPCallbackSink(srv.URL+"/fallback", 0))
			d.ReplaceTaskHTTPSink("s", "video_segment", client.NewHTTPCallbackSink(srv.URL+"/segmentation", 0))
			d.ReplaceTaskHTTPSink("s", "gesture_recognition", client.NewHTTPCallbackSink(srv.URL+"/gesture", 0))
			done := make(chan struct{})
			go func() { d.Run(ctx); close(done) }()
			defer func() {
				cancel()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("dispatcher failed to stop")
				}
			}()
			mask := pipeline.RecognitionResult{SessionID: "s", Kind: "video_mask", IsFinal: true}
			gesture := pipeline.RecognitionResult{SessionID: "s", Kind: "video_gesture"}
			slow, fast := mask, gesture
			want := "gesture_recognition:/gesture"
			if blocked == "gesture_recognition" {
				slow, fast = gesture, mask
				want = "video_segment:/segmentation"
			}
			if err := d.Push(ctx, slow); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("blocked callback did not start")
			}
			if err := d.Push(ctx, slow); err != nil {
				t.Fatal(err)
			} // fill only the slow lane
			if d.TaskQueueLen(blocked) != 1 {
				t.Fatal("slow queue is not full")
			}
			pushCtx, stop := context.WithTimeout(ctx, time.Second)
			defer stop()
			if err := d.Push(pushCtx, fast); err != nil {
				t.Fatal(err)
			}
			if err := d.Push(pushCtx, pipeline.RecognitionResult{SessionID: "s", IsFinal: true}); err != nil {
				t.Fatal(err)
			}
			got := map[string]bool{}
			for len(got) < 2 {
				select {
				case x := <-delivered:
					got[x] = true
				case <-pushCtx.Done():
					t.Fatal("other task/audio blocked behind slow callback", got)
				}
			}
			if !got[want] || !got["default:/fallback"] {
				t.Fatalf("wrong task URL routing: %v", got)
			}
			// Removing an override restores the legacy session endpoint.
			task := "gesture_recognition"
			if fast.IsVideoMask() {
				task = "video_segment"
			}
			d.ReplaceTaskHTTPSink("s", task, nil)
			if err := d.Push(pushCtx, fast); err != nil {
				t.Fatal(err)
			}
			select {
			case x := <-delivered:
				if x != task+":/fallback" {
					t.Fatal(x)
				}
			case <-pushCtx.Done():
				t.Fatal("fallback was not restored")
			}
			d.UnregisterSinks("s")
			if len(d.taskHTTP) != 0 {
				t.Fatal("task sinks retained after cleanup")
			}
		})
	}
}
