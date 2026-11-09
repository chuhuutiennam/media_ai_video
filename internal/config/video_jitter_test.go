package config

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"media-ai-gateway/internal/jitter"
)

const videoJitterBaseYAML = "video:\n  enabled: true\n  ai:\n    grpc_target: 'seg:50052'\n"

func TestVideoJitter_Defaults(t *testing.T) {
	cfg := Default()
	want := VideoJitterSection{Enabled: true, BufferMs: 80, MaxLateMs: 200, PacketTimeMs: 20,
		PacketQueueSize: 1024, OutQueueSize: 1024, MaxPackets: 1024}
	if cfg.Video.Jitter != want {
		t.Fatalf("defaults = %+v, want %+v", cfg.Video.Jitter, want)
	}
	cc := cfg.ToCoordinatorConfig()
	wantVideo := jitter.Config{BufferMs: 80, MaxLateMs: 200, PacketTimeMs: 20, SkipGaps: true, MaxPackets: 1024}
	if cc.VideoJitterConfig != wantVideo || cc.VideoJitterOutSize != 1024 {
		t.Fatalf("video jitter = %+v out=%d, want %+v out=1024", cc.VideoJitterConfig, cc.VideoJitterOutSize, wantVideo)
	}
	// Audio không đổi: không SkipGaps, không giới hạn.
	if cc.JitterConfig.SkipGaps || cc.JitterConfig.MaxPackets != 0 || cc.JitterOutSize != cfg.Pipeline.JitterOutQueueSize {
		t.Fatalf("audio jitter changed: %+v out=%d", cc.JitterConfig, cc.JitterOutSize)
	}
}

func TestVideoJitter_OverrideAndDisable(t *testing.T) {
	cfg, err := Load(writeTemp(t, videoJitterBaseYAML+"  jitter:\n    buffer_ms: 120\n    packet_time_ms: 10\n    out_queue_size: 512\n    packet_queue_size: 256\n    max_packets: 300\n"))
	if err != nil {
		t.Fatal(err)
	}
	cc := cfg.ToCoordinatorConfig()
	if cc.VideoJitterConfig.BufferMs != 120 || cc.VideoJitterConfig.PacketTimeMs != 10 || cc.VideoJitterConfig.MaxLateMs != 200 ||
		cc.VideoJitterConfig.MaxPackets != 300 || !cc.VideoJitterConfig.SkipGaps || cc.VideoJitterOutSize != 512 ||
		cfg.ToSessionManagerConfig().VideoPacketQueueSize != 256 {
		t.Fatalf("override not applied: %+v out=%d", cc.VideoJitterConfig, cc.VideoJitterOutSize)
	}

	legacy := "session:\n  per_session_packet_queue: 300\npipeline:\n  jitter_buffer_ms: 70\n  max_packet_late_ms: 150\n  packet_time_ms: 20\n  jitter_out_queue_size: 64\n" +
		videoJitterBaseYAML + "  jitter:\n    enabled: false\n    buffer_ms: 0\n"
	cfg, err = Load(writeTemp(t, legacy))
	if err != nil {
		t.Fatalf("disabled video.jitter must skip validation: %v", err)
	}
	cc = cfg.ToCoordinatorConfig()
	if cc.VideoJitterConfig != cc.JitterConfig || cc.VideoJitterOutSize != 64 || cfg.ToSessionManagerConfig().VideoPacketQueueSize != 300 {
		t.Fatalf("disabled: video must reuse audio jitter: video=%+v audio=%+v out=%d", cc.VideoJitterConfig, cc.JitterConfig, cc.VideoJitterOutSize)
	}
}

func TestVideoJitter_Validation(t *testing.T) {
	for _, tc := range []struct{ field, value string }{
		{"buffer_ms", "5"}, {"buffer_ms", "1001"},
		{"max_late_ms", "10"},
		{"packet_time_ms", "0"}, {"packet_time_ms", "101"},
		{"packet_queue_size", "8"}, {"out_queue_size", "70000"},
		{"max_packets", "32"},
	} {
		_, err := Load(writeTemp(t, videoJitterBaseYAML+"  jitter:\n    "+tc.field+": "+tc.value+"\n"))
		if err == nil || !strings.Contains(err.Error(), "video.jitter."+tc.field) {
			t.Errorf("%s=%s: want video.jitter.%s error, got %v", tc.field, tc.value, tc.field, err)
		}
	}
	_, err := Load(writeTemp(t, videoJitterBaseYAML+"  jitter:\n    buffer_ms: 30\n    packet_time_ms: 40\n"))
	if err == nil || !strings.Contains(err.Error(), "packet_time_ms (40) must be <= video.jitter.buffer_ms (30)") {
		t.Errorf("packet_time_ms > buffer_ms: got %v", err)
	}
	// Video tắt: không kiểm tra video.jitter.
	if _, err := Load(writeTemp(t, "video:\n  enabled: false\n  jitter:\n    buffer_ms: 1\n")); err != nil {
		t.Errorf("video disabled: %v", err)
	}
}

// Các config video mẫu trong repo nạp được và bật jitter riêng cho video.
func TestVideoJitter_RepoConfigs(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "config")
	for _, name := range []string{"gateway-video-ims.yaml", "gateway-video-services.yaml", "gateway-video-workers.yaml",
		"gateway-video-engine.yaml", "gateway-video-external.yaml", "gateway-mock-video.yaml"} {
		cfg, err := Load(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !cfg.Video.Jitter.Enabled || !cfg.ToCoordinatorConfig().VideoJitterConfig.SkipGaps {
			t.Errorf("%s: video.jitter must be enabled", name)
		}
	}
}
