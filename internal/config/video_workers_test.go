package config

import (
	"strings"
	"testing"
	"time"

	"media-ai-gateway/internal/ai"
)

func TestVideoEngineConfigurationSelectsSourceMode(t *testing.T) {
	cfg, err := Load(writeTemp(t, `video:
  enabled: true
  gesture:
    enabled: true
  ai:
    engine_target: engine:50054
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ToVideoWorkers()) != 0 || !cfg.ToCoordinatorConfig().VideoEngine {
		t.Fatal("engine mode still routes per-task workers")
	}
	a := cfg.ToVideoAIConfig()
	if a.StreamTimeout != 0 || a.SendTimeout != 5*time.Second || a.RecvIdleTimeout != 0 {
		t.Fatalf("source deadlines/lifetime: %+v", a)
	}
	_, err = Load(writeTemp(t, `video:
  enabled: true
  ai:
    engine_target: engine:50054
    grpc_target: worker:50052
`))
	if err == nil {
		t.Fatal("ambiguous engine/direct worker configuration accepted")
	}
}

func TestVideoWorkersLoadAndRegisterSeparateTasks(t *testing.T) {
	cfg, err := Load(writeTemp(t, `
video:
  enabled: true
  gesture:
    enabled: true
  ai:
    grpc_target: ignored-shared:50051
    max_active_streams: 12
    workers:
      - id: segmentation-external
        grpc_target: segmentation:50051
        tasks: [video_segment]
        max_streams: 3
      - id: gesture-mock
        grpc_target: gesture:50052
        tasks: [gesture_recognition]
`))
	if err != nil {
		t.Fatal(err)
	}
	workers := cfg.ToVideoWorkers()
	if len(workers) != 2 || workers[0].Addr != "segmentation:50051" || workers[1].Addr != "gesture:50052" {
		t.Fatalf("explicit workers not used: %+v", workers)
	}
	if workers[0].MaxStreams != 3 || workers[1].MaxStreams != 12 {
		t.Fatal("per-worker/default capacity not propagated")
	}
	registry := ai.NewWorkerRegistry(0)
	for _, worker := range workers {
		registry.Register(worker)
	}
	for task, expected := range map[string]string{"video_segment": "segmentation:50051", "gesture_recognition": "gesture:50052"} {
		worker, err := registry.SelectVideoTask(task, "jpeg", "video_segment")
		if err != nil || worker.Addr != expected {
			t.Fatalf("wrong route for %s: %+v, %v", task, worker, err)
		}
		registry.ReleaseVideoTask(worker.ID)
	}
}

func TestVideoWorkersLegacySharedEndpoint(t *testing.T) {
	for _, gesture := range []bool{false, true} {
		cfg := Default()
		cfg.Video.AI.GRPCTarget = "shared:50051"
		cfg.Video.Gesture.Enabled = gesture
		workers := cfg.ToVideoWorkers()
		if len(workers) != 1 || workers[0].ID != "config-video:shared:50051" || len(workers[0].Tasks) != 1+boolInt(gesture) {
			t.Fatalf("legacy config changed: %+v", workers)
		}
	}
	if workers := Default().ToVideoWorkers(); len(workers) != 0 {
		t.Fatal("empty video endpoint should retain null mode")
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestVideoWorkerValidation(t *testing.T) {
	for _, tc := range []struct {
		name, workers, want string
	}{
		{"missing id", "[{grpc_target: 'seg:50051', tasks: [video_segment]}]", "requires id"},
		{"missing target", "[{id: seg, tasks: [video_segment]}]", "requires id"},
		{"missing tasks", "[{id: seg, grpc_target: 'seg:50051'}]", "requires id"},
		{"wildcard", "[{id: seg, grpc_target: 'seg:50051', tasks: ['*']}]", "unsupported task"},
		{"audio task", "[{id: seg, grpc_target: 'seg:50051', tasks: [speech_to_text]}]", "unsupported task"},
		{"negative capacity", "[{id: seg, grpc_target: 'seg:50051', tasks: [video_segment], max_streams: -1}]", "nonnegative"},
		{"duplicate task", "[{id: seg, grpc_target: 'seg:50051', tasks: [video_segment, video_segment]}]", "duplicate task"},
		{"duplicate id", "[{id: seg, grpc_target: 'seg:50051', tasks: [video_segment]}, {id: seg, grpc_target: 'other:50051', tasks: [gesture_recognition]}]", "duplicate id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTemp(t, "video:\n  ai:\n    workers: "+tc.workers+"\n"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func TestGestureRequiresMatchingWorker(t *testing.T) {
	_, err := Load(writeTemp(t, `
video:
  enabled: true
  gesture:
    enabled: true
  ai:
    grpc_target: ignored-shared:50051
    workers:
      - id: segmentation-only
        grpc_target: seg:50051
        tasks: [video_segment]
`))
	if err == nil || !strings.Contains(err.Error(), "gesture worker") {
		t.Fatalf("gesture silently fell back to legacy endpoint: %v", err)
	}
	// A gesture-only deployment does not require a segmentation endpoint.
	_, err = Load(writeTemp(t, `
video:
  enabled: true
  gesture:
    enabled: true
  ai:
    workers:
      - id: gesture-only
        grpc_target: gesture:50052
        tasks: [gesture_recognition]
`))
	if err != nil {
		t.Fatal(err)
	}
}
