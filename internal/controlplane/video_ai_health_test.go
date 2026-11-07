package controlplane

import (
	"context"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"

	"media-ai-gateway/internal/ai"
)

func videoServiceWorker(service, task, addr string) ai.WorkerInfo {
	return ai.WorkerInfo{ID: "video-service:" + service + ":" + addr, Addr: addr,
		Tasks: []string{task}, Services: []string{service}, MaxStreams: 4,
		MediaTypes: []string{"video"}, InputFormats: []string{"jpeg"}, Protocols: []string{"video_segment"}}
}

// Mọi target của dịch vụ mất kết nối → ANSWER 503 trước khi tạo session (không còn 502 sau khi tạo).
func TestVideoServiceCatalog_UnreachableAIReturns503(t *testing.T) {
	starter := &fakeStarter{}
	start, end := availableUDPRange(t, 4)
	s := newTestServerWithVideo(t, starter, start, end)
	reg := ai.NewWorkerRegistry(0)
	reg.Register(videoServiceWorker("change_background", VideoTaskSegment, "seg-a:1"))
	reg.Register(videoServiceWorker("change_background", VideoTaskSegment, "seg-b:1"))
	reachable := false
	reg.SetReachable(func(string) bool { return reachable })
	s.SetVideoWorkerRegistry(reg)

	w := postJSON(t, s, notifyPath(testCallID), SessionEvent{Event: "ANSWER", SelectedService: "change_background"})
	assertStatus(t, w, http.StatusServiceUnavailable)
	var e ErrorResponse
	decodeJSON(t, w, &e)
	if !strings.Contains(e.Error, "change_background") || e.RetryAfterMs == 0 || s.sessionMgr.Count() != 0 || len(starter.sessions) != 0 {
		t.Fatalf("want 503 before creating sessions, got %+v (sessions %d)", e, s.sessionMgr.Count())
	}

	reachable = true
	assertStatus(t, postJSON(t, s, notifyPath(testCallID), SessionEvent{Event: "ANSWER", SelectedService: "change_background"}), http.StatusCreated)
}

// gRPC thật: video_effects có server sống, change_background chỉ có địa chỉ không ai listen.
func TestHealthReadyAndConnections_VideoAITargets(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	live := lis.Addr().String()
	deadLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := deadLis.Addr().String()
	deadLis.Close()

	pool := ai.NewSharedConnPool(0, 0, ai.WithReconnectMaxBackoff(time.Second))
	pool.Preconnect(live)
	pool.Preconnect(dead)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.WaitReady(ctx, live); err != nil {
		t.Fatal(err)
	}
	for pool.State(dead) != connectivity.TransientFailure {
		if ctx.Err() != nil {
			t.Fatalf("dead target state = %v, want TRANSIENT_FAILURE", pool.State(dead))
		}
		time.Sleep(20 * time.Millisecond)
	}

	s := newTestServer(t, &fakeStarter{})
	s.cfg.VideoEnabled = true
	s.cfg.VideoServices = DefaultVideoServices(true)
	reg := ai.NewWorkerRegistry(0)
	reg.Register(videoServiceWorker("change_background", VideoTaskSegment, dead))
	reg.Register(videoServiceWorker("video_effects", VideoTaskGesture, live))
	s.SetVideoWorkerRegistry(reg)
	s.SetGRPCPool(pool)

	assertStatus(t, getPath(t, s, "/health/ready"), http.StatusOK) // mặc định không xét AI video

	s.cfg.VideoReadinessRequiresAI = true
	w := getPath(t, s, "/health/ready")
	assertStatus(t, w, http.StatusServiceUnavailable)
	var body struct {
		Status       string   `json:"status"`
		Reason       string   `json:"reason"`
		Services     []string `json:"services"`
		RetryAfterMs int      `json:"retry_after_ms"`
	}
	decodeJSON(t, w, &body)
	if body.Status != "not_ready" || body.Reason != "video_service_unavailable" || !slices.Equal(body.Services, []string{"change_background"}) || body.RetryAfterMs == 0 {
		t.Fatalf("readiness body: %+v", body)
	}

	w = getPath(t, s, "/v1/connections")
	assertStatus(t, w, http.StatusOK)
	var conns ConnectionsResponse
	decodeJSON(t, w, &conns)
	got := map[string]AIWorkerConn{}
	for _, c := range conns.AIWorkers {
		got[c.Addr] = c
	}
	if c := got[dead]; c.State != "TRANSIENT_FAILURE" || !slices.Equal(c.Services, []string{"change_background"}) || !slices.Equal(c.Tasks, []string{VideoTaskSegment}) {
		t.Errorf("dead target: %+v", c)
	}
	if c := got[live]; c.State != "READY" || !slices.Equal(c.Services, []string{"video_effects"}) || !slices.Equal(c.Tasks, []string{VideoTaskGesture}) || c.ActiveStream != 0 {
		t.Errorf("live target: %+v", c)
	}
}
