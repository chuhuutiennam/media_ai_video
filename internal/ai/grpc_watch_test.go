package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
)

// Các test trong file này dùng gRPC server và TCP listener thật trên loopback (không mock)
// để kiểm tra WaitReady, WatchAndReconnect và WithReconnectMaxBackoff.

const (
	msgConnLost     = "ai: grpc connection lost"
	msgConnRestored = "ai: grpc connection restored"
)

// freeLoopbackAddr trả về một địa chỉ loopback hiện không có ai listen.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	lis.Close()
	return addr
}

// startGRPCServer chạy gRPC server thật (không cần service) tại addr.
// Thử lại tối đa 3s vì port vừa đóng có thể chưa bind lại được ngay.
func startGRPCServer(t *testing.T, addr string) *grpc.Server {
	t.Helper()
	var lis net.Listener
	var err error
	for deadline := time.Now().Add(3 * time.Second); ; {
		if lis, err = net.Listen("tcp", addr); err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("listen %s: %v", addr, err)
	}
	srv := grpc.NewServer()
	go srv.Serve(lis)
	return srv
}

func closePoolConns(t *testing.T, p *SharedConnPool) {
	t.Cleanup(func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, c := range p.conns {
			c.Close()
		}
	})
}

// logSink giữ các dòng log JSON thật (encoder production) ghi qua global zap logger.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *logSink) Sync() error { return nil }

// find trả về các dòng log có msg và addr cho trước.
func (s *logSink) find(msg, addr string) []map[string]any {
	s.mu.Lock()
	data := append([]byte(nil), s.buf.Bytes()...)
	s.mu.Unlock()
	var out []map[string]any
	for _, line := range bytes.Split(data, []byte("\n")) {
		var e map[string]any
		if json.Unmarshal(line, &e) == nil && e["msg"] == msg && e["addr"] == addr {
			out = append(out, e)
		}
	}
	return out
}

// observeLogs thay global zap logger bằng logger JSON ghi vào logSink trong suốt test.
func observeLogs(t *testing.T) *logSink {
	t.Helper()
	sink := &logSink{}
	core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), sink, zap.DebugLevel)
	restore := zap.ReplaceGlobals(zap.New(core))
	t.Cleanup(restore)
	return sink
}

func countLogs(logs *logSink, msg, addr string) int { return len(logs.find(msg, addr)) }

func waitUntil(timeout time.Duration, cond func() bool) bool {
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}

func TestWaitReady_RealServer(t *testing.T) {
	addr := freeLoopbackAddr(t)
	srv := startGRPCServer(t, addr)
	defer srv.Stop()

	pool := NewSharedConnPool(0, 0)
	closePoolConns(t, pool)
	pool.Preconnect(addr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.WaitReady(ctx, addr); err != nil {
		t.Fatalf("WaitReady with live server: %v (state=%v)", err, pool.State(addr))
	}

	if err := pool.WaitReady(ctx, "127.0.0.1:1"); err == nil {
		t.Fatal("WaitReady on address never dialed: want error")
	}

	down := freeLoopbackAddr(t) // không có server
	pool.Preconnect(down)
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer shortCancel()
	if err := pool.WaitReady(shortCtx, down); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitReady without server: got %v, want DeadlineExceeded", err)
	}
}

