package config

import (
	"strings"
	"testing"

	"media-ai-gateway/internal/controlplane"
)

const videoServicesYAML = `
video:
  enabled: true
  ai:
    max_active_streams: 12
    open_timeout_ms: 4000   # blur_background legs: both → 2 × 4000ms < server.write_timeout_sec 10s
  services:
    Change_Background:
      legs: tcore
      effect_type: bg_replace
      ai:
        task: video_segment
        targets: [' seg-a:50052 ', 'seg-b:50052']
        max_streams: 4
    video_effects:
      ai:
        task: Gesture_Recognition
        targets: ['gest:50053']
    blur_background:
      legs: Both
      ai: {task: video_segment, targets: ['blur:50052']}
    off_service:
      enabled: false
      ai: {task: video_segment, targets: ['off:50052']}
`

func TestVideoServices_LoadCatalog(t *testing.T) {
	cfg, err := Load(writeTemp(t, videoServicesYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := map[string]controlplane.VideoService{
		"change_background": {Enabled: true, SingleLeg: true, EffectType: "bg_replace", AITask: "video_segment"},
		"video_effects":     {Enabled: true, SingleLeg: true, AITask: "gesture_recognition"}, // legs mặc định tcore
		"blur_background":   {Enabled: true, EffectType: "bg_blur", AITask: "video_segment"}, // legs both, effect mặc định code
		"off_service":       {Enabled: false, SingleLeg: true, EffectType: "bg_blur", AITask: "video_segment"},
	}
	got := cfg.ToVideoServices()
	if len(got) != len(want) {
		t.Fatalf("services: got %+v", got)
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s: got %+v, want %+v", name, got[name], w)
		}
	}
	if sc := cfg.ToControlPlaneServerConfig(); len(sc.VideoServices) != len(want) || sc.VideoServices["video_effects"] != want["video_effects"] {
		t.Errorf("control plane catalog not propagated: %+v", sc.VideoServices)
	}
	if !cfg.ToCoordinatorConfig().GestureEnabled {
		t.Error("coordinator gesture must be enabled when a service uses gesture_recognition")
	}

	// Worker theo dịch vụ: mỗi target một worker gắn đúng dịch vụ; dịch vụ tắt không đăng ký.
	type wk struct {
		addr, task, service string
		max                 int
	}
	var workers []wk
	for _, w := range cfg.ToVideoWorkers() {
		if len(w.Services) != 1 || len(w.Tasks) != 1 {
			t.Fatalf("worker must serve exactly one service and task: %+v", w)
		}
		workers = append(workers, wk{w.Addr, w.Tasks[0], w.Services[0], w.MaxStreams})
	}
	wantWorkers := []wk{
		{"blur:50052", "video_segment", "blur_background", 12},
		{"seg-a:50052", "video_segment", "change_background", 4},
		{"seg-b:50052", "video_segment", "change_background", 4},
		{"gest:50053", "gesture_recognition", "video_effects", 12},
	}
	if len(workers) != len(wantWorkers) {
		t.Fatalf("workers: got %+v", workers)
	}
	for i := range wantWorkers {
		if workers[i] != wantWorkers[i] {
			t.Errorf("worker %d: got %+v, want %+v", i, workers[i], wantWorkers[i])
		}
	}
}

func TestVideoServices_Validation(t *testing.T) {
	svc := func(body string) string {
		return "video:\n  enabled: true\n  services:\n" + body
	}
	ok := "    change_background:\n      ai: {task: video_segment, targets: ['a:1']}\n"
	for _, tc := range []struct {
		name, yaml, want string
	}{
		{"video disabled", "video:\n  services:\n" + ok, "requires video.enabled"},
		{"with workers", svc(ok) + "  ai:\n    workers: [{id: w, grpc_target: 'w:1', tasks: [video_segment]}]\n", "cannot be combined"},
		{"with grpc_target", svc(ok) + "  ai:\n    grpc_target: 'w:1'\n", "cannot be combined"},
		{"with engine", svc(ok) + "  ai:\n    engine_target: 'e:1'\n", "cannot be combined"},
		{"with gesture.enabled", svc(ok) + "  gesture:\n    enabled: true\n", "move video.gesture to video.services.<name>.gesture"},
		{"bad name", svc("    bad-name:\n      ai: {task: video_segment, targets: ['a:1']}\n"), "invalid service name"},
		{"audio name", svc("    speech_to_text:\n      ai: {task: video_segment, targets: ['a:1']}\n"), "audio service"},
		{"duplicate name", svc(ok + "    CHANGE_BACKGROUND:\n      ai: {task: video_segment, targets: ['b:1']}\n"), "duplicate service"},
		{"bad legs", svc("    s1:\n      legs: taccess\n      ai: {task: video_segment, targets: ['a:1']}\n"), "legs must be"},
		{"bad task", svc("    s1:\n      ai: {task: speech_to_text, targets: ['a:1']}\n"), "ai.task must be"},
		{"effect on gesture", svc("    s1:\n      effect_type: bg_blur\n      ai: {task: gesture_recognition, targets: ['a:1']}\n"), "effect_type is only valid"},
		{"no targets", svc("    s1:\n      ai: {task: video_segment}\n"), "requires at least one"},
		{"duplicate target", svc("    s1:\n      ai: {task: video_segment, targets: ['a:1', ' a:1']}\n"), "duplicate target"},
		{"negative max_streams", svc("    s1:\n      ai: {task: video_segment, targets: ['a:1'], max_streams: -1}\n"), "max_streams must be"},
		// Thông số AI theo dịch vụ (quyết định 17).
		{"gesture limits", svc("    g1:\n      gesture: {max_hands: 9}\n      ai: {task: gesture_recognition, targets: ['a:1']}\n"),
			"video.services.g1.gesture.max_hands must be 1..8, got 9"},
		{"negative gesture queue", svc("    g1:\n      gesture: {frame_queue_size: -1}\n      ai: {task: gesture_recognition, targets: ['a:1']}\n"),
			"video.services.g1.gesture.frame_queue_size must be 1..32, got -1"},
		{"gesture dimension", svc("    g1:\n      gesture: {max_dimension: 2000}\n      ai: {task: gesture_recognition, targets: ['a:1']}\n"),
			"video.services.g1.gesture.max_dimension must be 32..1920, got 2000"},
		{"segmentation fps", svc("    s1:\n      segmentation: {infer_fps: 61}\n      ai: {task: video_segment, targets: ['a:1']}\n"),
			"video.services.s1.segmentation.infer_fps must be 1..60, got 61"},
		{"segmentation width", svc("    s1:\n      segmentation: {infer_width: 8}\n      ai: {task: video_segment, targets: ['a:1']}\n"),
			"video.services.s1.segmentation.infer_width must be 16..1920, got 8"},
		{"segmentation on gesture", svc("    g1:\n      segmentation: {infer_fps: 5}\n      ai: {task: gesture_recognition, targets: ['a:1']}\n"),
			"video.services.g1.segmentation is only valid with ai.task video_segment"},
		{"gesture on segmentation", svc("    s1:\n      gesture: {max_hands: 2}\n      ai: {task: video_segment, targets: ['a:1']}\n"),
			"video.services.s1.gesture is only valid with ai.task gesture_recognition"},
		// Khoá chung cũ cùng video.services: báo chỗ chuyển thay vì bỏ qua im lặng.
		{"shared gesture", svc(ok) + "  gesture:\n    max_hands: 2\n", "move video.gesture to video.services.<name>.gesture"},
		{"shared infer_fps", svc(ok) + "  infer_fps: 5\n", "move video.infer_fps to video.services.<name>.segmentation.infer_fps"},
		{"shared infer size", svc(ok) + "  infer_width: 256\n  infer_height: 256\n",
			"move video.infer_width to video.services.<name>.segmentation.infer_width, video.infer_height to video.services.<name>.segmentation.infer_height"},
		{"shared default effect", svc(ok) + "  default_effect_type: bg_replace\n", "move video.default_effect_type to video.services.<name>.effect_type"},
	} {
		_, err := Load(writeTemp(t, tc.yaml))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want error containing %q", tc.name, err, tc.want)
		}
	}
}

