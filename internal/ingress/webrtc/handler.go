package webrtc

import (
	"encoding/json"
	"net/http"
	"sync"

	pion "github.com/pion/webrtc/v4"

	"media-ai-gateway/internal/result"
	"media-ai-gateway/internal/session"
)

// Handler handles WebRTC signaling (SDP offer/answer) over HTTP.
//
// A client must first create a session via POST /v1/sessions with
// source_type="webrtc", then call POST /v1/webrtc/offer to begin the
// PeerConnection. ICE candidates are gathered inline (the answer SDP
// returned by ServeOffer is complete — no trickle ICE round-trip needed).
type Handler struct {
	api  *pion.API
	cfg  Config
	mgr  *session.Manager
	disp *result.Dispatcher

	mu  sync.RWMutex
	pcs map[string]*PeerSession // sessionID → active PeerSession
}

// NewHandler creates a Handler using a pre-built pion API and session manager.
func NewHandler(api *pion.API, cfg Config, mgr *session.Manager) *Handler {
	return &Handler{
		api: api,
		cfg: cfg,
		mgr: mgr,
		pcs: make(map[string]*PeerSession),
	}
}

// SetDispatcher wires a result.Dispatcher so that a DataChannelSink is
// registered for every new PeerSession, enabling the gateway to push
// AI transcripts back to the UE over the WebRTC DataChannel.
func (h *Handler) SetDispatcher(disp *result.Dispatcher) { h.disp = disp }

// OfferRequest is the JSON body for POST /v1/webrtc/offer.
type OfferRequest struct {
	SessionID string `json:"session_id"`
	SDP       string `json:"sdp"`
	Type      string `json:"type"` // must be "offer"
}

// AnswerResponse is returned on successful offer processing.
type AnswerResponse struct {
	SessionID    string `json:"session_id"`
	SDP          string `json:"sdp"`
	Type         string `json:"type"`                    // always "answer"
	UDPProxyPort int    `json:"udp_proxy_port,omitempty"` // AC.6-2: local UDP port for App Server → UE relay; 0 = UDP proxy disabled
}

// ServeOffer implements POST /v1/webrtc/offer.
func (h *Handler) ServeOffer(w http.ResponseWriter, r *http.Request) {
	var req OfferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	switch {
	case req.SessionID == "":
		writeErr(w, http.StatusBadRequest, "session_id required")
		return
	case req.SDP == "":
		writeErr(w, http.StatusBadRequest, "sdp required")
		return
	case req.Type != "offer":
		writeErr(w, http.StatusBadRequest, "type must be 'offer'")
		return
	}

	sess, ok := h.mgr.Get(req.SessionID)
	if !ok {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if sess.SourceType != "webrtc" {
		writeErr(w, http.StatusBadRequest, "session source_type is not 'webrtc'")
		return
	}

	// Close any existing PeerSession for this session (reconnect scenario).
	h.mu.Lock()
	if existing, ok := h.pcs[req.SessionID]; ok {
		existing.Close()
	}
	h.mu.Unlock()

	ps, err := newPeerSession(h.api, h.cfg, sess)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "peerconnection: "+err.Error())
		return
	}

	// Register a DataChannelSink so the dispatcher can push transcripts to the UE.
	if h.disp != nil && ps.DC != nil {
		h.disp.RegisterSink(req.SessionID, result.NewDataChannelSink(req.SessionID, ps.DC))
	}

	h.mu.Lock()
	h.pcs[req.SessionID] = ps
	h.mu.Unlock()

	// Remove the entry when the PeerSession ends (connection closed or session expired).
	go func() {
		<-ps.ctx.Done()
		h.mu.Lock()
		if h.pcs[ps.SessID] == ps {
			delete(h.pcs, ps.SessID)
		}
		h.mu.Unlock()
		_ = ps.pc.Close()
	}()

	if err := ps.pc.SetRemoteDescription(pion.SessionDescription{
		Type: pion.SDPTypeOffer,
		SDP:  req.SDP,
	}); err != nil {
		ps.Close()
		writeErr(w, http.StatusBadRequest, "SetRemoteDescription: "+err.Error())
		return
	}

	answer, err := ps.pc.CreateAnswer(nil)
	if err != nil {
		ps.Close()
		writeErr(w, http.StatusInternalServerError, "CreateAnswer: "+err.Error())
		return
	}
	if err := ps.pc.SetLocalDescription(answer); err != nil {
		ps.Close()
		writeErr(w, http.StatusInternalServerError, "SetLocalDescription: "+err.Error())
		return
	}

	// Block until all ICE candidates are gathered so the returned SDP is complete.
	<-pion.GatheringCompletePromise(ps.pc)

	resp := AnswerResponse{
		SessionID: req.SessionID,
		SDP:       ps.pc.LocalDescription().SDP,
		Type:      "answer",
	}
	if ps.udpProxy != nil {
		resp.UDPProxyPort = ps.udpProxy.LocalPort()
	}
	writeJSON(w, http.StatusOK, resp)
}

// ActiveSessions returns the number of live PeerSessions.
func (h *Handler) ActiveSessions() int {
	h.mu.RLock()
	n := len(h.pcs)
	h.mu.RUnlock()
	return n
}

// helpers — minimal JSON response writers (mirrors controlplane package pattern).

type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}
