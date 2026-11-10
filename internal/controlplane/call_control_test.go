package controlplane

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// fakeDCSF: DCSF giả (h2c). Đường dẫn chứa /hang → không trả lời tới khi client huỷ; /err → 500
// kèm cause; còn lại → 200. Ghi lại body CALL_CTRL và thời điểm request /hang bị huỷ.
type fakeDCSF struct {
	*httptest.Server
	mu        sync.Mutex
	bodies    []map[string]string
	cancelled chan time.Time
}

func newFakeDCSF(t *testing.T) *fakeDCSF {
	t.Helper()
	f := &fakeDCSF{cancelled: make(chan time.Time, 8)}
	f.Server = httptest.NewServer(h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body) //nolint:errcheck
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()
		switch {
		case strings.Contains(r.URL.Path, "/hang"):
			<-r.Context().Done()
			f.cancelled <- time.Now()
		case strings.Contains(r.URL.Path, "/err"):
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"cause":"MF resource unavailable"}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}), &http2.Server{}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeDCSF) lastBody() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		return nil
	}
	return f.bodies[len(f.bodies)-1]
}

func answerWithCallback(t *testing.T, s *Server, path, callID, callbackURL string) int {
	t.Helper()
	req := validAnswerReq()
	req.CallID, req.CallbackURL = callID, callbackURL
	return postJSON(t, s, path, req).Code
}

func callControlServer(t *testing.T, timeout time.Duration) *Server {
	t.Helper()
	s := newTestServer(t, &fakeStarter{})
	s.cfg.DCSFCallControlTimeout = timeout
	s.cfg.PublicURL = "http://dcas.example"
	return s
}

// CALL_CTRL cũ còn treo hết hạn sau khi callId đã được dùng lại: không được đóng cuộc gọi mới.
// DELETE huỷ ngay CALL_CTRL đang chờ (không đợi hết hạn).
func TestCallControl_StaleFailureDoesNotCloseReusedCall(t *testing.T) {
	dcsf := newFakeDCSF(t)
	s := callControlServer(t, 400*time.Millisecond)

	if code := answerWithCallback(t, s, notifyPath(testCallID), testCallID, dcsf.URL+"/call-control/hang"); code != http.StatusCreated {
		t.Fatalf("ANSWER 1: %d", code)
	}
	time.Sleep(50 * time.Millisecond)
	deleted := time.Now()
	assertStatus(t, deletePath(t, s, sessionPath(testCallID)), http.StatusNoContent)
	select {
	case at := <-dcsf.cancelled:
		if d := at.Sub(deleted); d > 200*time.Millisecond {
			t.Errorf("CALL_CTRL cancelled %v after DELETE, want promptly", d)
		}
	case <-time.After(time.Second):
		t.Fatal("pending CALL_CTRL not cancelled by DELETE")
	}

	if code := answerWithCallback(t, s, notifyPath(testCallID), testCallID, dcsf.URL+"/call-control/ok"); code != http.StatusCreated {
		t.Fatalf("ANSWER 2: %d", code)
	}
	time.Sleep(600 * time.Millisecond) // quá hạn CALL_CTRL đầu tiên
	assertStatus(t, getPath(t, s, sessionPath(testCallID)), http.StatusOK)
	s.sessionMgr.Close(testCallID + "-tcore")
	s.sessionMgr.Close(testCallID + "-taccess")
}

