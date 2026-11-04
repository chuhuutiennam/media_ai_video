package ai

import (
	"context"
	"errors"
	"testing"
)

func videoWorker(id, task string) WorkerInfo {
	return WorkerInfo{ID: id, Addr: id, Tasks: []string{task}, MaxStreams: 1,
		MediaTypes: []string{"video"}, InputFormats: []string{"jpeg"}, Protocols: []string{"video_segment"}}
}

func TestVideoRoutingSeparateWorkersAndCapacity(t *testing.T) {
	registry := NewWorkerRegistry(0)
	registry.Register(WorkerInfo{ID: "audio-wildcard", Addr: "wrong"})
	registry.Register(videoWorker("segmentation", "video_segment"))
	registry.Register(videoWorker("gesture", "gesture_recognition"))
	router := NewVideoRoutingDialer(registry, func(ctx context.Context, address, _, _ string, cfg VideoStreamConfig) (VideoStreamClient, error) {
		want := "segmentation"
		if cfg.AIService == "gesture_recognition" {
			want = "gesture"
		}
		if address != want {
			t.Fatalf("wrong endpoint: %q for %+v", address, cfg)
		}
		return &nullVideoClient{ctx: ctx}, nil
	})
	dial := func(service string) VideoStreamClient {
		t.Helper()
		client, err := router.Dial(context.Background(), "call", "video", VideoStreamConfig{AIService: service})
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
	// Legacy empty service must now reserve a segmentation slot too.
	segmentation := dial("")
	gesture := dial("gesture_recognition")
	defer gesture.(*reservedVideoClient).Close()
	for _, service := range []string{"", "segmentation", "gesture_recognition"} {
		if _, err := router.Dial(context.Background(), "other", "video", VideoStreamConfig{AIService: service}); !errors.Is(err, ErrNoWorkerAvailable) {
			t.Fatalf("capacity bypassed for %q: %v", service, err)
		}
	}
	segmentation.(*reservedVideoClient).Close()
	reopened := dial("segmentation")
	defer reopened.(*reservedVideoClient).Close()
	segmentation.(*reservedVideoClient).Close() // Must not release the reopened stream's slot.
	for _, worker := range registry.List() {
		if worker.ID != "audio-wildcard" && worker.ActiveStreams != 1 {
			t.Fatalf("wrong worker reservation: %+v", worker)
		}
	}
}

func TestVideoRoutingDialFailureReleasesWorker(t *testing.T) {
	for _, service := range []string{"", "segmentation", "gesture_recognition"} {
		t.Run(service, func(t *testing.T) {
			registry := NewWorkerRegistry(0)
			registry.Register(videoWorker("segmentation", "video_segment"))
			registry.Register(videoWorker("gesture", "gesture_recognition"))
			dialErr := errors.New("worker refused connection")
			router := NewVideoRoutingDialer(registry, func(context.Context, string, string, string, VideoStreamConfig) (VideoStreamClient, error) {
				return nil, dialErr
			})
			_, err := router.Dial(context.Background(), "call", "video", VideoStreamConfig{AIService: service})
			if !errors.Is(err, dialErr) {
				t.Fatal(err)
			}
			for _, worker := range registry.List() {
				if worker.ActiveStreams != 0 {
					t.Fatalf("failed dial leaked slot: %+v", worker)
				}
			}
		})
	}
}

func TestVideoRoutingRejectsAudioWildcardForSegmentation(t *testing.T) {
	registry := NewWorkerRegistry(0)
	registry.Register(WorkerInfo{ID: "audio-wildcard", Addr: "wrong"})
	_, err := NewVideoRoutingDialer(registry, nil).Dial(context.Background(), "call", "video", VideoStreamConfig{})
	if !errors.Is(err, ErrNoWorkerAvailable) {
		t.Fatal("segmentation selected an audio worker", err)
	}
}
