package controlplane

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"
	"golang.org/x/net/http2"

	"media-ai-gateway/internal/ai"
	"media-ai-gateway/internal/pipeline"
	"media-ai-gateway/internal/result"
	"media-ai-gateway/internal/session"
)

// ---------- fakes ----------

type fakeStarter struct {
	err          error
	videoErr     error
	videoFailAt  int
	videoStarted int
	started      []string
	sessions     []*session.Session
}

func (f *fakeStarter) Start(sess *session.Session) (*result.HTTPCallbackSink, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.started = append(f.started, sess.ID)
	f.sessions = append(f.sessions, sess)
	return nil, nil
}

func (f *fakeStarter) UpdateCallbackSink(_, _ string) *result.HTTPCallbackSink { return nil }
func (f *fakeStarter) StartMockResultPump(_ *session.Session, _ string)        {}
func (f *fakeStarter) StartVideo(_ *session.Session) error {
	f.videoStarted++
	if f.videoFailAt == 0 || f.videoStarted == f.videoFailAt {
		return f.videoErr
	}
	return nil
}

type noopDialer struct{}

func (noopDialer) Dial(_ context.Context, _, _, _, _ string) (ai.StreamClient, error) {
	return nil, fmt.Errorf("noop")
}

// ---------- test stack ----------

func newTestServer(t *testing.T, coord Starter) *Server {
	t.Helper()
	mgr := session.NewManager(session.ManagerConfig{
		MaxSessions:     10,
		PacketQueueSize: 8,
		AudioQueueSize:  8,
		ResultQueueSize: 8,
		IdleTimeout:     time.Minute,
		GCInterval:      time.Minute,
	})
	pool := pipeline.NewWorkerPool(pipeline.WorkerPoolConfig{Workers: 0, QueueSize: 8})
	aiMgr := ai.NewManager(ai.Config{MaxActiveStreams: 10, StreamTimeout: time.Minute}, noopDialer{})
	disp := result.NewDispatcher(result.DefaultConfig())

	return NewServer(DefaultServerConfig(), mgr, coord, pool, aiMgr, disp)
}

// newTestServerWithVideo tạo Server với port allocator THẬT (bind UDP socket
// thật qua rawrtp.StartSessionListener/StartVideoSessionListener) và
// cfg.VideoEnabled=true — dùng cho test nhánh video-only qua HTTP thật
// (newTestServer mặc định KHÔNG cấp port thật, portAlloc=nil, không đủ để
// test đường end-to-end handleAnswer→StartVideoSessionListener).
func newTestServerWithVideo(t *testing.T, coord Starter, portStart, portEnd int) *Server {
	t.Helper()
	mgr := session.NewManager(session.ManagerConfig{
		MaxSessions:          10,
		PacketQueueSize:      8,
		AudioQueueSize:       8,
		ResultQueueSize:      8,
		VideoPacketQueueSize: 32,
		VideoFrameQueueSize:  8,
		IdleTimeout:          time.Minute,
		GCInterval:           time.Minute,
	})
	pool := pipeline.NewWorkerPool(pipeline.WorkerPoolConfig{Workers: 0, QueueSize: 8})
	aiMgr := ai.NewManager(ai.Config{MaxActiveStreams: 10, StreamTimeout: time.Minute}, noopDialer{})
	disp := result.NewDispatcher(result.DefaultConfig())

	cfg := DefaultServerConfig()
	cfg.VideoEnabled = true
	cfg.RTPBindIP = "127.0.0.1"
	cfg.RTPPortStart = portStart
	cfg.RTPPortEnd = portEnd
	cfg.RTPPublicIP = "127.0.0.1"
	server := NewServer(cfg, mgr, coord, pool, aiMgr, disp)
	t.Cleanup(func() {
		mgr.Close(testCallID + "-tcore")
		mgr.Close(testCallID + "-taccess")
		waitPortsReleased(t, server)
	})
	return server
}

func postJSON(t *testing.T, s *Server, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	return w
}

func getPath(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	return w
}

func deletePath(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodDelete, path, nil)
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	return w
}

func decodeJSON(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.NewDecoder(w.Body).Decode(v); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
}

func assertStatus(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Errorf("status: want %d, got %d — body: %s", want, w.Code, w.Body.String())
	}
}

func assertContentType(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type: want application/json, got %q", ct)
	}
}

