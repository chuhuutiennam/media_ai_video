package config

import (
	"strings"
	"testing"
	"time"
)

const fixtureOpenTimeout = "    open_timeout_ms: 4000   # blur_background legs: both → 2 × 4000ms < server.write_timeout_sec 10s\n"

func TestAnswerDeadline_DefaultsAndOverride(t *testing.T) {
	d := Default()
	if d.Server.WriteTimeoutSec != 10 || d.Video.AI.OpenTimeoutMs != 5000 || d.ToControlPlaneServerConfig().WriteTimeout != 10*time.Second {
		t.Fatalf("defaults: write=%ds open=%dms", d.Server.WriteTimeoutSec, d.Video.AI.OpenTimeoutMs)
	}
	if !strings.Contains(videoServicesYAML, fixtureOpenTimeout) {
		t.Fatal("fixture changed: update fixtureOpenTimeout")
	}
	cfg, err := Load(writeTemp(t, "server:\n  write_timeout_sec: 20\n"+videoServicesYAML))
	if err != nil || cfg.ToControlPlaneServerConfig().WriteTimeout != 20*time.Second || cfg.Video.AI.OpenTimeoutMs != 4000 {
		t.Fatalf("override: %v", err)
	}
}

func TestAnswerDeadline_Validation(t *testing.T) {
	with := func(prefix, openLine string, oneLeg bool) string {
		y := strings.Replace(videoServicesYAML, fixtureOpenTimeout, openLine, 1)
		if oneLeg {
			y = strings.Replace(y, "      legs: Both\n", "", 1)
		}
		return prefix + y
	}
	open := func(ms string) string { return "    open_timeout_ms: " + ms + "\n" }
	for name, tc := range map[string]struct {
		yaml, want string
	}{
		"write timeout 0":     {with("server:\n  write_timeout_sec: 0\n", fixtureOpenTimeout, false), "server.write_timeout_sec must be >= 1"},
		"negative open":       {with("", open("-1"), false), "video.ai.open_timeout_ms must be >= 0"},
		"2 legs exceed write": {with("", open("5000"), false), "x video legs (2) must be less than server.write_timeout_sec (10s)"},
		"1 leg exceeds write": {with("server:\n  write_timeout_sec: 4\n", open("4000"), true), "x video legs (1) must be less than server.write_timeout_sec (4s)"},
	} {
		_, err := Load(writeTemp(t, tc.yaml))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", name, tc.want, err)
		}
	}
	for name, y := range map[string]string{
		"1 leg, default 5000": with("", "", true),
		"1 leg, 0 = no limit": with("", open("0"), true),
		"2 legs, 0":           with("", open("0"), false),
	} {
		if _, err := Load(writeTemp(t, y)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
