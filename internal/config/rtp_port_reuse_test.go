package config

import (
	"strings"
	"testing"
	"time"
)

func TestRTPPortReuseDelay(t *testing.T) {
	cfg, err := Load(writeTemp(t, "rtp:\n  port_start: 40000\n  port_end: 40099\n"))
	if err != nil || cfg.ToControlPlaneServerConfig().RTPPortReuseDelay != 5*time.Second {
		t.Fatalf("default: %v, %v", cfg.ToControlPlaneServerConfig().RTPPortReuseDelay, err)
	}
	cfg, err = Load(writeTemp(t, "rtp:\n  port_start: 40000\n  port_end: 40099\n  port_reuse_delay_ms: 0\n"))
	if err != nil || cfg.ToControlPlaneServerConfig().RTPPortReuseDelay != 0 {
		t.Fatalf("0 = off: %v", err)
	}
	if _, err := Load(writeTemp(t, "rtp:\n  port_reuse_delay_ms: -1\n")); err == nil || !strings.Contains(err.Error(), "rtp.port_reuse_delay_ms must be >= 0") {
		t.Fatalf("negative: want error, got %v", err)
	}
}