// validAnswerReq trả SessionEvent cho ANSWER event với speech_to_text service.
// callId mặc định "call-sess-1" (khớp với notifyPath/sessionPath bên dưới).
func validAnswerReq() SessionEvent {
	return SessionEvent{
		CallID:          "call-sess-1",
		Event:           "ANSWER",
		SelectedService: "speech_to_text",
	}
}

const testCallID = "call-sess-1"

func notifyPath(callId string) string {
	return "/v1/vonras/call-sessions/" + callId + "/notify-event"
}

func ctrlResultPath(callId string) string {
	return "/v1/vonras/call-sessions/" + callId + "/ctrl-result"
}

func sessionPath(callId string) string {
	return "/v1/vonras/call-sessions/" + callId
}

// ---------- POST notify-event ----------

func TestNotifyEvent_Begin_OK(t *testing.T) {
	s := newTestServer(t, &fakeStarter{})
	req := SessionEvent{CallID: testCallID, Event: "BEGIN"}
	w := postJSON(t, s, notifyPath(testCallID), req)
	assertStatus(t, w, http.StatusOK)
}

func TestNotifyEvent_Answer_Created(t *testing.T) {
	coord := &fakeStarter{}
	s := newTestServer(t, coord)

	w := postJSON(t, s, notifyPath(testCallID), validAnswerReq())

	assertStatus(t, w, http.StatusCreated)
	assertContentType(t, w)

	var resp SessionResponse
	decodeJSON(t, w, &resp)
	if resp.SessionID != testCallID {
		t.Errorf("session_id: want %s, got %s", testCallID, resp.SessionID)
	}
	if resp.Codec != "PCMU" {
		t.Errorf("codec: want PCMU, got %s", resp.Codec)
	}
	if resp.SampleRate != 8000 {
		t.Errorf("sample_rate: want 8000, got %d", resp.SampleRate)
	}
	if resp.Channels != 1 {
		t.Errorf("channels: want 1, got %d", resp.Channels)
	}
	if resp.Status == "" {
		t.Error("status should not be empty")
	}
	if resp.Task != "speech_to_text" {
		t.Errorf("task: want speech_to_text, got %s", resp.Task)
	}
	// 1 call → 2 sessions: tcore + taccess
	wantStarted := []string{testCallID + "-tcore", testCallID + "-taccess"}
	if len(coord.started) != 2 {
		t.Errorf("coordinator started %d sessions, want 2; got %v", len(coord.started), coord.started)
	} else {
		for i, want := range wantStarted {
			if coord.started[i] != want {
				t.Errorf("coord.started[%d]: want %s, got %s", i, want, coord.started[i])
			}
		}
	}
}

func TestNotifyEvent_Answer_UnknownService(t *testing.T) {
	// Services not yet handled (e.g. fun_calling) should return 200 OK without creating a session.
	coord := &fakeStarter{}
	s := newTestServer(t, coord)

	req := SessionEvent{
		CallID:          testCallID,
		Event:           "ANSWER",
		SelectedService: "fun_calling",
	}
	w := postJSON(t, s, notifyPath(testCallID), req)
	assertStatus(t, w, http.StatusOK)

	// No session should have been created.
	wg := getPath(t, s, sessionPath(testCallID))
	assertStatus(t, wg, http.StatusNotFound)
}

func TestNotifyEvent_Answer_InvalidJSON(t *testing.T) {
	s := newTestServer(t, &fakeStarter{})
	r := httptest.NewRequest(http.MethodPost, notifyPath(testCallID), strings.NewReader("{bad json"))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	assertStatus(t, w, http.StatusBadRequest)
}

func TestNotifyEvent_Answer_DuplicateID(t *testing.T) {
	s := newTestServer(t, &fakeStarter{})
	postJSON(t, s, notifyPath(testCallID), validAnswerReq())
	w := postJSON(t, s, notifyPath(testCallID), validAnswerReq())
	assertStatus(t, w, http.StatusConflict)
}

func TestNotifyEvent_Answer_CoordFail_RollsBack(t *testing.T) {
	coord := &fakeStarter{err: fmt.Errorf("pipeline broke")}
	s := newTestServer(t, coord)

	w := postJSON(t, s, notifyPath(testCallID), validAnswerReq())
	assertStatus(t, w, http.StatusBadRequest)

	wg := getPath(t, s, sessionPath(testCallID))
	assertStatus(t, wg, http.StatusNotFound)
}

