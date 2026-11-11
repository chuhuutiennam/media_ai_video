package coordinator

import (
	"context"
	"testing"
	"time"

	"media-ai-gateway/internal/pipeline"
	"media-ai-gateway/internal/result"
)

func TestGestureKeywordCallbackConfiguration(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(map[bool]string{false: "per_sink", true: "shared_pool"}[shared], func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.GestureKeywords = result.DefaultGestureKeywordConfig()
			cfg.GestureKeywords.Mode = "keyword"
			cfg.GestureKeywords.BaseURL = "http://mf.test"
			cfg.CallbackRetryBackoff = 7 * time.Millisecond
			if shared {
				cfg.CallbackClient = result.NewCallbackHTTPClient(time.Second, 0, 0)
			}
			coord := New(cfg, nil, nil, result.NewDispatcher(result.DefaultConfig()))
			// An empty task override restores the configured keyword base URL.
			sink := coord.UpdateTaskCallbackSink("call-tcore", "gesture_recognition", "")
			if sink == nil || sink.URL != cfg.GestureKeywords.BaseURL || sink.RetryBackoff != cfg.CallbackRetryBackoff {
				t.Fatalf("keyword callback configuration lost: %+v", sink)
			}
			// Valid no-hand snapshots are suppressed without a request or resources.
			// This also verifies both construction paths actually enable the mapper.
			empty := pipeline.RecognitionResult{SessionID: "call-tcore", Kind: "video_gesture",
				GestureResult: &pipeline.GestureFrameResult{MediaEpoch: "media", RecognizerEpoch: "recognizer",
					ResponseID: "recognizer/1", FrameID: 1, Status: "ok", OriginalWidth: 640, OriginalHeight: 360}}
			if err := sink.Send(context.Background(), empty); err != nil {
				t.Fatal(err)
			}
			fullURL := "http://other.test/nmf-mrm/v2/contexts/ctx/termination/term/keyword"
			if override := coord.UpdateTaskCallbackSink("call-tcore", "gesture_recognition", fullURL); override.URL != fullURL {
				t.Fatal("explicit keyword URL was overridden by base_url")
			}
			if coord.UpdateTaskCallbackSink("call-tcore", "video_segment", "") != nil {
				t.Fatal("keyword base URL leaked into segmentation callback")
			}
		})
	}
}