// Worker restart: kết nối được watch phải tự về READY mà không cần RPC nào và log
// lost/restored đúng một lần. Pool đối chứng không watch phải nằm IDLE.
func TestWatchAndReconnect_ServerRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("restarts a real gRPC server")
	}
	logs := observeLogs(t)
	addr := freeLoopbackAddr(t)
	srv := startGRPCServer(t, addr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watched := NewSharedConnPool(0, 0, WithReconnectMaxBackoff(time.Second))
	plain := NewSharedConnPool(0, 0, WithReconnectMaxBackoff(time.Second))
	closePoolConns(t, watched)
	closePoolConns(t, plain)
	for _, p := range []*SharedConnPool{watched, plain} {
		p.Preconnect(addr)
		readyCtx, readyCancel := context.WithTimeout(ctx, 5*time.Second)
		err := p.WaitReady(readyCtx, addr)
		readyCancel()
		if err != nil {
			t.Fatalf("initial WaitReady: %v", err)
		}
	}
	go watched.WatchAndReconnect(ctx, addr)

	srv.Stop() // worker chết: kết nối READY bị đóng
	if !waitUntil(5*time.Second, func() bool { return countLogs(logs, msgConnLost, addr) == 1 }) {
		t.Fatalf("no %q log after server stop (state=%v)", msgConnLost, watched.State(addr))
	}

	srv2 := startGRPCServer(t, addr)
	defer srv2.Stop()
	restartedAt := time.Now()
	readyCtx, readyCancel := context.WithTimeout(ctx, 5*time.Second)
	defer readyCancel()
	if err := watched.WaitReady(readyCtx, addr); err != nil {
		t.Fatalf("watched conn did not reconnect without an RPC: %v (state=%v)", err, watched.State(addr))
	}
	t.Logf("watched conn READY %v after server restart", time.Since(restartedAt).Round(time.Millisecond))

	if !waitUntil(2*time.Second, func() bool { return countLogs(logs, msgConnRestored, addr) == 1 }) {
		t.Fatalf("no %q log after reconnect", msgConnRestored)
	}
	if n := countLogs(logs, msgConnLost, addr); n != 1 {
		t.Errorf("%q logged %d times, want 1", msgConnLost, n)
	}
	entry := logs.find(msgConnRestored, addr)[0]
	if down, _ := entry["down_ms"].(float64); down <= 0 {
		t.Errorf("down_ms = %v, want > 0", entry["down_ms"])
	}
	t.Logf("log: %v", entry)

	// Đối chứng: không watch thì gRPC để kết nối IDLE cho tới RPC kế tiếp.
	if st := plain.State(addr); st != connectivity.Idle {
		t.Errorf("unwatched conn state = %v, want IDLE", st)
	}
}

// acceptCloseListener nhận TCP rồi đóng ngay (không gửi HTTP/2 preface), nên mỗi lần
// gRPC thử nối đều thất bại và bị tính backoff. Ghi lại thời điểm từng lần thử.
type acceptCloseListener struct {
	addr     string
	mu       sync.Mutex
	attempts []time.Time
}

func newAcceptCloseListener(t *testing.T) *acceptCloseListener {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &acceptCloseListener{addr: lis.Addr().String()}
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			l.mu.Lock()
			l.attempts = append(l.attempts, time.Now())
			l.mu.Unlock()
			c.Close()
		}
	}()
	t.Cleanup(func() { lis.Close() })
	return l
}

func (l *acceptCloseListener) snapshot() []time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]time.Time(nil), l.attempts...)
}

func maxGap(ts []time.Time) time.Duration {
	var g time.Duration
	for i := 1; i < len(ts); i++ {
		g = max(g, ts[i].Sub(ts[i-1]))
	}
	return g
}

// Backoff mặc định gRPC: các lần thử cách nhau 1s, 1.6s, 2.56s, 4.1s… (±20%) → trong 6.5s
// chỉ có 4 lần. Với max backoff 1s: cách nhau ≤ 1.2s → ít nhất 6 lần.
func TestReconnectMaxBackoff_CapsRetryInterval(t *testing.T) {
	if testing.Short() {
		t.Skip("measures real gRPC reconnect backoff (~7s)")
	}
	const window = 6500 * time.Millisecond
	cappedSrv := newAcceptCloseListener(t)
	defaultSrv := newAcceptCloseListener(t)

	capped := NewSharedConnPool(0, 0, WithReconnectMaxBackoff(time.Second))
	dflt := NewSharedConnPool(0, 0)
	closePoolConns(t, capped)
	closePoolConns(t, dflt)
	capped.Preconnect(cappedSrv.addr)
	dflt.Preconnect(defaultSrv.addr)
	time.Sleep(window)

	ca, da := cappedSrv.snapshot(), defaultSrv.snapshot()
	t.Logf("capped: %d attempts, max gap %v; default: %d attempts, max gap %v",
		len(ca), maxGap(ca).Round(time.Millisecond), len(da), maxGap(da).Round(time.Millisecond))
	if len(ca) < 6 {
		t.Errorf("capped pool made %d attempts in %v, want >= 6", len(ca), window)
	}
	if g := maxGap(ca); g > 1500*time.Millisecond {
		t.Errorf("capped pool max gap %v, want <= 1.5s", g)
	}
	if len(da) > 5 {
		t.Errorf("control: default pool made %d attempts in %v, want <= 5", len(da), window)
	}
}
