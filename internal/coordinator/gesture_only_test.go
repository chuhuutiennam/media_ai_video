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
	sess, err := mgr.Create(session.SessionConfig{ID: "gesture-only-tcore", Task: "gesture_recognition", VideoEnabled: true})
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
	if cfg.AIService != "gesture_recognition" || cfg.SelectedService != "gesture_recognition" || cfg.EffectType != "" {
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

func TestStartGestureOnlyRequiresFeatureEnabled(t *testing.T) {
	coord, _, _ := newVideoStack(t)
	mgr := newVideoMgr()
	sess, err := mgr.Create(session.SessionConfig{ID: "gesture-disabled", Task: "gesture_recognition", VideoEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(sess.ID)
	if err := coord.StartVideo(sess); err == nil || coord.videoAI.Count() != 0 {
		t.Fatal("disabled gesture service was accepted")
	}
}

func TestStartEngineVideoOpensOneSourceWithEnabledTasks(t *testing.T) {
	for _, mode := range []string{"both", "segmentation", "gesture"} {
		t.Run(mode, func(t *testing.T) {
			coord, _, _ := newVideoStack(t)
			coord.cfg.VideoEngine, coord.cfg.VideoSourceMaxDimension = true, 64
			coord.cfg.VideoInferWidth, coord.cfg.VideoInferHeight, coord.cfg.VideoInferFPS = 32, 32, 5
			coord.cfg.GestureEnabled = mode != "segmentation"
			coord.cfg.GestureFPS, coord.cfg.GestureMaxDimension, coord.cfg.GestureMaxHands = 15, 64, 2
			dialer := &gestureOnlyDialer{configs: make(chan ai.VideoStreamConfig, 4)}
			coord.videoAI = ai.NewVideoManager(ai.Config{MaxActiveStreams: 4}, dialer)
			mgr := newVideoMgr()
			service := "change_background"
			if mode == "gesture" {
				service = "gesture_recognition"
			}
			sess, err := mgr.Create(session.SessionConfig{ID: "source-tcore", Task: service, VideoEnabled: true})
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
			if (cfg.Source.Segmentation != nil) != (mode != "gesture") || (cfg.Source.Gesture != nil) != (mode != "segmentation") {
				t.Fatal("wrong enabled task set")
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