// DCSF trả lỗi: đóng đúng các session của cuộc gọi đó.
func TestCallControl_ErrorClosesOwnCall(t *testing.T) {
	dcsf := newFakeDCSF(t)
	s := callControlServer(t, time.Second)
	if code := answerWithCallback(t, s, notifyPath(testCallID), testCallID, dcsf.URL+"/call-control/err"); code != http.StatusCreated {
		t.Fatalf("ANSWER: %d", code)
	}
	deadline := time.Now().Add(2 * time.Second)
	for s.sessionMgr.Count() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("sessions still open after CALL_CTRL error: %d", s.sessionMgr.Count())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAnswer_RejectsInvalidCallbackURL(t *testing.T) {
	s := callControlServer(t, time.Second)
	for _, raw := range []string{"ftp://dcsf/call-control/x", "/call-control/x", "http://u:p@dcsf/x", "http://"} {
		if code := answerWithCallback(t, s, notifyPath(testCallID), testCallID, raw); code != http.StatusBadRequest {
			t.Errorf("callbackUrl %q: got %d, want 400", raw, code)
		}
	}
	if s.sessionMgr.Count() != 0 {
		t.Fatal("rejected ANSWER created sessions")
	}
}

// callId kiểu IMS: URL ctrl-result gửi DCSF phải escape '[' ']' và vẫn trỏ đúng route.
func TestCallControl_EscapesCallIDInCtrlResultURL(t *testing.T) {
	dcsf := newFakeDCSF(t)
	s := callControlServer(t, time.Second)
	const callID = "p2uc31@[FC00:0DB8::]"
	if code := answerWithCallback(t, s, notifyPath(url.PathEscape(callID)), callID, dcsf.URL+"/call-control/ok"); code != http.StatusCreated {
		t.Fatalf("ANSWER: %d", code)
	}
	deadline := time.Now().Add(2 * time.Second)
	for dcsf.lastBody() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	got := dcsf.lastBody()["callbackUrl"]
	want := "http://dcas.example/v1/vonras/call-sessions/p2uc31@%5BFC00:0DB8::%5D/ctrl-result"
	if got != want {
		t.Fatalf("ctrl-result URL: got %q, want %q", got, want)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, u.RequestURI(), strings.NewReader(`{"callId":"`+callID+`"}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	assertStatus(t, w, http.StatusOK)
	s.sessionMgr.Close(callID + "-tcore")
	s.sessionMgr.Close(callID + "-taccess")
}

// https:// dùng TLS thật (trước đây: TCP thường + h2c → "unexpected EOF"); giới hạn client động.
func TestDCSFPool_TLSAndDynamicClientCap(t *testing.T) {
	var gotTLS atomic.Bool
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTLS.Store(r.TLS != nil && r.ProtoMajor == 2)
		w.WriteHeader(http.StatusOK)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	pool := NewDCSFPool(nil)
	target := srv.URL + "/call-control/x"
	err := pool.SendCallControl(t.Context(), target, "c1", "http://dcas/ctrl-result")
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("untrusted test cert: want a TLS certificate error (proves TLS is used), got %v", err)
	}
	// Tin cert của server test → CALL_CTRL thành công qua TLS + HTTP/2.
	pool.clientFor(target).Transport.(*http2.Transport).TLSClientConfig = &tls.Config{
		RootCAs: srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}
	if err := pool.SendCallControl(t.Context(), target, "c1", "http://dcas/ctrl-result"); err != nil || !gotTLS.Load() {
		t.Fatalf("TLS CALL_CTRL: err=%v, server saw TLS+h2=%v", err, gotTLS.Load())
	}
	if pool.clientFor("http://"+strings.TrimPrefix(srv.URL, "https://")) == pool.clientFor(target) {
		t.Fatal("http and https to the same host must not share a client")
	}

	for i := 0; i < maxDynamicDCSFClients+10; i++ {
		pool.clientFor("http://dcsf-" + string(rune('a'+i%26)) + strings.Repeat("x", i/26) + ":9090/x")
	}
	if pool.dynamic != maxDynamicDCSFClients {
		t.Fatalf("dynamic clients: %d, want cap %d", pool.dynamic, maxDynamicDCSFClients)
	}
	if pool.clientFor("http://beyond-cap:9090/x") != dcsfFallbackClient {
		t.Fatal("hosts beyond the cap must use the shared fallback client")
	}
}
