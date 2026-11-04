package coordinator

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"media-ai-gateway/internal/ai"
	"media-ai-gateway/internal/pipeline"
	"media-ai-gateway/internal/session"
)

type isolatedTaskDialer struct {
	mu      sync.Mutex
	clients map[string]*fakeVideoClient
}

func (d *isolatedTaskDialer) Dial(ctx context.Context, _, _ string, cfg ai.VideoStreamConfig) (ai.VideoStreamClient, error) {
	c := &fakeVideoClient{ctx: ctx, frames: make(chan pipeline.VideoFrame, 1), results: make(chan pipeline.RecognitionResult, 1)}
	d.mu.Lock()
	d.clients[cfg.AIService] = c
	d.mu.Unlock()
	return c, nil
}
func TestVideoTasksUseSeparateResultPumpsAndPreserveCallbacks(t *testing.T) {
	coord, pool, _ := newVideoStack(t)
	coord.cfg.GestureEnabled = true
	coord.cfg.GestureFPS, coord.cfg.GestureQueueSize = 15, 2
	coord.cfg.GestureMaxDimension, coord.cfg.GestureMaxHands = 64, 2
	dialer := &isolatedTaskDialer{clients: make(map[string]*fakeVideoClient)}
	coord.WithVideo(pool, ai.NewVideoManager(ai.Config{MaxActiveStreams: 4}, dialer))
	received := make(chan pipeline.RecognitionResult, 4)
	server := newH2CTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body pipeline.RecognitionResult
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		received <- body
		w.WriteHeader(200)
	}))
	defer server.Close()
	mgr := newVideoMgr()
	sess, err := mgr.Create(session.SessionConfig{ID: "isolated", SourceType: "raw_rtp", VideoEnabled: true, CallbackURL: server.URL, VideoInferWidth: 64, VideoInferHeight: 64, VideoInferFPS: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(sess.ID)
	if sess.VideoMaskResultQueue == nil || sess.GestureResultQueue == nil || sess.VideoMaskResultQueue == sess.GestureResultQueue || sess.VideoMaskResultQueue == sess.ResultQueue || sess.GestureResultQueue == sess.ResultQueue {
		t.Fatal("task result queues are not independent")
	}
	if _, err = coord.Start(sess); err != nil {
		t.Fatal(err)
	}
	if err = coord.StartVideo(sess); err != nil {
		t.Fatal(err)
	}
	dialer.mu.Lock()
	mask, gesture := dialer.clients[""], dialer.clients["gesture_recognition"]
	dialer.mu.Unlock()
	if mask == nil || gesture == nil || mask == gesture {
		t.Fatal("AI task streams are not independent")
	}
	mask.results <- pipeline.RecognitionResult{SessionID: sess.ID, Kind: "video_mask", IsFinal: true, VideoMask: &pipeline.VideoMask{FrameID: 1, Status: "ok"}}
	gesture.results <- pipeline.RecognitionResult{SessionID: sess.ID, Kind: "video_gesture", AIService: "gesture_recognition", GestureResult: &pipeline.GestureFrameResult{FrameID: 2}}
	got := map[string]bool{}
	for len(got) < 2 {
		select {
		case r := <-received:
			if r.SessionID != sess.ID || r.SourceType != "raw_rtp" {
				t.Fatalf("callback identity changed: %+v", r)
			}
			got[r.Kind] = true
		case <-time.After(2 * time.Second):
			t.Fatal("task callback missing", got)
		}
	}
	if !got["video_mask"] || !got["video_gesture"] {
		t.Fatal(got)
	}
}
