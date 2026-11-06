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
	clients map[string]*fakeVideoClient // session ID → client
	tasks   map[string][]string         // session ID → AIService đã mở
}

func (d *isolatedTaskDialer) Dial(ctx context.Context, sessionID, _ string, cfg ai.VideoStreamConfig) (ai.VideoStreamClient, error) {
	c := &fakeVideoClient{ctx: ctx, frames: make(chan pipeline.VideoFrame, 1), results: make(chan pipeline.RecognitionResult, 1)}
	d.mu.Lock()
	d.clients[sessionID] = c
	d.tasks[sessionID] = append(d.tasks[sessionID], cfg.AIService)
	d.mu.Unlock()
	return c, nil
}

// 1 dịch vụ ↔ 1 AI: mỗi session video mở đúng một AI stream; mask và gesture đi qua
// queue/pump riêng và callback giữ nguyên identity của session.
func TestVideoTasksUseSeparateResultPumpsAndPreserveCallbacks(t *testing.T) {
	coord, pool, _ := newVideoStack(t)
	coord.cfg.GestureEnabled = true
	coord.cfg.GestureFPS, coord.cfg.GestureQueueSize = 15, 2
	coord.cfg.GestureMaxDimension, coord.cfg.GestureMaxHands = 64, 2
	dialer := &isolatedTaskDialer{clients: make(map[string]*fakeVideoClient), tasks: make(map[string][]string)}
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
	sessions := map[string]*session.Session{}
	for _, def := range []struct{ id, service, task string }{
		{"isolated-mask", "change_background", "video_segment"},
		{"isolated-gesture", "video_effects", "gesture_recognition"},
	} {
		sess, err := mgr.Create(session.SessionConfig{ID: def.id, SourceType: "raw_rtp", Task: def.service, VideoAITask: def.task,
			VideoEnabled: true, CallbackURL: server.URL, VideoInferWidth: 64, VideoInferHeight: 64, VideoInferFPS: 5})
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
		sessions[def.task] = sess
	}
	dialer.mu.Lock()
	maskTasks, gestureTasks := dialer.tasks["isolated-mask"], dialer.tasks["isolated-gesture"]
	mask, gesture := dialer.clients["isolated-mask"], dialer.clients["isolated-gesture"]
	dialer.mu.Unlock()
	if len(maskTasks) != 1 || maskTasks[0] != "" || len(gestureTasks) != 1 || gestureTasks[0] != "gesture_recognition" {
		t.Fatalf("each session must open exactly its own AI task: mask=%v gesture=%v", maskTasks, gestureTasks)
	}
	if mask == nil || gesture == nil || mask == gesture {
		t.Fatal("AI task streams are not independent")
	}
	maskSess, gestureSess := sessions["video_segment"], sessions["gesture_recognition"]
	mask.results <- pipeline.RecognitionResult{SessionID: maskSess.ID, Kind: "video_mask", IsFinal: true, VideoMask: &pipeline.VideoMask{FrameID: 1, Status: "ok"}}
	gesture.results <- pipeline.RecognitionResult{SessionID: gestureSess.ID, Kind: "video_gesture", AIService: "gesture_recognition", GestureResult: &pipeline.GestureFrameResult{FrameID: 2}}
	got := map[string]string{}
	for len(got) < 2 {
		select {
		case r := <-received:
			if r.SourceType != "raw_rtp" {
				t.Fatalf("callback identity changed: %+v", r)
			}
			got[r.Kind] = r.SessionID
		case <-time.After(2 * time.Second):
			t.Fatal("task callback missing", got)
		}
	}
	if got["video_mask"] != maskSess.ID || got["video_gesture"] != gestureSess.ID {
		t.Fatal(got)
	}
}
