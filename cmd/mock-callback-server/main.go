// Command mock-callback-server là H2C (cleartext HTTP/2) callback receiver dùng cho lab/dev.
//
// Nhận POST /callback từ HTTPCallbackSink của media-ai-gateway,
// parse JSON body (event_type + RecognitionResult), log ra stdout.
//
// Usage:
//
//	mock-callback-server [--port 9999] [--expect-final 0] [--timeout 30s]
//
// Output — mỗi dòng là một JSON object:
//
//	{"event":"ready","port":9999}
//	{"event":"callback","event_type":"asr.transcript.final","session_id":"...","text":"...","is_final":true,...}
//	{"event":"summary","stats":{"asr.transcript.final":1,"asr.transcript.partial":2}}
//	{"event":"timeout","stats":{...}}
//
// Exit codes:
//
//	0  nhận đủ callbacks (expect-final) hoặc daemon bị SIGTERM/Ctrl-C
//	1  timeout (expect-final > 0 và chưa đủ)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

type mediaResource struct {
	ContextID     string `json:"contextId,omitempty"`
	TerminationID string `json:"terminationId,omitempty"`
}

type mediaResources struct {
	TCore   *mediaResource `json:"tCore,omitempty"`
	TAccess *mediaResource `json:"tAccess,omitempty"`
}

// callbackBody mirrors JSON body gửi bởi HTTPCallbackSink.
type callbackBody struct {
	EventType      string          `json:"event_type"`
	SessionID      string          `json:"session_id"`
	StreamID       string          `json:"stream_id"`
	SourceType     string          `json:"source_type"`
	Kind           string          `json:"kind,omitempty"`
	VideoMask      json.RawMessage `json:"video_mask,omitempty"`
	Text           string          `json:"text"`
	IsFinal        bool            `json:"is_final"`
	Confidence     float64         `json:"confidence"`
	Language       string          `json:"language"`
	Seq            int             `json:"seq"`
	TsStart        int64           `json:"ts_start"`
	TsEnd          int64           `json:"ts_end"`
	MediaResources *mediaResources `json:"mediaResources,omitempty"`
}

var enc = json.NewEncoder(os.Stdout)
var encMu sync.Mutex

func emit(v any) {
	encMu.Lock()
	defer encMu.Unlock()
	enc.Encode(v) //nolint:errcheck
}

func main() {
	port := flag.Int("port", 9999, "TCP port lắng nghe")
	expectFinal := flag.Int("expect-final", 0, "thoát sau khi nhận đủ N final (0 = daemon)")
	timeout := flag.Duration("timeout", 30*time.Second, "thời gian chờ khi expect-final > 0")
	flag.Parse()

	var (
		mu    sync.Mutex
		stats = map[string]int{}

		finalCount atomic.Int32
		done       = make(chan struct{})
		doneOnce   sync.Once
	)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body callbackBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			emit(map[string]any{"event": "parse_error", "detail": err.Error()})
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		mu.Lock()
		stats[body.EventType]++
		mu.Unlock()

		emit(map[string]any{
			"event":          "callback",
			"event_type":     body.EventType,
			"session_id":     body.SessionID,
			"stream_id":      body.StreamID,
			"source_type":    body.SourceType,
			"kind":           body.Kind,
			"video_mask":     body.VideoMask,
			"protocol":       r.Proto,
			"path":           r.URL.Path,
			"text":           body.Text,
			"is_final":       body.IsFinal,
			"confidence":     body.Confidence,
			"language":       body.Language,
			"seq":            body.Seq,
			"ts_start":       body.TsStart,
			"ts_end":         body.TsEnd,
			"mediaResources": body.MediaResources,
		})

		w.WriteHeader(http.StatusOK)

		if body.IsFinal && *expectFinal > 0 {
			n := int(finalCount.Add(1))
			if n >= *expectFinal {
				doneOnce.Do(func() { close(done) })
			}
		}
	})

	srv := &http.Server{
		Handler: h2c.NewHandler(handler, &http2.Server{}),
	}

	ln, err := net.Listen("tcp", listenAddr(*port))
	if err != nil {
		emit(map[string]any{"event": "error", "detail": err.Error()})
		os.Exit(1)
	}
	emit(map[string]any{"event": "ready", "port": ln.Addr().(*net.TCPAddr).Port})

	go srv.Serve(ln) //nolint:errcheck

	// Xác định điều kiện thoát.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)

	timedOut := false
	if *expectFinal > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()
		select {
		case <-done:
		case <-ctx.Done():
			timedOut = true
		case <-sig:
		}
	} else {
		<-sig
	}

	srv.Close() //nolint:errcheck

	mu.Lock()
	snapshot := copyStats(stats)
	mu.Unlock()

	if timedOut {
		emit(map[string]any{"event": "timeout", "stats": snapshot})
		os.Exit(1)
	}
	emit(map[string]any{"event": "summary", "stats": snapshot})
}

func listenAddr(port int) string {
	return "0.0.0.0:" + itoa(port)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 10)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}

func copyStats(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
