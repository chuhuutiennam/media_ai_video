package webrtc

import (
	"context"

	pion "github.com/pion/webrtc/v4"

	"media-ai-gateway/internal/session"
)

// PeerSession couples a pion PeerConnection with a gateway session.
//
// Lifecycle:
//   - Created by Handler.ServeOffer after SDP negotiation.
//   - OnTrack fires readRTPLoop in a dedicated goroutine.
//   - OnConnectionStateChange → Failed/Closed/Disconnected cancels ctx.
//   - When sess.Ctx is cancelled (session closed upstream), ctx is also cancelled
//     because ctx is derived from sess.Ctx.
type PeerSession struct {
	pc     *pion.PeerConnection
	ctx    context.Context
	cancel context.CancelFunc
	SessID string
	// DC is the DataChannel used to send AI transcripts back to the UE.
	// Created during newPeerSession; nil if DataChannel creation failed.
	DC *pion.DataChannel
	// udpProxy is set when Mode 2.2 (UDP Proxy) is configured.
	// Nil when UDP proxy is disabled.
	udpProxy *udpProxy
}

// newPeerSession creates a PeerConnection wired to the given session.
// The connection is recvonly (gateway receives audio, does not send).
func newPeerSession(api *pion.API, cfg Config, sess *session.Session) (*PeerSession, error) {
	iceServers := make([]pion.ICEServer, len(cfg.ICEServers))
	for i, s := range cfg.ICEServers {
		iceServers[i] = pion.ICEServer{
			URLs:       s.URLs,
			Username:   s.Username,
			Credential: s.Credential,
		}
	}

	pc, err := api.NewPeerConnection(pion.Configuration{
		ICEServers: iceServers,
	})
	if err != nil {
		return nil, err
	}

	// Declare recvonly audio transceiver so the SDP answer includes an audio line.
	if _, err := pc.AddTransceiverFromKind(
		pion.RTPCodecTypeAudio,
		pion.RTPTransceiverInit{Direction: pion.RTPTransceiverDirectionRecvonly},
	); err != nil {
		_ = pc.Close()
		return nil, err
	}

	// Create the DataChannel used to send AI transcripts back to the UE.
	dc, err := pc.CreateDataChannel("transcript", nil)
	if err != nil {
		_ = pc.Close()
		return nil, err
	}

	// ctx is a child of sess.Ctx — cancels automatically when the session is closed.
	ctx, cancel := context.WithCancel(sess.Ctx)
	ps := &PeerSession{pc: pc, ctx: ctx, cancel: cancel, SessID: sess.ID, DC: dc}

	pc.OnTrack(func(track *pion.TrackRemote, _ *pion.RTPReceiver) {
		go readRTPLoop(ps.ctx, track, sess)
	})

	pc.OnConnectionStateChange(func(state pion.PeerConnectionState) {
		switch state {
		case pion.PeerConnectionStateFailed,
			pion.PeerConnectionStateClosed,
			pion.PeerConnectionStateDisconnected:
			ps.cancel()
		}
	})

	// Wire DC proxy — UDP Proxy Mode (AC.6-2) takes precedence over HTTP Proxy.
	switch {
	case cfg.DCUDPProxyAddr != "":
		up, err := newUDPProxy(sess.ID, cfg.DCUDPProxyAddr)
		if err != nil {
			_ = pc.Close()
			return nil, err
		}
		ps.udpProxy = up
		up.wire(ctx, dc)
		// Start App Server → UE relay goroutine when DataChannel is open.
		dc.OnOpen(func() { go up.run(ps.ctx, dc) })

	case cfg.DCProxyURL != "":
		proxy := newDCProxy(sess.ID, cfg.DCProxyURL)
		proxy.wire(ctx, dc)
	}

	return ps, nil
}

// Close cancels the session-scoped context and closes the PeerConnection.
func (ps *PeerSession) Close() {
	ps.cancel()
	_ = ps.pc.Close()
}
