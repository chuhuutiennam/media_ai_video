package config

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAudioEnabled(t *testing.T) {
	cfg, err := Load(writeTemp(t, "log:\n  level: info\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Audio.Enabled || cfg.ToControlPlaneServerConfig().AudioDisabled {
		t.Fatal("audio must stay enabled by default")
	}

	cfg, err = Load(writeTemp(t, "audio:\n  enabled: false\nai:\n  grpc_target: ''\n"))
	if err != nil || !cfg.ToControlPlaneServerConfig().AudioDisabled {
		t.Fatalf("audio.enabled=false must reach the control plane: %v", err)
	}

	// Mặc định ai.grpc_target = ai-router:50051 → mâu thuẫn khi tắt audio.
	_, err = Load(writeTemp(t, "audio:\n  enabled: false\n"))
	if err == nil || !strings.Contains(err.Error(), "ai.grpc_target (audio AI) must be empty") {
		t.Fatalf("audio disabled with audio AI target: want error, got %v", err)
	}
}

// Các config chỉ video trong repo đều tắt audio và vẫn nạp được.
func TestVideoOnlyConfigsDisableAudio(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "config")
	for _, name := range []string{"gateway-video-ims.yaml", "gateway-video-services.yaml", "gateway-video-workers.yaml",
		"gateway-video-engine.yaml", "gateway-video-external.yaml"} {
		cfg, err := Load(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if cfg.Audio.Enabled {
			t.Errorf("%s: audio.enabled must be false on a video-only gateway", name)
		}
	}
	if cfg, err := Load(filepath.Join(dir, "gateway-local.yaml")); err != nil || !cfg.Audio.Enabled {
		t.Errorf("gateway-local.yaml (audio dev mode, no AI) must keep audio enabled: %v", err)
	}
}
