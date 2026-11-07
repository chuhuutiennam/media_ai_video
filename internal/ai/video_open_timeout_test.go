package ai

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/connectivity"
)

func openTimeoutRouter(dial VideoGRPCDialFunc, timeout time.Duration) (*VideoRoutingDialer, *WorkerRegistry) {
	r := NewWorkerRegistry(0)
	r.Register(serviceWorker("a", 0))
	r.Register(serviceWorker("b", 1))
	router := NewVideoRoutingDialer(r, dial)
	router.Failover, router.OpenTimeout = true, timeout
	return router, r
}

func TestVideoRoutingOpenTimeout(t *testing.T) {
	cfg := VideoStreamConfig{SelectedService: "change_background"}

	// Mọi worker treo (như kết nối CONNECTING tới máy không phản hồi): một hạn chờ chung cho
	// mọi lần thử, hết hạn thì trả lỗi và trả slot.
	var mu sync.Mutex
	var attempts []string
	hang := func(ctx context.Context, addr, _, _ string, _ VideoStreamConfig) (VideoStreamClient, error) {
		mu.Lock()
		attempts = append(attempts, addr)
		mu.Unlock()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	router, r := openTimeoutRouter(hang, 150*time.Millisecond)
	start := time.Now()
	_, err := router.Dial(context.Background(), "call", "video", cfg)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "timed out after 150ms") {
		t.Fatalf("want open timeout error, got %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("Dial took %v, want about 150ms", elapsed)
	}
	if len(attempts) != 1 {
		t.Fatalf("failover must stop when the open budget is spent, attempts %v", attempts)
	}
	for _, w := range r.List() {
		if (w.ID == "a" && w.ActiveStreams != 0) || (w.ID == "b" && w.ActiveStreams != 1) {
			t.Fatalf("timed out dial leaked slot: %+v", w)
		}
	}

	// Mở thành công: stream không bị timer huỷ về sau.
	ok := func(ctx context.Context, _, _, _ string, _ VideoStreamConfig) (VideoStreamClient, error) {
		return &nullVideoClient{ctx: ctx}, nil
	}
	router, _ = openTimeoutRouter(ok, 50*time.Millisecond)
	client, err := router.Dial(context.Background(), "call", "video", cfg)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if ctxErr := client.(*reservedVideoClient).VideoStreamClient.(*nullVideoClient).ctx.Err(); ctxErr != nil {
		t.Fatalf("open timeout cancelled an established stream: %v", ctxErr)
	}
	client.(*reservedVideoClient).Close()

	// Session đóng trong lúc mở (ctx cha huỷ): trả lỗi của ctx, không báo "timed out".
	router, _ = openTimeoutRouter(hang, time.Minute)
	parent, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	if _, err := router.Dial(parent, "call", "video", cfg); !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "timed out") {
		t.Fatalf("parent cancel: got %v", err)
	}
}

// gRPC thật: máy nhận TCP nhưng không trả lời HTTP/2 → kết nối nằm CONNECTING và NewStream chờ
// tới hết lần thử kết nối (~20s). OpenTimeout phải cắt ngắn.
func TestVideoRoutingOpenTimeoutRealConnecting(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var held []net.Conn
	var mu sync.Mutex
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c) // giữ kết nối, không gửi gì
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		lis.Close()
		mu.Lock()
		for _, c := range held {
			c.Close()
		}
		mu.Unlock()
	})

	pool := NewSharedConnPool(0, 0)
	closePoolConns(t, pool)
	r := NewWorkerRegistry(0)
	w := serviceWorker("silent", 0)
	w.Addr = lis.Addr().String()
	r.Register(w)
	r.SetReachable(pool.Reachable)
	router := NewVideoRoutingDialer(r, pool.VideoDialFunc())
	router.Failover, router.OpenTimeout = true, 300*time.Millisecond

	start := time.Now()
	_, err = router.Dial(context.Background(), "call-tcore", "call-tcore", VideoStreamConfig{
		SelectedService: "change_background", EffectType: "bg_blur", InferWidth: 8, InferHeight: 4, InferFPS: 5})
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "timed out after 300ms") || elapsed > 3*time.Second {
		t.Fatalf("want timeout after ~300ms, got %v after %v", err, elapsed)
	}
	if st := pool.State(w.Addr); st != connectivity.Connecting {
		t.Logf("note: conn state %v (expected CONNECTING while the peer stays silent)", st)
	}
	if r.List()[0].ActiveStreams != 0 {
		t.Fatal("timed out dial leaked slot")
	}
	t.Logf("Dial returned after %v: %v", elapsed.Round(time.Millisecond), err)
}