func TestNotifyEvent_Answer_MaxSessions(t *testing.T) {
	// 1 call needs 2 session slots (tcore + taccess). MaxSessions=2 → 1st call OK, 2nd fails.
	mgr := session.NewManager(session.ManagerConfig{
		MaxSessions:     2,
		PacketQueueSize: 8,
		AudioQueueSize:  8,
		ResultQueueSize: 8,
		IdleTimeout:     time.Minute,
		GCInterval:      time.Minute,
	})
	pool := pipeline.NewWorkerPool(pipeline.WorkerPoolConfig{Workers: 0, QueueSize: 8})
	aiMgr := ai.NewManager(ai.Config{MaxActiveStreams: 10, StreamTimeout: time.Minute}, noopDialer{})
	disp := result.NewDispatcher(result.DefaultConfig())
	s := NewServer(DefaultServerConfig(), mgr, &fakeStarter{}, pool, aiMgr, disp)

	// First call uses both slots.
	postJSON(t, s, notifyPath(testCallID), validAnswerReq())

	// Second call cannot allocate 2 more sessions.
	req2 := SessionEvent{
		CallID:          "call-sess-2",
		Event:           "ANSWER",
		SelectedService: "speech_to_text",
	}
	w := postJSON(t, s, notifyPath("call-sess-2"), req2)
	assertStatus(t, w, http.StatusServiceUnavailable)
}

func TestNotifyEvent_UnknownEvent(t *testing.T) {
	s := newTestServer(t, &fakeStarter{})
	req := SessionEvent{CallID: testCallID, Event: "RELEASE"}
	w := postJSON(t, s, notifyPath(testCallID), req)
	assertStatus(t, w, http.StatusBadRequest)
}

// ---------- GET /v1/vonras/call-sessions/{callId} ----------

func TestGetCallSession_Found(t *testing.T) {
	s := newTestServer(t, &fakeStarter{})
	postJSON(t, s, notifyPath(testCallID), validAnswerReq())

	w := getPath(t, s, sessionPath(testCallID))
	assertStatus(t, w, http.StatusOK)
	assertContentType(t, w)

	var resp SessionResponse
	decodeJSON(t, w, &resp)
	// session_id in response is always the callID, not the internal tcore/taccess ID
	if resp.SessionID != testCallID {
		t.Errorf("session_id: want %s, got %s", testCallID, resp.SessionID)
	}
	if resp.Codec != "PCMU" {
		t.Errorf("codec mismatch")
	}
}

func TestGetCallSession_NotFound(t *testing.T) {
	s := newTestServer(t, &fakeStarter{})
	w := getPath(t, s, sessionPath("nope"))
	assertStatus(t, w, http.StatusNotFound)
}

// ---------- DELETE /v1/vonras/call-sessions/{callId} ----------

func TestDeleteCallSession_NoContent(t *testing.T) {
	s := newTestServer(t, &fakeStarter{})
	postJSON(t, s, notifyPath(testCallID), validAnswerReq())

	w := deletePath(t, s, sessionPath(testCallID))
	assertStatus(t, w, http.StatusNoContent)
}

func TestDeleteCallSession_NotFound(t *testing.T) {
	s := newTestServer(t, &fakeStarter{})
	w := deletePath(t, s, sessionPath("nope"))
	assertStatus(t, w, http.StatusNotFound)
}

func TestDeleteCallSession_GoneAfterDelete(t *testing.T) {
	s := newTestServer(t, &fakeStarter{})
	postJSON(t, s, notifyPath(testCallID), validAnswerReq())
	deletePath(t, s, sessionPath(testCallID))

	w := getPath(t, s, sessionPath(testCallID))
	assertStatus(t, w, http.StatusNotFound)
}

// ---------- POST /v1/vonras/call-sessions/{callId}/ctrl-result ----------

func TestCtrlResult_OK(t *testing.T) {
	s := newTestServer(t, &fakeStarter{})
	postJSON(t, s, notifyPath(testCallID), validAnswerReq())

	req := CtrlResultRequest{
		CallID: testCallID,
		MediaResources: &MediaResources{
			TCore: MediaResourceInfo{
				ContextID:   "ctx-1",
				Termination: TerminationInfo{TerminationID: "term-1"},
				CallbackURL: "http://callback.example.com/tcore",
			},
			TAccess: MediaResourceInfo{
				ContextID:   "ctx-2",
				Termination: TerminationInfo{TerminationID: "term-2"},
				CallbackURL: "http://callback.example.com/taccess",
			},
		},
	}
	w := postJSON(t, s, ctrlResultPath(testCallID), req)
	assertStatus(t, w, http.StatusOK)
	assertContentType(t, w)

	var resp SessionResponse
	decodeJSON(t, w, &resp)
	if resp.SessionID != testCallID {
		t.Errorf("session_id: want %s, got %s", testCallID, resp.SessionID)
	}
}