// Không có video.services: danh mục mặc định theo quy tắc 1:1 — change_background chỉ
// segmentation, video_effects (gesture) chỉ có khi bật gesture; cả hai chỉ leg tcore.
func TestVideoServices_LegacyDefaults(t *testing.T) {
	cfg := Default()
	cfg.Video.Enabled = true
	got := cfg.ToVideoServices()
	if len(got) != 1 || got["change_background"] != (controlplane.VideoService{Enabled: true, SingleLeg: true, EffectType: "bg_replace", AITask: "video_segment"}) {
		t.Fatalf("gesture disabled: %+v", got)
	}

	cfg, err := Load(writeTemp(t, "video:\n  enabled: true\n  gesture:\n    enabled: true\n  ai:\n    grpc_target: 'shared:50051'\n"))
	if err != nil {
		t.Fatal(err)
	}
	got = cfg.ToVideoServices()
	if got["video_effects"] != (controlplane.VideoService{Enabled: true, SingleLeg: true, AITask: "gesture_recognition"}) || !got["change_background"].SingleLeg {
		t.Fatalf("gesture enabled: %+v", got)
	}
	for _, w := range cfg.ToVideoWorkers() {
		if len(w.Services) != 0 {
			t.Fatalf("legacy worker must not be bound to a service: %+v", w)
		}
	}
}
