package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ── Default ───────────────────────────────────────────────────────────────────

func TestDefault_HTTPAddr(t *testing.T) {
	cfg := Default()
	if cfg.Server.HTTPAddr != ":8080" {
		t.Errorf("Server.HTTPAddr = %q, want :8080", cfg.Server.HTTPAddr)
	}
}

func TestDefault_MaxSessions(t *testing.T) {
	cfg := Default()
	if cfg.Session.MaxSessions != 10000 {
		t.Errorf("Session.MaxSessions = %d, want 10000", cfg.Session.MaxSessions)
	}
}

func TestDefault_RawRTPEnabled(t *testing.T) {
	cfg := Default()
	if !cfg.Gateway.RawRTPEnabled {
		t.Error("Gateway.RawRTPEnabled should default to true")
	}
}

func TestDefault_AIMaxActiveStreams(t *testing.T) {
	cfg := Default()
	if cfg.AI.MaxActiveStreams != 1000 {
		t.Errorf("AI.MaxActiveStreams = %d, want 1000", cfg.AI.MaxActiveStreams)
	}
}

func TestDefault_LogLevel(t *testing.T) {
	cfg := Default()
	if cfg.Log.Level != "info" {
		t.Errorf("Log.Level = %q, want info", cfg.Log.Level)
	}
}

func TestDefault_ShutdownTimeoutSec(t *testing.T) {
	cfg := Default()
	if cfg.Server.ShutdownTimeoutSec != 10 {
		t.Errorf("Server.ShutdownTimeoutSec = %d, want 10", cfg.Server.ShutdownTimeoutSec)
	}
}

// ── Load ─────────────────────────────────────────────────────────────────────

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return f.Name()
}