func TestCtrlResult_NotFound(t *testing.T) {
	s := newTestServer(t, &fakeStarter{})
	req := CtrlResultRequest{CallID: "nope"}
	w := postJSON(t, s, ctrlResultPath("nope"), req)
	assertStatus(t, w, http.StatusNotFound)
}

// ---------- GET /health ----------

func TestHealthReady_OK(t *testing.T) {
	s := newTestServer(t, &fakeStarter{})
	w := getPath(t, s, "/health/ready")
	assertStatus(t, w, http.StatusOK)

	var body map[string]string
	decodeJSON(t, w, &body)
	if body["status"] != "ready" {
		t.Errorf("status: want ready, got %s", body["status"])
	}
}

func TestHealthReady_NotReady_HasReason(t *testing.T) {
	s := newTestServer(t, &fakeStarter{})
	s.SetMemThreshold(1)

	w := getPath(t, s, "/health/ready")
	assertStatus(t, w, http.StatusServiceUnavailable)

	var body map[string]any
	decodeJSON(t, w, &body)
	if body["status"] != "not_ready" {
		t.Errorf("status: want not_ready, got %v", body["status"])
	}
	if body["reason"] == "" || body["reason"] == nil {
		t.Error("reason should be non-empty when not ready")
	}
	if body["retry_after_ms"] == nil {
		t.Error("retry_after_ms should be present when not ready")
	}
}

func TestHealthReady_WorkerRegistry_NoWorker_NotReady(t *testing.T) {
	s := newTestServer(t, &fakeStarter{})
	reg := ai.NewWorkerRegistry(0) // empty
	s.SetWorkerRegistry(reg)

	w := getPath(t, s, "/health/ready")
	assertStatus(t, w, http.StatusServiceUnavailable)

	var body map[string]any
	decodeJSON(t, w, &body)
	if body["reason"] != "no_ai_worker" {
		t.Errorf("reason: want no_ai_worker, got %v", body["reason"])
	}
}

func TestHealth_OK(t *testing.T) {
	s := newTestServer(t, &fakeStarter{})
	w := getPath(t, s, "/health")
	assertStatus(t, w, http.StatusOK)
	assertContentType(t, w)

	var body map[string]string
	decodeJSON(t, w, &body)
	if body["status"] != "ok" {
		t.Errorf("status: want ok, got %s", body["status"])
	}
}

// ---------- GET /v1/stats ----------

func TestStats_OK(t *testing.T) {
	s := newTestServer(t, &fakeStarter{})
	postJSON(t, s, notifyPath(testCallID), validAnswerReq())

	w := getPath(t, s, "/v1/stats")
	assertStatus(t, w, http.StatusOK)
	assertContentType(t, w)

	var resp StatsResponse
	decodeJSON(t, w, &resp)
	// 1 call creates 2 sessions: tcore + taccess
	if resp.Sessions != 2 {
		t.Errorf("sessions: want 2 (1 call = 2 sessions), got %d", resp.Sessions)
	}
}

// ---------- GET /metrics ----------

func TestMetrics_ContainsRequiredMetrics(t *testing.T) {
	s := newTestServer(t, &fakeStarter{})
	w := getPath(t, s, "/metrics")
	assertStatus(t, w, http.StatusOK)

	body := w.Body.String()

	required := []string{
		"media_ai_sessions_active",
		"media_ai_sessions_max",
		"media_ai_ai_streams_active",
		"media_ai_pool_sessions_active",
		"media_ai_pool_queue_len",
		"media_ai_pool_queue_cap",
		"media_ai_pool_submitted_total",
		"media_ai_pool_dropped_total",
		"media_ai_pool_processed_total",
		"media_ai_pool_decode_errors_total",
		"media_ai_dispatcher_queue_len",
		"media_ai_dispatcher_sent_total",
		"media_ai_dispatcher_send_errors_total",
		"media_ai_gateway_nodes_registered",
		"media_ai_scrape_timestamp_ms",
		"media_ai_ai_send_errors_total",
		"media_ai_ai_recv_errors_total",
		"media_ai_ai_reconnects_total",
		"media_ai_result_partial_total",
		"media_ai_result_final_total",
		"media_ai_result_queue_dropped_total",
		"media_ai_callback_retry_total",
		"media_ai_goroutines_current",
		"media_ai_memory_usage_bytes",
	}
	for _, name := range required {
		if !strings.Contains(body, name) {
			t.Errorf("metric %q not found in /metrics output", name)
		}
	}
}

