package coordinator

// Integration tests: Coordinator.Start() với sess.CallbackURL != "".
//
// Gap được lấp: tất cả coordinator test trước đây dùng sess.CallbackURL="" nên code path
//   if sess.CallbackURL != "" { cbSink = NewHTTPCallbackSink(...) }
// chưa bao giờ được thực thi trong test suite.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"media-ai-gateway/internal/pipeline"
	"media-ai-gateway/internal/session"
)

// newH2CTestServer khởi động H2C test server — dùng riêng cho package coordinator
// vì helper newH2CServer trong result package không export.
func newH2CTestServer(handler http.Handler) *httptest.Server {
	return httptest.NewServer(h2c.NewHandler(handler, &http2.Server{}))
}

// coordCallbackBody mirrors JSON body của HTTPCallbackSink.
type coordCallbackBody struct {
	EventType string `json:"event_type"`
	pipeline.RecognitionResult
}

// createSessionWithCallback tạo session với CallbackURL, dùng chung SessionConfig
// như createSession nhưng thêm CallbackURL.
func createSessionWithCallback(t *testing.T, mgr *session.Manager, id, callbackURL string) *session.Session {
	t.Helper()
	sess, err := mgr.Create(session.SessionConfig{
		ID: id, SourceType: "raw_rtp",
		Codec: "PCMU", SampleRate: 8000, Channels: 1,
		Language: "vi", Task: "transcribe",
		CallbackURL: callbackURL,
	})
	if err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
	return sess
}

// recvCoordCallback blocks cho đến khi H2C server nhận request hoặc timeout.
func recvCoordCallback(t *testing.T, ch <-chan coordCallbackBody) coordCallbackBody {
	t.Helper()
	select {
	case b := <-ch:
		return b
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: H2C callback server did not receive request")
		return coordCallbackBody{}
	}
}

// TestStart_WithCallbackURL_ReturnsSink verifies that Start returns a non-nil
// HTTPCallbackSink when sess.CallbackURL is configured.
func TestStart_WithCallbackURL_ReturnsSink(t *testing.T) {
	srv := newH2CTestServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	coord, pool, _, disp, _ := newStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runStack(ctx, pool, disp)

	mgr := newMgr()
	sess := createSessionWithCallback(t, mgr, "cb-sink", srv.URL)

	cbSink, err := coord.Start(sess)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if cbSink == nil {
		t.Error("Start returned nil cbSink for session with CallbackURL set")
	}
	if cbSink != nil && cbSink.URL != srv.URL {
		t.Errorf("cbSink.URL = %q, want %q", cbSink.URL, srv.URL)
	}
}

// TestStart_NoCallbackURL_ReturnsNilSink verifies that Start returns nil cbSink
// when sess.CallbackURL is empty — guards the existing behaviour assumed by other tests.
func TestStart_NoCallbackURL_ReturnsNilSink(t *testing.T) {
	coord, pool, _, disp, _ := newStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runStack(ctx, pool, disp)

	mgr := newMgr()
	sess := createSession(t, mgr, "no-url")

	cbSink, err := coord.Start(sess)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if cbSink != nil {
		t.Errorf("Start returned non-nil cbSink for empty CallbackURL, got URL=%q", cbSink.URL)
	}
}

// TestStart_WithCallbackURL_ResultReachesH2CServer là test end-to-end chính:
// RTP packet → decode → chunk → gRPC (fake) recv → Dispatcher → HTTPCallbackSink → H2C POST.
func TestStart_WithCallbackURL_ResultReachesH2CServer(t *testing.T) {
	received := make(chan coordCallbackBody, 4)
	srv := newH2CTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b coordCallbackBody
		json.NewDecoder(r.Body).Decode(&b)
		w.WriteHeader(http.StatusOK)
		select {
		case received <- b:
		default:
		}
	}))
	defer srv.Close()

	coord, pool, _, disp, dialer := newStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runStack(ctx, pool, disp)

	mgr := newMgr()
	sess := createSessionWithCallback(t, mgr, "e2e", srv.URL)

	if _, err := coord.Start(sess); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Gửi packet để đảm bảo pipeline goroutines đang chạy.
	sess.PacketQueue <- pcmuPacket(sess.ID, 1)

	// Đợi AI client được tạo và chunk đến nơi.
	client := waitForClient(t, dialer, sess.ID)
	select {
	case <-client.chunks:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: audio chunk did not reach AI client")
	}

	// Inject recognition result từ phía AI (simulate gRPC recv).
	want := pipeline.RecognitionResult{
		SessionID: sess.ID,
		Text:      "xin chào từ AI",
		IsFinal:   true,
		Language:  "vi",
	}
	client.results <- want

	// Verify kết quả đến H2C callback server qua Dispatcher → HTTPCallbackSink.
	got := recvCoordCallback(t, received)

	if got.EventType != "asr.transcript.final" {
		t.Errorf("event_type = %q, want %q", got.EventType, "asr.transcript.final")
	}
	if got.Text != want.Text {
		t.Errorf("text = %q, want %q", got.Text, want.Text)
	}
	if got.SessionID != want.SessionID {
		t.Errorf("session_id = %q, want %q", got.SessionID, want.SessionID)
	}
	if !got.IsFinal {
		t.Error("is_final = false, want true")
	}
}

