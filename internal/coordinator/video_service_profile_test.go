package coordinator

import (
	"testing"
	"time"

	"media-ai-gateway/internal/ai"
	"media-ai-gateway/internal/session"
)

// Thông số AI theo dịch vụ (video.services.<tên>.segmentation/gesture): mỗi dịch vụ mở stream với
// thông số của chính nó, không dùng bộ chung Video*/Gesture* (chỉ còn cho config cũ, AI Engine).
func TestStartVideoUsesServiceProfile(t *testing.T) {
	requireFFmpegForVideoE2E(t)
	coord, _, _ := newVideoStack(t)
	coord.cfg.GestureEnabled = true
	// Bộ chung khác hẳn bảng theo dịch vụ để phát hiện nếu coordinator còn dùng nó.
	coord.cfg.VideoInferWidth, coord.cfg.VideoInferHeight, coord.cfg.VideoInferFPS = 256, 256, 5
	coord.cfg.GestureFPS, coord.cfg.GestureMaxDimension = 15, 64
	coord.cfg.GestureMaxHands, coord.cfg.GestureQueueSize = 4, 8
	coord.cfg.VideoServiceProfiles = map[string]VideoServiceProfile{
		"change_background": {InferWidth: 48, InferHeight: 32, InferFPS: 3,
			GestureFPS: 15, GestureMaxDimension: 64, GestureMaxHands: 4, GestureQueueSize: 8},
		"video_effects": {InferWidth: 256, InferHeight: 256, InferFPS: 5,
			GestureFPS: 7, GestureMaxDimension: 32, GestureMaxHands: 2, GestureQueueSize: 3},
	}
	dialer := &gestureOnlyDialer{configs: make(chan ai.VideoStreamConfig, 4)}
	coord.videoAI = ai.NewVideoManager(ai.Config{MaxActiveStreams: 4, StreamTimeout: 30 * time.Second}, dialer)
	mgr := newVideoMgr()
	start := func(cfg session.SessionConfig) *session.Session {
		t.Helper()
		sess, err := mgr.Create(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { mgr.Close(sess.ID) })
		if _, err := coord.Start(sess); err != nil {
			t.Fatal(err)
		}
		if err := coord.StartVideo(sess); err != nil {
			t.Fatal(err)
		}
		return sess
	}

	// Session.Task giữ chữ hoa/thường của selectedService; bảng tra theo chữ thường.
	start(session.SessionConfig{ID: "bg-profile-tcore", Task: "Change_Background", VideoAITask: "video_segment",
		EffectType: "bg_replace", VideoEnabled: true})
	if cfg := <-dialer.configs; cfg.AIService != "" || cfg.InferFPS != 3 || cfg.InferWidth != 48 || cfg.InferHeight != 32 {
		t.Fatalf("change_background must use its segmentation profile (3 fps, 48x32): %+v", cfg)
	}

	gesture := start(session.SessionConfig{ID: "fx-profile-tcore", Task: "video_effects", VideoAITask: "gesture_recognition", VideoEnabled: true})
	if cfg := <-dialer.configs; cfg.AIService != "gesture_recognition" || cfg.InferFPS != 7 || cfg.MaxHands != 2 {
		t.Fatalf("video_effects must use its gesture profile (7 fps, 2 hands): %+v", cfg)
	}
	for _, packet := range genShortH264Packets(t, gesture.ID) { // nguồn 64x64
		gesture.VideoPacketQueue <- packet
	}
	select {
	case frame := <-dialer.client.frames:
		if frame.Width != 32 || frame.Height != 32 {
			t.Fatalf("gesture frame %dx%d, want 32x32 from the service max_dimension", frame.Width, frame.Height)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("video_effects did not produce a gesture frame")
	}
}
