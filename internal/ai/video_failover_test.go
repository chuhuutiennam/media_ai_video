package ai

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"media-ai-gateway/internal/mockai"
)

func serviceWorker(id string, active int) WorkerInfo {
	w := videoWorker(id, "video_segment")
	w.Services, w.MaxStreams, w.ActiveStreams = []string{"change_background"}, 4, active
	return w
}

// Worker có kết nối hỏng (SetReachable) không được chọn và không tính là còn slot;
// VideoWorkers vẫn liệt kê nó (dùng cho readiness/quan sát).
func TestSelectVideoTaskSkipsUnreachable(t *testing.T) {
	r := NewWorkerRegistry(0)
	r.Register(serviceWorker("live", 1))
	r.Register(serviceWorker("dead", 0)) // tải thấp hơn: không xét kết nối thì luôn được chọn
	w, err := r.SelectVideoTask("change_background", "video_segment", "jpeg", "video_segment")
	if err != nil || w.ID != "dead" {
		t.Fatalf("control: want dead (least loaded), got %q, %v", w.ID, err)
	}
	r.ReleaseVideoTask("dead")

	r.SetReachable(func(addr string) bool { return addr != "dead" })
	for i := 0; i < 3; i++ {
		if w, err := r.SelectVideoTask("change_background", "video_segment", "jpeg", "video_segment"); err != nil || w.ID != "live" {
			t.Fatalf("pick %d: want live, got %q, %v", i, w.ID, err)
		}
	}
	if r.HasVideoCapacity("change_background", "video_segment") {
		t.Fatal("live is full and dead is unreachable: no capacity expected")
	}
	if _, err := r.SelectVideoTask("change_background", "video_segment", "jpeg", "video_segment"); !errors.Is(err, ErrNoWorkerAvailable) {
		t.Fatalf("want ErrNoWorkerAvailable, got %v", err)
	}
	if n := len(r.VideoWorkers("change_background", "video_segment")); n != 2 {
		t.Fatalf("VideoWorkers: want 2, got %d", n)
	}
	r.ReleaseVideoTask("live")
	if _, err := r.SelectVideoTask("change_background", "video_segment", "jpeg", "video_segment", "live"); !errors.Is(err, ErrNoWorkerAvailable) {
		t.Fatalf("excluded worker selected: %v", err)
	}
}

