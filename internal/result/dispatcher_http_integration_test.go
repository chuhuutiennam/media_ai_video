package result

// Integration tests: Dispatcher.Push() → HTTPCallbackSink.Send() → real H2C server.
//
// Gap được lấp: các test đơn vị trước đây chỉ test Dispatcher với captureSink,
// và HTTPCallbackSink.Send() được gọi trực tiếp — không có test nào nối cả hai.

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"media-ai-gateway/internal/pipeline"
)

// httpCallbackBody mirrors the JSON body sent by HTTPCallbackSink over the wire.
type httpCallbackBody struct {
	EventType string `json:"event_type"`
	pipeline.RecognitionResult
}

// recvCallback blocks until the channel delivers a body or the test times out.
func recvCallback(t *testing.T, ch <-chan httpCallbackBody) httpCallbackBody {
	t.Helper()
	select {
	case b := <-ch:
		return b
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: H2C server did not receive callback request")
		return httpCallbackBody{}
	}
}

// captureHandler returns an HTTP handler that JSON-decodes the request body
// and sends it to ch (non-blocking; extra deliveries are dropped).
func captureHandler(ch chan httpCallbackBody) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var b httpCallbackBody
		json.NewDecoder(r.Body).Decode(&b)
		w.WriteHeader(http.StatusOK)
		select {
		case ch <- b:
		default:
		}
	}
}

// TestDispatcher_HTTPCallbackSink_DeliversFinal verifies that a final result
// pushed through Dispatcher reaches the H2C callback server with correct JSON.
func TestDispatcher_HTTPCallbackSink_DeliversFinal(t *testing.T) {
	received := make(chan httpCallbackBody, 1)
	srv := newH2CServer(captureHandler(received))
	defer srv.Close()

	d := NewDispatcher(smallCfg())
	d.RegisterSink("s1", NewHTTPCallbackSink(srv.URL, time.Second, 0))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	if err := d.Push(context.Background(), pipeline.RecognitionResult{
		SessionID: "s1",
		Text:      "xin chào",
		IsFinal:   true,
		Language:  "vi",
	}); err != nil {
		t.Fatalf("Push: %v", err)
	}

	got := recvCallback(t, received)

	if got.EventType != "asr.transcript.final" {
		t.Errorf("event_type = %q, want %q", got.EventType, "asr.transcript.final")
	}
	if got.Text != "xin chào" {
		t.Errorf("text = %q, want 'xin chào'", got.Text)
	}
	if got.SessionID != "s1" {
		t.Errorf("session_id = %q, want 's1'", got.SessionID)
	}
	if !got.IsFinal {
		t.Error("is_final = false, want true")
	}
	if got.Language != "vi" {
		t.Errorf("language = %q, want 'vi'", got.Language)
	}
}

// TestDispatcher_HTTPCallbackSink_DeliversPartial verifies partial results
// reach the callback server with event_type="asr.transcript.partial".
func TestDispatcher_HTTPCallbackSink_DeliversPartial(t *testing.T) {
	received := make(chan httpCallbackBody, 1)
	srv := newH2CServer(captureHandler(received))
	defer srv.Close()

	// DropPartialWhenFull=false ensures the partial is not silently dropped.
	cfg := Config{Workers: 2, QueueSize: 8, DropPartialWhenFull: false, SendTimeout: time.Second}
	d := NewDispatcher(cfg)
	d.RegisterSink("s1", NewHTTPCallbackSink(srv.URL, time.Second, 0))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	if err := d.Push(context.Background(), pipeline.RecognitionResult{
		SessionID: "s1",
		Text:      "xin",
		IsFinal:   false,
	}); err != nil {
		t.Fatalf("Push: %v", err)
	}

	got := recvCallback(t, received)

	if got.EventType != "asr.transcript.partial" {
		t.Errorf("event_type = %q, want %q", got.EventType, "asr.transcript.partial")
	}
	if got.IsFinal {
		t.Error("is_final = true for partial result, want false")
	}
}