// TestStart_WithCallbackURL_PartialResult verifies partial results reach the
// callback server with event_type="asr.transcript.partial".
func TestStart_WithCallbackURL_PartialResult(t *testing.T) {
	received := make(chan coordCallbackBody, 4)
	srv := newH2CTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b coordCallbackBody
		json.NewDecoder(r.Body).Decode(&b)
		w.WriteHeader(http.StatusOK)
		select {
		case received <- b:
		default:
		}
	}))
	defer srv.Close()

	coord, pool, _, disp, dialer := newStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runStack(ctx, pool, disp)

	mgr := newMgr()
	sess := createSessionWithCallback(t, mgr, "partial", srv.URL)

	if _, err := coord.Start(sess); err != nil {
		t.Fatalf("Start: %v", err)
	}

	sess.PacketQueue <- pcmuPacket(sess.ID, 1)
	client := waitForClient(t, dialer, sess.ID)
	select {
	case <-client.chunks:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: chunk did not reach AI client")
	}

	client.results <- pipeline.RecognitionResult{
		SessionID: sess.ID,
		Text:      "xin",
		IsFinal:   false,
	}

	got := recvCoordCallback(t, received)

	if got.EventType != "asr.transcript.partial" {
		t.Errorf("event_type = %q, want %q", got.EventType, "asr.transcript.partial")
	}
	if got.IsFinal {
		t.Error("is_final = true for partial result, want false")
	}
}

// TestStart_WithCallbackURL_CloseSession_StopsDelivery verifies that results
// no longer reach the callback server after the session is closed (sink unregistered).
func TestStart_WithCallbackURL_CloseSession_StopsDelivery(t *testing.T) {
	received := make(chan coordCallbackBody, 8)
	srv := newH2CTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b coordCallbackBody
		json.NewDecoder(r.Body).Decode(&b)
		w.WriteHeader(http.StatusOK)
		select {
		case received <- b:
		default:
		}
	}))
	defer srv.Close()

	coord, pool, _, disp, dialer := newStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runStack(ctx, pool, disp)

	mgr := newMgr()
	sess := createSessionWithCallback(t, mgr, "close", srv.URL)

	if _, err := coord.Start(sess); err != nil {
		t.Fatalf("Start: %v", err)
	}

	sess.PacketQueue <- pcmuPacket(sess.ID, 1)
	client := waitForClient(t, dialer, sess.ID)
	select {
	case <-client.chunks:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: chunk did not reach AI client")
	}

	// Deliver one result before close — must arrive.
	client.results <- pipeline.RecognitionResult{SessionID: sess.ID, Text: "before", IsFinal: true}
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("first result not delivered before session close")
	}

	// Close session → cleanup goroutine calls UnregisterSinks.
	mgr.Close(sess.ID)
	time.Sleep(60 * time.Millisecond)

	// Push directly to dispatcher after unregister — sink should not fire.
	disp.Push(context.Background(), pipeline.RecognitionResult{
		SessionID: sess.ID, Text: "after-close", IsFinal: true,
	})
	time.Sleep(100 * time.Millisecond)

	if n := len(received); n != 0 {
		t.Errorf("received %d results after session close, want 0", n)
	}
}

// TestStart_MultiSession_WithCallbackURL_Isolation verifies that each session's
// results reach only its own callback URL, not the other session's server.
func TestStart_MultiSession_WithCallbackURL_Isolation(t *testing.T) {
	recv1 := make(chan coordCallbackBody, 4)
	recv2 := make(chan coordCallbackBody, 4)

	srv1 := newH2CTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b coordCallbackBody
		json.NewDecoder(r.Body).Decode(&b)
		w.WriteHeader(http.StatusOK)
		select {
		case recv1 <- b:
		default:
		}
	}))
	srv2 := newH2CTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b coordCallbackBody
		json.NewDecoder(r.Body).Decode(&b)
		w.WriteHeader(http.StatusOK)
		select {
		case recv2 <- b:
		default:
		}
	}))
	defer srv1.Close()
	defer srv2.Close()

	coord, pool, _, disp, dialer := newStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runStack(ctx, pool, disp)

	mgr := newMgr()
	sess1 := createSessionWithCallback(t, mgr, "iso-1", srv1.URL)
	sess2 := createSessionWithCallback(t, mgr, "iso-2", srv2.URL)

	if _, err := coord.Start(sess1); err != nil {
		t.Fatalf("Start sess1: %v", err)
	}
	if _, err := coord.Start(sess2); err != nil {
		t.Fatalf("Start sess2: %v", err)
	}

	// Bring both pipelines live.
	sess1.PacketQueue <- pcmuPacket(sess1.ID, 1)
	sess2.PacketQueue <- pcmuPacket(sess2.ID, 1)
	c1 := waitForClient(t, dialer, sess1.ID)
	c2 := waitForClient(t, dialer, sess2.ID)

	waitChunk := func(client *fakeClient, label string) {
		t.Helper()
		select {
		case <-client.chunks:
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for chunk at %s", label)
		}
	}
	waitChunk(c1, "sess1 AI client")
	waitChunk(c2, "sess2 AI client")

	// Inject results.
	c1.results <- pipeline.RecognitionResult{SessionID: sess1.ID, Text: "session-one", IsFinal: true}
	c2.results <- pipeline.RecognitionResult{SessionID: sess2.ID, Text: "session-two", IsFinal: true}

	got1 := recvCoordCallback(t, recv1)
	got2 := recvCoordCallback(t, recv2)

	if got1.Text != "session-one" || got1.SessionID != sess1.ID {
		t.Errorf("srv1 got text=%q session_id=%q, want 'session-one'/%s", got1.Text, got1.SessionID, sess1.ID)
	}
	if got2.Text != "session-two" || got2.SessionID != sess2.ID {
		t.Errorf("srv2 got text=%q session_id=%q, want 'session-two'/%s", got2.Text, got2.SessionID, sess2.ID)
	}
	if len(recv1) != 0 || len(recv2) != 0 {
		t.Error("unexpected extra callbacks — cross-session contamination detected")
	}
}