func TestLoad_MissingFile_ReturnsError(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nonexistent.yaml"))
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestLoad_ValidYAML_OverridesFields(t *testing.T) {
	yaml := `
gateway:
  name: "test-gateway"
  id: "node-1"
server:
  http_addr: ":9000"
  shutdown_timeout_sec: 30
session:
  max_sessions: 500
ai:
  grpc_target: "ai-worker:50051"
  max_active_streams: 200
`
	path := writeTemp(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Gateway.Name != "test-gateway" {
		t.Errorf("Gateway.Name = %q, want test-gateway", cfg.Gateway.Name)
	}
	if cfg.Gateway.ID != "node-1" {
		t.Errorf("Gateway.ID = %q, want node-1", cfg.Gateway.ID)
	}
	if cfg.Server.HTTPAddr != ":9000" {
		t.Errorf("Server.HTTPAddr = %q, want :9000", cfg.Server.HTTPAddr)
	}
	if cfg.Server.ShutdownTimeoutSec != 30 {
		t.Errorf("Server.ShutdownTimeoutSec = %d, want 30", cfg.Server.ShutdownTimeoutSec)
	}
	if cfg.Session.MaxSessions != 500 {
		t.Errorf("Session.MaxSessions = %d, want 500", cfg.Session.MaxSessions)
	}
	if cfg.AI.GRPCTarget != "ai-worker:50051" {
		t.Errorf("AI.GRPCTarget = %q, want ai-worker:50051", cfg.AI.GRPCTarget)
	}
	if cfg.AI.MaxActiveStreams != 200 {
		t.Errorf("AI.MaxActiveStreams = %d, want 200", cfg.AI.MaxActiveStreams)
	}
}

func TestLoad_PartialYAML_PreservesDefaults(t *testing.T) {
	// Only override one field — all others should stay at Default() values.
	yaml := `
server:
  http_addr: ":7070"
`
	path := writeTemp(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Overridden field.
	if cfg.Server.HTTPAddr != ":7070" {
		t.Errorf("Server.HTTPAddr = %q, want :7070", cfg.Server.HTTPAddr)
	}
	// Fields not in YAML keep Default values.
	if cfg.Session.MaxSessions != 10000 {
		t.Errorf("Session.MaxSessions = %d, want default 10000", cfg.Session.MaxSessions)
	}
	if cfg.AI.MaxActiveStreams != 1000 {
		t.Errorf("AI.MaxActiveStreams = %d, want default 1000", cfg.AI.MaxActiveStreams)
	}
	if cfg.Pipeline.AudioWorkerCount != 16 {
		t.Errorf("Pipeline.AudioWorkerCount = %d, want default 16", cfg.Pipeline.AudioWorkerCount)
	}
}

func TestLoad_BoolFalse_OverridesTrue(t *testing.T) {
	// raw_rtp_enabled defaults to true; YAML sets false.
	yaml := `
gateway:
  raw_rtp_enabled: false
`
	path := writeTemp(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Gateway.RawRTPEnabled {
		t.Error("Gateway.RawRTPEnabled should be false after YAML override")
	}
	// WebRTCEnabled not in YAML → keeps default true.
	if !cfg.Gateway.WebRTCEnabled {
		t.Error("Gateway.WebRTCEnabled should remain default true")
	}
}

func TestLoad_TURNServers(t *testing.T) {
	yaml := `
webrtc:
  stun_servers:
    - "stun:custom.example.com:3478"
  turn_servers:
    - urls: ["turn:turn.example.com:3478"]
      username: "user"
      credential: "pass"
`
	path := writeTemp(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.WebRTC.STUNServers) != 1 || cfg.WebRTC.STUNServers[0] != "stun:custom.example.com:3478" {
		t.Errorf("STUNServers = %v, want [stun:custom.example.com:3478]", cfg.WebRTC.STUNServers)
	}
	if len(cfg.WebRTC.TURNServers) != 1 {
		t.Fatalf("TURNServers len = %d, want 1", len(cfg.WebRTC.TURNServers))
	}
	if cfg.WebRTC.TURNServers[0].Username != "user" {
		t.Errorf("TURNServers[0].Username = %q, want user", cfg.WebRTC.TURNServers[0].Username)
	}
}

func TestLoad_InvalidYAML_ReturnsError(t *testing.T) {
	path := writeTemp(t, "gateway: {name: [bad yaml")
	_, err := Load(path)
	if err == nil {
		t.Error("expected error for invalid YAML")
	}
}

// ── Conversion helpers ────────────────────────────────────────────────────────

func TestToSessionManagerConfig(t *testing.T) {
	cfg := Default()
	cfg.Session.MaxSessions = 500
	cfg.Session.IdleTimeoutSec = 60
	cfg.Session.PerSessionPacketQueue = 256

	sc := cfg.ToSessionManagerConfig()
	if sc.MaxSessions != 500 {
		t.Errorf("MaxSessions = %d, want 500", sc.MaxSessions)
	}
	if sc.IdleTimeout != 60*time.Second {
		t.Errorf("IdleTimeout = %v, want 60s", sc.IdleTimeout)
	}
	if sc.PacketQueueSize != 256 {
		t.Errorf("PacketQueueSize = %d, want 256", sc.PacketQueueSize)
	}
}

func TestToWorkerPoolConfig(t *testing.T) {
	cfg := Default()
	cfg.Pipeline.AudioWorkerCount = 8
	cfg.Pipeline.AudioJobQueueSize = 4096

	pc := cfg.ToWorkerPoolConfig()
	if pc.Workers != 8 {
		t.Errorf("Workers = %d, want 8", pc.Workers)
	}
	if pc.QueueSize != 4096 {
		t.Errorf("QueueSize = %d, want 4096", pc.QueueSize)
	}
}

func TestToAIConfig(t *testing.T) {
	cfg := Default()
	cfg.AI.MaxActiveStreams = 200
	cfg.AI.SendTimeoutMs = 300
	cfg.AI.StreamTimeoutSec = 120
	cfg.AI.MaxRetry = 5

	ac := cfg.ToAIConfig()
	if ac.MaxActiveStreams != 200 {
		t.Errorf("MaxActiveStreams = %d, want 200", ac.MaxActiveStreams)
	}
	if ac.SendTimeout != 300*time.Millisecond {
		t.Errorf("SendTimeout = %v, want 300ms", ac.SendTimeout)
	}
	if ac.StreamTimeout != 120*time.Second {
		t.Errorf("StreamTimeout = %v, want 120s", ac.StreamTimeout)
	}
	if ac.MaxRetries != 5 {
		t.Errorf("MaxRetries = %d, want 5", ac.MaxRetries)
	}
}

func TestToDispatcherConfig(t *testing.T) {
	cfg := Default()
	cfg.Result.DispatcherWorkers = 8
	cfg.Result.QueueSize = 2048
	cfg.Result.DropPartialWhenFull = false
	cfg.Result.SendTimeoutMs = 500

	dc := cfg.ToDispatcherConfig()
	if dc.Workers != 8 {
		t.Errorf("Workers = %d, want 8", dc.Workers)
	}
	if dc.QueueSize != 2048 {
		t.Errorf("QueueSize = %d, want 2048", dc.QueueSize)
	}
	if dc.DropPartialWhenFull {
		t.Error("DropPartialWhenFull should be false")
	}
	if dc.SendTimeout != 500*time.Millisecond {
		t.Errorf("SendTimeout = %v, want 500ms", dc.SendTimeout)
	}
}

func TestToCoordinatorConfig(t *testing.T) {
	cfg := Default()
	cfg.Audio.ChunkMs = 250
	cfg.Audio.OutputSampleRate = 8000
	cfg.Callback.TimeoutMs = 2000
	cfg.Callback.MaxRetry = 5

	cc := cfg.ToCoordinatorConfig()
	if cc.ChunkMs != 250 {
		t.Errorf("ChunkMs = %d, want 250", cc.ChunkMs)
	}
	if cc.OutSampleRate != 8000 {
		t.Errorf("OutSampleRate = %d, want 8000", cc.OutSampleRate)
	}
	if cc.CallbackTimeout != 2*time.Second {
		t.Errorf("CallbackTimeout = %v, want 2s", cc.CallbackTimeout)
	}
	if cc.CallbackRetry != 5 {
		t.Errorf("CallbackRetry = %d, want 5", cc.CallbackRetry)
	}
}

func TestToControlPlaneServerConfig(t *testing.T) {
	cfg := Default()
	cfg.Server.HTTPAddr = ":9000"
	cfg.Gateway.ID = "gw-1"
	cfg.RTP.PublicIP = "1.2.3.4"
	cfg.RTP.PortStart = 40000
	cfg.RTP.PortEnd = 40099

	sc := cfg.ToControlPlaneServerConfig()
	if sc.Addr != ":9000" {
		t.Errorf("Addr = %q, want :9000", sc.Addr)
	}
	if sc.GatewayID != "gw-1" {
		t.Errorf("GatewayID = %q, want gw-1", sc.GatewayID)
	}
	if sc.RTPPublicIP != "1.2.3.4" {
		t.Errorf("RTPPublicIP = %q, want 1.2.3.4", sc.RTPPublicIP)
	}
	if sc.RTPPortStart != 40000 || sc.RTPPortEnd != 40099 {
		t.Errorf("RTPPort range = %d-%d, want 40000-40099", sc.RTPPortStart, sc.RTPPortEnd)
	}
}

func TestToRTPIngressConfig(t *testing.T) {
	cfg := Default()
	cfg.RTP.ListenAddr = ":5100"
	cfg.RTP.SocketReadBuffer = 2 * 1024 * 1024

	rc := cfg.ToRTPIngressConfig()
	if rc.Addr != ":5100" {
		t.Errorf("Addr = %q, want :5100", rc.Addr)
	}
	if rc.ReadBufferBytes != 2*1024*1024 {
		t.Errorf("ReadBufferBytes = %d, want 2MiB", rc.ReadBufferBytes)
	}
}

func TestToWebRTCConfig_STUNOnly(t *testing.T) {
	cfg := Default()
	// Default has one STUN server.
	wc := cfg.ToWebRTCConfig()
	if len(wc.ICEServers) != 1 {
		t.Fatalf("ICEServers len = %d, want 1", len(wc.ICEServers))
	}
	if wc.ICEServers[0].URLs[0] != "stun:stun.l.google.com:19302" {
		t.Errorf("ICEServers[0].URLs[0] = %q", wc.ICEServers[0].URLs[0])
	}
}

func TestToWebRTCConfig_WithTURN(t *testing.T) {
	cfg := Default()
	cfg.WebRTC.TURNServers = []TURNServer{
		{URLs: []string{"turn:turn.example.com:3478"}, Username: "u", Credential: "p"},
	}

	wc := cfg.ToWebRTCConfig()
	// 1 STUN (from default) + 1 TURN
	if len(wc.ICEServers) != 2 {
		t.Fatalf("ICEServers len = %d, want 2 (1 STUN + 1 TURN)", len(wc.ICEServers))
	}
	turn := wc.ICEServers[1]
	if turn.Username != "u" || turn.Credential != "p" {
		t.Errorf("TURN creds: got user=%q cred=%q", turn.Username, turn.Credential)
	}
}

// The binary must pass bounded video queues through to SessionManager. Leaving
// these at zero silently dropped packets/JPEGs with non-blocking sends.
func TestVideoSessionQueueConfig(t *testing.T) {
	cfg := Default()
	cfg.Session.PerSessionPacketQueue = 512
	cfg.Video.FrameQueueSize = 7
	got := cfg.ToSessionManagerConfig()
	if got.VideoPacketQueueSize != 512 || got.VideoFrameQueueSize != 7 {
		t.Fatalf("video queues not propagated: %+v", got)
	}
}
