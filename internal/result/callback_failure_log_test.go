package result

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"media-ai-gateway/internal/pipeline"
)

type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *logBuffer) Sync() error { return nil }

// count trả số dòng log JSON có level và msg cho trước.
func (b *logBuffer) count(level, msg string) int {
	b.mu.Lock()
	data := append([]byte(nil), b.buf.Bytes()...)
	b.mu.Unlock()
	n := 0
	for _, line := range bytes.Split(data, []byte("\n")) {
		var e map[string]any
		if json.Unmarshal(line, &e) == nil && e["level"] == level && e["msg"] == msg {
			n++
		}
	}
	return n
}

// Log level info (production) phải thấy được khi callback bắt đầu lỗi và khi hồi phục, nhưng
// không log mỗi kết quả lỗi (tránh tràn log khi MF chết).
func TestDispatcherLogsCallbackFailureTransitions(t *testing.T) {
	logs := &logBuffer{}
	restore := zap.ReplaceGlobals(zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), logs, zap.InfoLevel)))
	defer restore()

	var fail atomic.Bool
	fail.Store(true)
	srv := newH2CServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := NewDispatcher(Config{Workers: 1, QueueSize: 8, SendTimeout: 2 * time.Second})
	sink := NewHTTPCallbackSink(srv.URL+"/results/tAccess", time.Second, 0)
	d.RegisterSink("s1", sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	push := func(n int, until func(Stats) bool) {
		t.Helper()
		for i := 0; i < n; i++ {
			if err := d.Push(context.Background(), pipeline.RecognitionResult{SessionID: "s1", Text: "x", IsFinal: true}); err != nil {
				t.Fatal(err)
			}
		}
		waitStats(t, d, until)
	}
	const failing, recovered = "result: callback delivery failing", "result: callback delivery recovered"

	push(3, func(s Stats) bool { return s.SendErrors == 3 })
	if logs.count("warn", failing) != 1 || logs.count("info", recovered) != 0 {
		t.Fatalf("3 failures: want 1 warn, got warn=%d info=%d", logs.count("warn", failing), logs.count("info", recovered))
	}
	fail.Store(false)
	push(2, func(s Stats) bool { return s.Sent == 2 })
	if logs.count("info", recovered) != 1 {
		t.Fatalf("recovery: want 1 info, got %d", logs.count("info", recovered))
	}
	fail.Store(true)
	push(2, func(s Stats) bool { return s.SendErrors == 5 })
	if logs.count("warn", failing) != 2 {
		t.Fatalf("second outage: want 2 warns total, got %d", logs.count("warn", failing))
	}
}
