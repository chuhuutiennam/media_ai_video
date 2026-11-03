package webrtc

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"

	"media-ai-gateway/internal/session"
)

// makeTestManager returns a session.Manager and a helper to close it.
func makeTestManager(t *testing.T) *session.Manager {
	t.Helper()
	return session.NewManager(session.ManagerConfig{
		MaxSessions:     10,
		PacketQueueSize: 32,
		AudioQueueSize:  8,
		ResultQueueSize: 8,
		IdleTimeout:     time.Minute,
		GCInterval:      time.Minute,
	})
}

// makeHandler builds a Handler with default config (STUN only — no real network needed
// for unit tests that check HTTP-level validation).
func makeHandler(t *testing.T, mgr *session.Manager) *Handler {
	t.Helper()
	api, err := NewAPI(DefaultConfig())
	if err != nil {
		t.Fatalf("NewAPI: %v", err)
	}
	return NewHandler(api, DefaultConfig(), mgr)
}

func postOffer(t *testing.T, h *Handler, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		t.Fatalf("encode: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/webrtc/offer", &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeOffer(w, req)
	return w
}

// ── validation ────────────────────────────────────────────────────────────────

func TestServeOffer_InvalidJSON(t *testing.T) {
	mgr := makeTestManager(t)
	h := makeHandler(t, mgr)

	req := httptest.NewRequest(http.MethodPost, "/v1/webrtc/offer", bytes.NewBufferString("{bad"))
	w := httptest.NewRecorder()
	h.ServeOffer(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestServeOffer_MissingSessionID(t *testing.T) {
	mgr := makeTestManager(t)
	h := makeHandler(t, mgr)

	w := postOffer(t, h, OfferRequest{SDP: "v=0", Type: "offer"})
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestServeOffer_MissingSDP(t *testing.T) {
	mgr := makeTestManager(t)
	h := makeHandler(t, mgr)

	w := postOffer(t, h, OfferRequest{SessionID: "s1", Type: "offer"})
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestServeOffer_WrongType(t *testing.T) {
	mgr := makeTestManager(t)
	h := makeHandler(t, mgr)

	w := postOffer(t, h, OfferRequest{SessionID: "s1", SDP: "v=0", Type: "answer"})
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestServeOffer_SessionNotFound(t *testing.T) {
	mgr := makeTestManager(t)
	h := makeHandler(t, mgr)

	w := postOffer(t, h, OfferRequest{SessionID: "no-such-session", SDP: "v=0", Type: "offer"})
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
}

func TestServeOffer_WrongSourceType(t *testing.T) {
	mgr := makeTestManager(t)
	_, err := mgr.Create(session.SessionConfig{
		ID:         "raw-sess",
		SourceType: "raw_rtp",
		Codec:      "PCMU",
		SampleRate: 8000,
		Channels:   1,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer mgr.Close("raw-sess")

	h := makeHandler(t, mgr)
	w := postOffer(t, h, OfferRequest{SessionID: "raw-sess", SDP: "v=0", Type: "offer"})
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestServeOffer_InvalidSDP(t *testing.T) {
	mgr := makeTestManager(t)
	_, err := mgr.Create(session.SessionConfig{
		ID:         "wrtc-sess",
		SourceType: "webrtc",
		Codec:      "opus",
		SampleRate: 48000,
		Channels:   2,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer mgr.Close("wrtc-sess")

	h := makeHandler(t, mgr)
	// Malformed SDP should cause SetRemoteDescription to fail.
	w := postOffer(t, h, OfferRequest{SessionID: "wrtc-sess", SDP: "not-valid-sdp", Type: "offer"})
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400 for invalid SDP, got %d", w.Code)
	}
}

// ── ActiveSessions counter ────────────────────────────────────────────────────

func TestHandler_ActiveSessions_Initial(t *testing.T) {
	mgr := makeTestManager(t)
	h := makeHandler(t, mgr)
	if n := h.ActiveSessions(); n != 0 {
		t.Errorf("want 0 active sessions, got %d", n)
	}
}

// ── happy-path answer response ────────────────────────────────────────────────

// TestServeOffer_ValidOffer performs a real SDP exchange using a pion offerer
// running in the same process. This verifies the full signaling path including
// PeerConnection creation and ICE gathering (host candidates only, no network).
func TestServeOffer_ValidOffer(t *testing.T) {
	mgr := makeTestManager(t)
	_, err := mgr.Create(session.SessionConfig{
		ID:         "e2e-sess",
		SourceType: "webrtc",
		Codec:      "opus",
		SampleRate: 48000,
		Channels:   2,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer mgr.Close("e2e-sess")

	h := makeHandler(t, mgr)

	// Build a real SDP offer using an offerer PeerConnection.
	offererAPI, err := NewAPI(Config{}) // no ICE servers for loopback test
	if err != nil {
		t.Fatalf("NewAPI: %v", err)
	}
	offerer, err := offererAPI.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatalf("offerer NewPeerConnection: %v", err)
	}
	defer offerer.Close()

	if _, err := offerer.AddTransceiverFromKind(
		pion.RTPCodecTypeAudio,
		pion.RTPTransceiverInit{Direction: pion.RTPTransceiverDirectionSendonly},
	); err != nil {
		t.Fatalf("AddTransceiver: %v", err)
	}

	offer, err := offerer.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if err := offerer.SetLocalDescription(offer); err != nil {
		t.Fatalf("offerer SetLocalDescription: %v", err)
	}
	<-pion.GatheringCompletePromise(offerer)

	// Send offer to handler.
	w := postOffer(t, h, OfferRequest{
		SessionID: "e2e-sess",
		SDP:       offerer.LocalDescription().SDP,
		Type:      "offer",
	})

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — body: %s", w.Code, w.Body.String())
	}

	var ans AnswerResponse
	if err := json.NewDecoder(w.Body).Decode(&ans); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	if ans.Type != "answer" {
		t.Errorf("type: want 'answer', got %q", ans.Type)
	}
	if ans.SessionID != "e2e-sess" {
		t.Errorf("session_id: want e2e-sess, got %q", ans.SessionID)
	}
	if ans.SDP == "" {
		t.Error("sdp is empty")
	}

	// ActiveSessions should be 1 after a successful offer.
	if n := h.ActiveSessions(); n != 1 {
		t.Errorf("want 1 active session, got %d", n)
	}
}
