package config

import (
	"strings"
	"testing"
)

func TestVideoAIFailover_Defaults(t *testing.T) {
	cfg, err := Load(writeTemp(t, videoServicesYAML))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Video.AI.Failover || cfg.Video.AI.ReadinessRequiresAI {
		t.Fatalf("defaults: failover=%v readiness_requires_ai=%v, want true/false", cfg.Video.AI.Failover, cfg.Video.AI.ReadinessRequiresAI)
	}
	if cfg.ToControlPlaneServerConfig().VideoReadinessRequiresAI {
		t.Fatal("readiness check must be off by default")
	}
}

func TestVideoAIFailover_Override(t *testing.T) {
	yaml := strings.Replace(videoServicesYAML, "    max_active_streams: 12\n",
		"    max_active_streams: 12\n    failover: false\n    readiness_requires_ai: true\n", 1)
	cfg, err := Load(writeTemp(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Video.AI.Failover || !cfg.Video.AI.ReadinessRequiresAI || !cfg.ToControlPlaneServerConfig().VideoReadinessRequiresAI {
		t.Fatalf("override not applied: %+v", cfg.Video.AI)
	}
}

func TestVideoAIFailover_ReadinessRequiresRegistryTargets(t *testing.T) {
	for name, yaml := range map[string]string{
		"video disabled": "video:\n  enabled: false\n  ai:\n    readiness_requires_ai: true\n",
		"engine":         "video:\n  enabled: true\n  ai:\n    engine_target: 'engine:50051'\n    readiness_requires_ai: true\n",
		"no targets":     "video:\n  enabled: true\n  ai:\n    readiness_requires_ai: true\n",
	} {
		_, err := Load(writeTemp(t, yaml))
		if err == nil || !strings.Contains(err.Error(), "readiness_requires_ai") {
			t.Errorf("%s: want readiness_requires_ai error, got %v", name, err)
		}
	}
	legacy := "video:\n  enabled: true\n  ai:\n    grpc_target: 'seg:50052'\n    readiness_requires_ai: true\n"
	if _, err := Load(writeTemp(t, legacy)); err != nil {
		t.Errorf("legacy grpc_target goes through the registry: %v", err)
	}
}