func TestMetrics_ContentType(t *testing.T) {
	s := newTestServer(t, &fakeStarter{})
	w := getPath(t, s, "/metrics")
	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type: want text/plain, got %q", ct)
	}
}

func TestMetrics_PoolQueueCapReflectsConfig(t *testing.T) {
	s := newTestServer(t, &fakeStarter{})
	w := getPath(t, s, "/metrics")
	body := w.Body.String()
	if !strings.Contains(body, "media_ai_pool_queue_cap 8") {
		t.Errorf("expected media_ai_pool_queue_cap 8 in metrics output, got:\n%s", body)
	}
}

// ---------- h2c integration ----------

func TestH2C_ServerServesHTTP2(t *testing.T) {
	s := newTestServer(t, &fakeStarter{})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	s.inner.Addr = addr
	go func() { _ = s.inner.Serve(ln) }()
	t.Cleanup(func() { _ = s.inner.Close() })

	h2cTransport := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, remoteAddr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, remoteAddr)
		},
	}
	client := &http.Client{Transport: h2cTransport}

	resp, err := client.Get("http://" + addr + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: want 200, got %d", resp.StatusCode)
	}
	if resp.Proto != "HTTP/2.0" {
		t.Errorf("proto: want HTTP/2.0, got %s", resp.Proto)
	}
	if !strings.Contains(string(body), "ok") {
		t.Errorf("body: want 'ok', got %s", body)
	}
}

func TestH2C_FullSessionLifecycle(t *testing.T) {
	s := newTestServer(t, &fakeStarter{})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	s.inner.Addr = addr
	go func() { _ = s.inner.Serve(ln) }()
	t.Cleanup(func() { _ = s.inner.Close() })

	h2cTransport := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, remoteAddr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, remoteAddr)
		},
	}
	client := &http.Client{Transport: h2cTransport}
	base := "http://" + addr

	body, _ := json.Marshal(validAnswerReq())
	resp, err := client.Post(base+notifyPath(testCallID), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status: want 201, got %d", resp.StatusCode)
	}
	if resp.Proto != "HTTP/2.0" {
		t.Errorf("POST proto: want HTTP/2.0, got %s", resp.Proto)
	}

	resp, err = client.Get(base + sessionPath(testCallID))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status: want 200, got %d", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodDelete, base+sessionPath(testCallID), nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status: want 204, got %d", resp.StatusCode)
	}
}

// ---------- POST notify-event ANSWER — video-only (change_background) ----------