func TestVideoRoutingFailover(t *testing.T) {
	dialErr := errors.New("connection refused")
	setup := func(failover bool, failing ...string) (*VideoRoutingDialer, *WorkerRegistry, *[]string) {
		r := NewWorkerRegistry(0)
		r.Register(serviceWorker("a", 0)) // thứ tự chọn theo tải: a, b, c
		r.Register(serviceWorker("b", 1))
		r.Register(serviceWorker("c", 2))
		var attempts []string
		router := NewVideoRoutingDialer(r, func(ctx context.Context, addr, _, _ string, _ VideoStreamConfig) (VideoStreamClient, error) {
			attempts = append(attempts, addr)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			for _, f := range failing {
				if addr == f {
					return nil, dialErr
				}
			}
			return &nullVideoClient{ctx: ctx}, nil
		})
		router.Failover = failover
		return router, r, &attempts
	}
	active := func(r *WorkerRegistry) map[string]int {
		out := map[string]int{}
		for _, w := range r.List() {
			out[w.ID] = w.ActiveStreams
		}
		return out
	}
	cfg := VideoStreamConfig{SelectedService: "change_background"}

	router, r, attempts := setup(true, "a", "b")
	client, err := router.Dial(context.Background(), "call", "video", cfg)
	if err != nil || client.(*reservedVideoClient).worker != "c" {
		t.Fatalf("failover: want c, got %v", err)
	}
	if got := active(r); got["a"] != 0 || got["b"] != 1 || got["c"] != 3 || len(*attempts) != 3 {
		t.Fatalf("failover: attempts %v, slots %v", *attempts, got)
	}

	router, r, attempts = setup(false, "a")
	if _, err := router.Dial(context.Background(), "call", "video", cfg); !errors.Is(err, dialErr) || len(*attempts) != 1 {
		t.Fatalf("failover off: want 1 attempt and dial error, got %v, %v", *attempts, err)
	}
	if got := active(r); got["a"] != 0 {
		t.Fatalf("failover off leaked slot: %v", got)
	}

	router, r, attempts = setup(true, "a", "b", "c")
	if _, err := router.Dial(context.Background(), "call", "video", cfg); !errors.Is(err, dialErr) || len(*attempts) != 3 {
		t.Fatalf("all fail: want 3 attempts and dial error, got %v, %v", *attempts, err)
	}
	if got := active(r); got["a"] != 0 || got["b"] != 1 || got["c"] != 2 {
		t.Fatalf("all fail leaked slots: %v", got)
	}

	router, _, attempts = setup(true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := router.Dial(ctx, "call", "video", cfg); !errors.Is(err, context.Canceled) || len(*attempts) != 1 {
		t.Fatalf("cancelled ctx must not fail over: %v, %v", *attempts, err)
	}
}

// gRPC thật: change_background có 2 target, một server sống và một địa chỉ không có ai listen.
func TestVideoRoutingSkipsDeadTargetRealServers(t *testing.T) {
	opened := make(chan string, 16)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.StreamInterceptor(func(s any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
		opened <- "live"
		return h(s, ss)
	}))
	mockai.RegisterVideo(srv)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	live, dead := lis.Addr().String(), freeLoopbackAddr(t)

	pool := NewSharedConnPool(0, 0, WithReconnectMaxBackoff(time.Second))
	closePoolConns(t, pool)
	pool.Preconnect(live)
	pool.Preconnect(dead)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := pool.WaitReady(ctx, live); err != nil {
		t.Fatal(err)
	}
	if !waitUntil(5*time.Second, func() bool { return pool.State(dead) == connectivity.TransientFailure }) {
		t.Fatalf("dead target state = %v, want TRANSIENT_FAILURE", pool.State(dead))
	}
	if !pool.Reachable(live) || pool.Reachable(dead) || !pool.Reachable("127.0.0.1:1") {
		t.Fatal("Reachable: want live=true, dead=false, never dialed=true")
	}

	newRegistry := func() *WorkerRegistry {
		r := NewWorkerRegistry(0)
		for label, addr := range map[string]string{"live": live, "dead": dead} {
			w := serviceWorker(label, 0)
			w.Addr = addr
			r.Register(w)
		}
		return r
	}
	cfg := VideoStreamConfig{SelectedService: "change_background", EffectType: "bg_blur", InferWidth: 8, InferHeight: 4, InferFPS: 5}
	dial := func(t *testing.T, router *VideoRoutingDialer) *reservedVideoClient {
		t.Helper()
		client, err := router.Dial(ctx, "call-tcore", "call-tcore", cfg)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-opened:
		case <-time.After(5 * time.Second):
			t.Fatal("stream did not reach the live server")
		}
		rc := client.(*reservedVideoClient)
		t.Cleanup(func() { rc.Close() })
		return rc
	}

	t.Run("skip unreachable", func(t *testing.T) {
		r := newRegistry()
		r.SetReachable(pool.Reachable)
		router := NewVideoRoutingDialer(r, pool.VideoDialFunc())
		for i := 0; i < 4; i++ { // giữ stream mở: live tải cao hơn dead mà vẫn được chọn
			if c := dial(t, router); c.worker != "live" {
				t.Fatalf("pick %d: %s", i, c.worker)
			}
		}
	})
	t.Run("failover after dial error", func(t *testing.T) {
		r := newRegistry()
		router := NewVideoRoutingDialer(r, pool.VideoDialFunc())
		router.Failover = true
		dial(t, router) // live giữ 1 stream → lần sau dead (tải 0) được chọn trước
		if c := dial(t, router); c.worker != "live" {
			t.Fatalf("want failover to live, got %s", c.worker)
		}
		for _, w := range r.List() {
			if w.ID == "dead" && w.ActiveStreams != 0 {
				t.Fatalf("failed dial leaked slot on dead: %+v", w)
			}
		}
	})
}
