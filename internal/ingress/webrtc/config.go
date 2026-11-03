// Package webrtc implements WebRTC ingress for the Media AI Gateway.
//
// Signaling flow (server is answerer):
//
//  1. Client creates a session via POST /v1/sessions (source_type="webrtc").
//  2. Client sends SDP offer to POST /v1/webrtc/offer.
//  3. Handler creates PeerConnection, sets remote description, generates answer.
//  4. ICE gathering completes inline; full answer SDP is returned.
//  5. Audio arrives via SRTP; OnTrack fires readRTPLoop which pushes MediaPackets
//     into session.PacketQueue for the existing audio pipeline to process.
package webrtc

// Config holds all tunable parameters for the WebRTC ingress.
type Config struct {
	// ICEServers lists STUN/TURN servers used for ICE candidate gathering.
	// If empty, no ICE servers are configured (host candidates only — LAN use).
	ICEServers []ICEServer

	// NAT1To1IPs is a list of public IPs to advertise as host candidates.
	// Required when the gateway runs behind NAT (K8s NodePort, cloud VM, etc.).
	NAT1To1IPs []string

	// ICEPortMin/ICEPortMax bound the ephemeral UDP port range for ICE.
	// 0/0 = OS default (any ephemeral port).
	ICEPortMin uint16
	ICEPortMax uint16

	// DCProxyURL enables IMS Data Channel Mode 2.1 (HTTP Proxy).
	// When non-empty, messages received from the UE on the DataChannel are
	// forwarded via HTTP POST to this URL (X-Session-ID header is set).
	// "" = disabled. Mutually exclusive with DCUDPProxyAddr.
	DCProxyURL string

	// DCUDPProxyAddr enables IMS Data Channel Mode 2.2 (UDP Proxy, AC.6-2).
	// When non-empty (format "host:port"), DataChannel messages from the UE are
	// relayed as raw UDP datagrams to this address, and UDP datagrams arriving
	// from the App Server are forwarded back to the UE via the DataChannel.
	// The gateway's local UDP port is returned in AnswerResponse.UDPProxyPort.
	// Takes precedence over DCProxyURL if both are set.
	// "" = disabled.
	DCUDPProxyAddr string
}

// ICEServer describes one STUN or TURN server.
type ICEServer struct {
	URLs       []string // e.g. ["stun:stun.example.com:3478"]
	Username   string
	Credential string
}

// DefaultConfig returns a minimal config suitable for LAN / testing.
// Production deployments should at minimum set ICEServers with a STUN server.
func DefaultConfig() Config {
	return Config{
		ICEServers: []ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}
}
