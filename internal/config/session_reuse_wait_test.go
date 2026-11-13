package config

import (
	"testing"
	"time"
)

func TestSessionReuseWaitDefaultOverrideAndValidation(t *testing.T) {
	if got := Default().ToCoordinatorConfig().SessionReuseWait; got != 2*time.Second {
		t.Fatalf("default SessionReuseWait = %v, want 2s", got)
	}
	cfg, err := Load(writeTemp(t, "session:\n  reuse_wait_ms: 750\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ToCoordinatorConfig().SessionReuseWait; got != 750*time.Millisecond {
		t.Fatalf("SessionReuseWait = %v, want 750ms", got)
	}
	for _, invalid := range []string{"0", "-1", "60001"} {
		if _, err := Load(writeTemp(t, "session:\n  reuse_wait_ms: "+invalid+"\n")); err == nil {
			t.Errorf("accepted session.reuse_wait_ms=%s", invalid)
		}
	}
	for _, path := range []string{"../../config/gateway.yaml", "../../config/gateway-ims.yaml", "../../config/gateway-video-ims.yaml"} {
		profile, err := Load(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if profile.Session.ReuseWaitMs != 2000 {
			t.Errorf("%s: session.reuse_wait_ms = %d, want 2000", path, profile.Session.ReuseWaitMs)
		}
	}
}