// TestNotifyEvent_Answer_VideoOnly_ChangeBackground là test handler (gọi
// qua s.routes().ServeHTTP, đúng đường handleAnswer sản xuất dùng) cho
// nhánh video-only vừa thêm — trước bản sửa này chỉ có
// TestStartVideoOnly_NoAudioCodec ở tầng Coordinator (bỏ qua HTTP hoàn
// toàn), CHƯA có test nào exercise handleAnswer() thật với 1 ServiceProfile
// NeedsAudio=false.
//
// Dùng newTestServerWithVideo (port allocator + video enabled THẬT) — bind
// UDP socket thật qua StartVideoSessionListener, không mock.
func TestNotifyEvent_Answer_VideoOnly_ChangeBackground(t *testing.T) {
	coord := &fakeStarter{}
	start, end := availableUDPRange(t, 10)
	s := newTestServerWithVideo(t, coord, start, end)

	req := SessionEvent{
		CallID:          testCallID,
		Event:           "ANSWER",
		SelectedService: "change_background",
	}
	w := postJSON(t, s, notifyPath(testCallID), req)

	assertStatus(t, w, http.StatusCreated)
	assertContentType(t, w)

	var resp SessionResponse
	decodeJSON(t, w, &resp)

	if resp.Codec != "" {
		t.Errorf("codec: want rỗng (video-only, không audio), got %q", resp.Codec)
	}
	if resp.TCoreRTPPort != 0 || resp.TAccessRTPPort != 0 {
		t.Errorf("audio RTP port: want 0,0 (ServiceProfile NeedsAudio=false không được cấp port audio), got tcore=%d taccess=%d", resp.TCoreRTPPort, resp.TAccessRTPPort)
	}
	if resp.TCoreVideoRTPPort == 0 || resp.TAccessVideoRTPPort == 0 {
		t.Errorf("video RTP port: kỳ vọng được cấp (khác 0), got tcore=%d taccess=%d", resp.TCoreVideoRTPPort, resp.TAccessVideoRTPPort)
	}
	if resp.TCoreLocalNonDcMedia != nil {
		t.Error("tcore_local_non_dc_media: kỳ vọng nil (không có audio track)")
	}
	if resp.TCoreLocalNonDcVideoMedia == nil {
		t.Fatal("tcore_local_non_dc_video_media: kỳ vọng có SDP m=video")
	}
	if !strings.HasPrefix(resp.TCoreLocalNonDcVideoMedia.SDPMLine, "video ") {
		t.Errorf("SDP m-line: want bắt đầu bằng \"video \", got %q", resp.TCoreLocalNonDcVideoMedia.SDPMLine)
	}

	// 1 call → 2 session (tcore+taccess) như audio, dù không có audio codec.
	if len(coord.sessions) != 2 {
		t.Fatalf("coordinator started %d session, want 2", len(coord.sessions))
	}
	for _, sess := range coord.sessions {
		if sess.Codec != "" {
			t.Errorf("session %s: Codec = %q, want rỗng", sess.ID, sess.Codec)
		}
		if !sess.VideoEnabled {
			t.Errorf("session %s: VideoEnabled = false, want true", sess.ID)
		}
	}

	// Dọn session để giải phóng UDP socket đã bind thật (tránh rò rỉ giữa các test).
	s.sessionMgr.Close(testCallID + "-tcore")
	s.sessionMgr.Close(testCallID + "-taccess")
}

// TestNotifyEvent_Answer_VideoOnly_BearerCapabilityStillWorks verify service
// audio bình thường (speech_to_text) VẪN nhận video khi bearerCapability:
// VIDEO — đảm bảo thêm ServiceProfile.NeedsVideo không phá vỡ đường tín
// hiệu bearerCapability hiện có (2 cách bật video độc lập, cả 2 phải chạy được).
func TestNotifyEvent_Answer_VideoOnly_BearerCapabilityStillWorks(t *testing.T) {
	coord := &fakeStarter{}
	start, end := availableUDPRange(t, 10)
	s := newTestServerWithVideo(t, coord, start, end)

	req := SessionEvent{
		CallID:           testCallID,
		Event:            "ANSWER",
		SelectedService:  "speech_to_text",
		BearerCapability: "VIDEO",
	}
	w := postJSON(t, s, notifyPath(testCallID), req)
	assertStatus(t, w, http.StatusCreated)

	var resp SessionResponse
	decodeJSON(t, w, &resp)

	if resp.Codec != "PCMU" {
		t.Errorf("codec: want PCMU (service vẫn cần audio), got %q", resp.Codec)
	}
	if resp.TCoreRTPPort == 0 {
		t.Error("audio RTP port: kỳ vọng được cấp (service cần audio)")
	}
	if resp.TCoreVideoRTPPort == 0 {
		t.Error("video RTP port: kỳ vọng được cấp (bearerCapability=VIDEO)")
	}

	// Audio must still arrive on its dedicated UDP ports after adding video.
	for _, leg := range []struct {
		id   string
		port int
	}{{"tcore", resp.TCoreRTPPort}, {"taccess", resp.TAccessRTPPort}} {
		sess, ok := s.sessionMgr.Get(testCallID + "-" + leg.id)
		if !ok {
			t.Fatal("missing audio session")
		}
		conn, err := net.Dial("udp", fmt.Sprintf("127.0.0.1:%d", leg.port))
		if err != nil {
			t.Fatal(err)
		}
		pkt := rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 0, SSRC: 123, SequenceNumber: 1}, Payload: []byte{0xff, 0xff}}
		data, err := pkt.Marshal()
		if err != nil {
			conn.Close()
			t.Fatal(err)
		}
		_, err = conn.Write(data)
		conn.Close()
		if err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-sess.PacketQueue:
			if got.Codec != "PCMU" || len(got.Payload) != 2 {
				t.Fatalf("bad audio RTP packet: %+v", got)
			}
		case <-time.After(time.Second):
			t.Fatal("audio UDP listener did not forward RTP")
		}
	}

	s.sessionMgr.Close(testCallID + "-tcore")
	s.sessionMgr.Close(testCallID + "-taccess")
}
