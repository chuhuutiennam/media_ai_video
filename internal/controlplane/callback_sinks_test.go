package controlplane

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"media-ai-gateway/internal/pipeline"
	"media-ai-gateway/internal/result"
	"media-ai-gateway/internal/session"
)

// sinkStarter trả về HTTPCallbackSink thật (trỏ tới cổng đã đóng, nên mỗi lần gửi đều
// retry) cho cả Start lẫn UpdateCallbackSink.
type sinkStarter struct {
	fakeStarter
	url   string
	sinks []*result.HTTPCallbackSink
}

func (f *sinkStarter) newSink() *result.HTTPCallbackSink {
	sink := result.NewHTTPCallbackSink(f.url, time.Second, 2)
	sink.RetryBackoff = time.Millisecond
	f.sinks = append(f.sinks, sink)
	return sink
}

func (f *sinkStarter) Start(sess *session.Session) (*result.HTTPCallbackSink, error) {
	if _, err := f.fakeStarter.Start(sess); err != nil {
		return nil, err
	}
	return f.newSink(), nil
}

func (f *sinkStarter) UpdateCallbackSink(_, _ string) *result.HTTPCallbackSink { return f.newSink() }

func closedPortURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return "http://" + addr + "/callback"
}

func callbackSessionCount(s *Server) int {
	s.cbMu.Lock()
	defer s.cbMu.Unlock()
	return len(s.httpCallbacks)
}

// Sink đăng ký ở ANSWER và ctrl-result bị gỡ khi session đóng; tổng retry ở /metrics giữ nguyên.
func TestCallbackSinksRetiredOnSessionClose(t *testing.T) {
	f := &sinkStarter{url: closedPortURL(t)}
	s := newTestServer(t, f)
	assertStatus(t, postJSON(t, s, notifyPath(testCallID), validAnswerReq()), http.StatusCreated)
	req := CtrlResultRequest{CallID: testCallID, MediaResources: &MediaResources{
		TCore:   MediaResourceInfo{CallbackURL: f.url},
		TAccess: MediaResourceInfo{CallbackURL: f.url},
	}}
	assertStatus(t, postJSON(t, s, ctrlResultPath(testCallID), req), http.StatusOK)
	if len(f.sinks) != 4 || callbackSessionCount(s) != 2 {
		t.Fatalf("want 4 sinks on 2 sessions, got %d sinks on %d sessions", len(f.sinks), callbackSessionCount(s))
	}

	for _, sink := range f.sinks {
		if err := sink.Send(context.Background(), pipeline.RecognitionResult{SessionID: testCallID}); err == nil {
			t.Fatal("send to closed port should fail")
		}
	}
	const wantRetries = 4 * 2
	if got := s.callbackRetries(); got != wantRetries {
		t.Fatalf("retries before close: want %d, got %d", wantRetries, got)
	}

	assertStatus(t, deletePath(t, s, sessionPath(testCallID)), http.StatusNoContent)
	deadline := time.Now().Add(2 * time.Second)
	for callbackSessionCount(s) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("sinks still registered for %d closed sessions", callbackSessionCount(s))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := s.callbackRetries(); got != wantRetries {
		t.Fatalf("retries after close: want %d, got %d", wantRetries, got)
	}
	body := getPath(t, s, "/metrics").Body.String()
	if !strings.Contains(body, "media_ai_callback_retry_total 8\n") {
		t.Fatal("metric lost retries of closed sessions")
	}
}
