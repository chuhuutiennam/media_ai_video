package config

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Mặc định stream AI video sống theo cuộc gọi; trước đây 300 s cắt AI sau 5 phút.
func TestVideoAILifetime_Defaults(t *testing.T) {
	cfg := Default()
	if cfg.Video.AI.StreamTimeoutSec != 0 || cfg.Video.AI.MaxRetry != 3 {
		t.Fatalf("video.ai defaults: stream_timeout_sec=%d max_retry=%d, want 0/3", cfg.Video.AI.StreamTimeoutSec, cfg.Video.AI.MaxRetry)
	}
	if ac := cfg.ToVideoAIConfig(); ac.StreamTimeout != 0 || ac.MaxRetries != 3 {
		t.Fatalf("ToVideoAIConfig: %+v", ac)
	}
	if cfg.AI.StreamTimeoutSec != 300 {
		t.Fatalf("audio ai.stream_timeout_sec must stay 300, got %d", cfg.AI.StreamTimeoutSec)
	}
}

func TestVideoAILifetime_Validation(t *testing.T) {
	for _, field := range []string{"stream_timeout_sec", "max_retry"} {
		_, err := Load(writeTemp(t, "video:\n  ai:\n    "+field+": -1\n"))
		if err == nil || !strings.Contains(err.Error(), "video.ai.stream_timeout_sec and video.ai.max_retry must be >= 0") {
			t.Errorf("%s=-1: want validation error, got %v", field, err)
		}
	}
}

// Mẫu danh mục dịch vụ mở lại stream khi worker restart.
func TestVideoAILifetime_ServicesSample(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	cfg, err := Load(filepath.Join(filepath.Dir(file), "..", "..", "config", "gateway-video-services.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Video.AI.MaxRetry != 3 || cfg.Video.AI.StreamTimeoutSec != 0 {
		t.Fatalf("gateway-video-services.yaml: max_retry=%d stream_timeout_sec=%d, want 3/0", cfg.Video.AI.MaxRetry, cfg.Video.AI.StreamTimeoutSec)
	}
}
