package coordinator

import (
	"context"
	"testing"
	"time"

	"media-ai-gateway/internal/ai"
	"media-ai-gateway/internal/pipeline"
	"media-ai-gateway/internal/session"
)

type gestureOnlyDialer struct {
	configs chan ai.VideoStreamConfig
	client  *fakeVideoClient
}

func (d *gestureOnlyDialer) Dial(ctx context.Context, _, _ string, cfg ai.VideoStreamConfig) (ai.VideoStreamClient, error) {
	d.configs <- cfg
	d.client = &fakeVideoClient{ctx: ctx, frames: make(chan pipeline.VideoFrame, 32), results: make(chan pipeline.RecognitionResult, 16)}
	return d.client, nil
}

func TestStartGestureOnlyOpensOneTaskWithoutMaskFrames(t *testing.T) {
	requireFFmpegForVideoE2E(t)
	coord, _, _ := newVideoStack(t)
	coord.cfg.GestureEnabled = true
	coord.cfg.GestureFPS, coord.cfg.GestureMaxDimension = 15, 64
	coord.cfg.GestureMaxHands, coord.cfg.GestureQueueSize = 4, 8
	dialer := &gestureOnlyDialer{configs: make(chan ai.VideoStreamConfig, 4)}
	coord.videoAI = ai.NewVideoManager(ai.Config{MaxActiveStreams: 4, StreamTimeout: 30 * time.Second}, dialer)
	mgr := newVideoMgr()
	sess, err := mgr.Create(session.SessionConfig{ID: "gesture-only-tcore", Task: "video_effects", VideoEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(sess.ID)
	if _, err := coord.Start(sess); err != nil {
		t.Fatal(err)
	}
	if err := coord.StartVideo(sess); err != nil {
		t.Fatal(err)
	}
	if len(dialer.configs) != 1 || coord.videoAI.Count() != 1 {
		t.Fatal("gesture-only opened more than one AI task")
	}
	cfg := <-dialer.configs
	if cfg.AIService != "gesture_recognition" || cfg.SelectedService != "video_effects" || cfg.EffectType != "" {
		t.Fatalf("wrong gesture-only task: %+v", cfg)
	}
	for _, packet := range genShortH264Packets(t, sess.ID) {
		sess.VideoPacketQueue <- packet
	}
	select {
	case frame := <-dialer.client.frames:
		if len(frame.JPEG) == 0 || frame.StreamID != "gesture-only:tcore:video" {
			t.Fatalf("bad gesture frame: %+v", frame)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gesture-only did not produce a JPEG frame")
	}
	if len(sess.VideoFrameQueue) != 0 || len(dialer.configs) != 0 {
		t.Fatal("gesture-only generated segmentation work")
	}
}

// 1 dịch vụ ↔ 1 AI: change_background chỉ mở segmentation dù gesture đang bật
// (trước đây mở thêm stream gesture ngay khi StartVideo).
func TestStartChangeBackgroundOpensOnlySegmentation(t *testing.T) {
	coord, _, _ := newVideoStack(t)
	coord.cfg.GestureEnabled = true
	coord.cfg.GestureFPS, coord.cfg.GestureMaxDimension = 15, 64
	coord.cfg.GestureMaxHands, coord.cfg.GestureQueueSize = 4, 8
	dialer := &gestureOnlyDialer{configs: make(chan ai.VideoStreamConfig, 4)}
	coord.videoAI = ai.NewVideoManager(ai.Config{MaxActiveStreams: 4, StreamTimeout: 30 * time.Second}, dialer)
	mgr := newVideoMgr()
	sess, err := mgr.Create(session.SessionConfig{ID: "bg-only-tcore", Task: "change_background", VideoAITask: "video_segment",
		EffectType: "bg_replace", VideoEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(sess.ID)
	if _, err := coord.Start(sess); err != nil {
		t.Fatal(err)
	}
	if err := coord.StartVideo(sess); err != nil {
		t.Fatal(err)
	}
	if len(dialer.configs) != 1 || coord.videoAI.Count() != 1 {
		t.Fatalf("change_background opened %d AI streams, want 1", coord.videoAI.Count())
	}
	cfg := <-dialer.configs
	if cfg.AIService != "" || cfg.SelectedService != "change_background" || cfg.EffectType != "bg_replace" {
		t.Fatalf("wrong segmentation task: %+v", cfg)
	}
}

func TestStartGestureOnlyRequiresFeatureEnabled(t *testing.T) {
	coord, _, _ := newVideoStack(t)
	mgr := newVideoMgr()
	sess, err := mgr.Create(session.SessionConfig{ID: "gesture-disabled", Task: "video_effects", VideoEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(sess.ID)
	if err := coord.StartVideo(sess); err == nil || coord.videoAI.Count() != 0 {
		t.Fatal("disabled gesture service was accepted")
	}
}

// 1 dịch vụ ↔ 1 AI: source của Engine chỉ chứa đúng task của dịch vụ; change_background
// không kèm gesture kể cả khi gesture được bật.
func TestStartEngineVideoOpensOneSourceWithServiceTask(t *testing.T) {
	for _, tc := range []struct {
		name, service, task string
		gestureEnabled      bool
	}{
		{"segmentation", "change_background", "video_segment", false},
		{"segmentation_gesture_enabled", "change_background", "video_segment", true},
		{"gesture", "video_effects", "gesture_recognition", true},
	} {
		mode := "segmentation"
		if tc.task == "gesture_recognition" {
			mode = "gesture"
		}
		t.Run(tc.name, func(t *testing.T) {
			coord, _, _ := newVideoStack(t)
			coord.cfg.VideoEngine, coord.cfg.VideoSourceMaxDimension = true, 64
			coord.cfg.VideoInferWidth, coord.cfg.VideoInferHeight, coord.cfg.VideoInferFPS = 32, 32, 5
			coord.cfg.GestureEnabled = tc.gestureEnabled
			coord.cfg.GestureFPS, coord.cfg.GestureMaxDimension, coord.cfg.GestureMaxHands = 15, 64, 2
			dialer := &gestureOnlyDialer{configs: make(chan ai.VideoStreamConfig, 4)}
			coord.videoAI = ai.NewVideoManager(ai.Config{MaxActiveStreams: 4}, dialer)
			mgr := newVideoMgr()
			service := tc.service
			sess, err := mgr.Create(session.SessionConfig{ID: "source-tcore", Task: service, VideoAITask: tc.task, VideoEnabled: true})
			if err != nil {
				t.Fatal(err)
			}
			defer mgr.Close(sess.ID)
			if _, err = coord.Start(sess); err != nil {
				t.Fatal(err)
			}
			if err = coord.StartVideo(sess); err != nil {
				t.Fatal(err)
			}
			if len(dialer.configs) != 1 || coord.videoAI.Count() != 1 {
				t.Fatal("engine source still opens per-task streams")
			}
			cfg := <-dialer.configs
			if cfg.Source == nil || cfg.MediaEpoch == "" || cfg.SelectedService != service {
				t.Fatalf("invalid source open: %+v", cfg)
			}
			if (cfg.Source.Segmentation != nil) != (mode == "segmentation") || (cfg.Source.Gesture != nil) != (mode == "gesture") {
				t.Fatalf("wrong task set: segmentation=%v gesture=%v", cfg.Source.Segmentation != nil, cfg.Source.Gesture != nil)
			}
			fps := 15
			if mode == "segmentation" {
				fps = 5
			}
			if cfg.Source.InputFPS != fps || cfg.Source.InputMaxDimension != 64 {
				t.Fatalf("wrong input profile: %+v", cfg.Source)
			}
		})
	}
}