// TestDispatcher_HTTPCallbackSink_UsesHTTP2 verifies that the Dispatcher worker
// delivers via HTTP/2 (not HTTP/1.1) when wired to HTTPCallbackSink.
func TestDispatcher_HTTPCallbackSink_UsesHTTP2(t *testing.T) {
	protoReceived := make(chan string, 1)
	srv := newH2CServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		select {
		case protoReceived <- r.Proto:
		default:
		}
	}))
	defer srv.Close()

	d := NewDispatcher(smallCfg())
	d.RegisterSink("s1", NewHTTPCallbackSink(srv.URL, time.Second, 0))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	d.Push(context.Background(), pipeline.RecognitionResult{SessionID: "s1", IsFinal: true})

	select {
	case proto := <-protoReceived:
		if proto != "HTTP/2.0" {
			t.Errorf("protocol = %q, want HTTP/2.0", proto)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: server did not receive request")
	}
}

// TestDispatcher_HTTPCallbackSink_SessionIsolation verifies that results are
// routed to the correct sink and do not cross session boundaries.
func TestDispatcher_HTTPCallbackSink_SessionIsolation(t *testing.T) {
	recv1 := make(chan httpCallbackBody, 4)
	recv2 := make(chan httpCallbackBody, 4)
	srv1 := newH2CServer(captureHandler(recv1))
	srv2 := newH2CServer(captureHandler(recv2))
	defer srv1.Close()
	defer srv2.Close()

	d := NewDispatcher(smallCfg())
	d.RegisterSink("s1", NewHTTPCallbackSink(srv1.URL, time.Second, 0))
	d.RegisterSink("s2", NewHTTPCallbackSink(srv2.URL, time.Second, 0))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	d.Push(context.Background(), pipeline.RecognitionResult{SessionID: "s1", Text: "result-one", IsFinal: true})
	d.Push(context.Background(), pipeline.RecognitionResult{SessionID: "s2", Text: "result-two", IsFinal: true})

	got1 := recvCallback(t, recv1)
	got2 := recvCallback(t, recv2)

	if got1.SessionID != "s1" || got1.Text != "result-one" {
		t.Errorf("srv1 got session_id=%q text=%q, want s1/result-one", got1.SessionID, got1.Text)
	}
	if got2.SessionID != "s2" || got2.Text != "result-two" {
		t.Errorf("srv2 got session_id=%q text=%q, want s2/result-two", got2.SessionID, got2.Text)
	}
	if len(recv1) != 0 || len(recv2) != 0 {
		t.Error("unexpected extra results in callback channels — cross-session contamination")
	}
}

// TestDispatcher_HTTPCallbackSink_RetryOn5xx_ViaDispatcher verifies end-to-end retry:
// dispatcher pushes a result, HTTPCallbackSink retries on 503, eventually delivers.
func TestDispatcher_HTTPCallbackSink_RetryOn5xx_ViaDispatcher(t *testing.T) {
	var attempts atomic.Int32
	received := make(chan httpCallbackBody, 1)

	srv := newH2CServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var b httpCallbackBody
		json.NewDecoder(r.Body).Decode(&b)
		w.WriteHeader(http.StatusOK)
		select {
		case received <- b:
		default:
		}
	}))
	defer srv.Close()

	d := NewDispatcher(smallCfg())
	sink := NewHTTPCallbackSink(srv.URL, time.Second, 3)
	sink.RetryBackoff = time.Millisecond
	d.RegisterSink("s1", sink)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	d.Push(context.Background(), pipeline.RecognitionResult{
		SessionID: "s1",
		Text:      "retry-ok",
		IsFinal:   true,
	})

	got := recvCallback(t, received)
	if got.Text != "retry-ok" {
		t.Errorf("text = %q, want 'retry-ok'", got.Text)
	}
	if n := attempts.Load(); n != 3 {
		t.Errorf("attempts = %d, want 3 (2 failures + 1 success)", n)
	}
}

// TestDispatcher_HTTPCallbackSink_SinkError_CountedInStats verifies that an
// HTTP 503 response (no retry) increments SendErrors in Dispatcher.Stats().
func TestDispatcher_HTTPCallbackSink_SinkError_CountedInStats(t *testing.T) {
	srv := newH2CServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	d := NewDispatcher(smallCfg())
	d.RegisterSink("s1", NewHTTPCallbackSink(srv.URL, time.Second, 0))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	d.Push(context.Background(), pipeline.RecognitionResult{SessionID: "s1", IsFinal: true})

	waitStats(t, d, func(st Stats) bool { return st.Sent+st.SendErrors >= 1 })

	st := d.Stats()
	if st.SendErrors != 1 {
		t.Errorf("SendErrors = %d, want 1", st.SendErrors)
	}
	if st.Sent != 0 {
		t.Errorf("Sent = %d, want 0 (server always 503)", st.Sent)
	}
}
