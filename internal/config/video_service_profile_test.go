package config

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"media-ai-gateway/internal/coordinator"
)

// Quyết định 17: thông số AI nằm trong dịch vụ dùng nó; trường bỏ trống = mặc định code.
func TestVideoServices_AIProfiles(t *testing.T) {
	cfg, err := Load(writeTemp(t, `
video:
  enabled: true
  services:
    change_background:
      segmentation: {infer_fps: 3, infer_width: 320}
      ai: {task: video_segment, targets: ['seg:50052']}
    blur_background:
      ai: {task: video_segment, targets: ['blur:50052']}
    video_effects:
      gesture: {infer_fps: 10, max_hands: 2}
      ai: {task: gesture_recognition, targets: ['gest:50053']}
    hand_effects:
      gesture: {}
      ai: {task: gesture_recognition, targets: ['gest:50054']}
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	coord := cfg.ToCoordinatorConfig()
	// Mặc định code: segmentation 5 fps 256x256; gesture 15 fps, 640, 4 tay, hàng đợi 2.
	seg := func(fps, w, h int) coordinator.VideoServiceProfile {
		return coordinator.VideoServiceProfile{InferFPS: fps, InferWidth: w, InferHeight: h,
			GestureFPS: 15, GestureMaxDimension: 640, GestureMaxHands: 4, GestureQueueSize: 2}
	}
	gesture := func(fps, dim, hands, queue int) coordinator.VideoServiceProfile {
		return coordinator.VideoServiceProfile{InferFPS: 5, InferWidth: 256, InferHeight: 256,
			GestureFPS: fps, GestureMaxDimension: dim, GestureMaxHands: hands, GestureQueueSize: queue}
	}
	want := map[string]coordinator.VideoServiceProfile{
		"change_background": seg(3, 320, 256),
		"blur_background":   seg(5, 256, 256),
		"video_effects":     gesture(10, 640, 2, 2),
		"hand_effects":      gesture(15, 640, 4, 2),
	}
	if len(coord.VideoServiceProfiles) != len(want) {
		t.Fatalf("profiles: got %+v", coord.VideoServiceProfiles)
	}
	for name, w := range want {
		if got := coord.VideoProfile(name); got != w {
			t.Errorf("%s: got %+v, want %+v", name, got, w)
		}
	}
	if got := coord.VideoProfile("Video_Effects"); got != want["video_effects"] {
		t.Errorf("lookup must ignore case (Session.Task keeps selectedService case): %+v", got)
	}
}

// Config cũ (không có video.services): vẫn dùng khoá chung video.infer_* và video.gesture.
func TestVideoServices_LegacySharedAIParams(t *testing.T) {
	cfg, err := Load(writeTemp(t, `
video:
  enabled: true
  infer_fps: 7
  infer_width: 128
  infer_height: 96
  gesture: {enabled: true, infer_fps: 9, max_hands: 3}
  ai:
    grpc_target: 'shared:50051'
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	coord := cfg.ToCoordinatorConfig()
	if coord.VideoServiceProfiles != nil {
		t.Fatalf("legacy config must not build per-service profiles: %+v", coord.VideoServiceProfiles)
	}
	want := coordinator.VideoServiceProfile{InferFPS: 7, InferWidth: 128, InferHeight: 96,
		GestureFPS: 9, GestureMaxDimension: 640, GestureMaxHands: 3, GestureQueueSize: 2}
	for _, name := range []string{"change_background", "video_effects"} {
		if got := coord.VideoProfile(name); got != want {
			t.Errorf("%s: got %+v, want %+v", name, got, want)
		}
	}

	_, err = Load(writeTemp(t, "video:\n  enabled: true\n  gesture: {enabled: true, max_hands: 9}\n  ai:\n    grpc_target: 'shared:50051'\n"))
	if err == nil || !strings.Contains(err.Error(), "video.gesture.max_hands must be 1..8, got 9") {
		t.Fatalf("legacy gesture limits: got %v", err)
	}
}

// Mẫu config dùng video.services khai báo thông số trong từng dịch vụ và nạp được.
func TestVideoServices_SampleConfigProfiles(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	want := map[string]coordinator.VideoServiceProfile{
		"change_background": {InferFPS: 5, InferWidth: 256, InferHeight: 256,
			GestureFPS: 15, GestureMaxDimension: 640, GestureMaxHands: 4, GestureQueueSize: 2},
		"video_effects": {InferFPS: 5, InferWidth: 256, InferHeight: 256,
			GestureFPS: 15, GestureMaxDimension: 640, GestureMaxHands: 4, GestureQueueSize: 8},
	}
	for _, name := range []string{"gateway-video-ims.yaml", "gateway-video-services.yaml"} {
		cfg, err := Load(filepath.Join(filepath.Dir(file), "..", "..", "config", name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		coord := cfg.ToCoordinatorConfig()
		for service, w := range want {
			if got := coord.VideoProfile(service); got != w {
				t.Errorf("%s %s: got %+v, want %+v", name, service, got, w)
			}
		}
	}
}
