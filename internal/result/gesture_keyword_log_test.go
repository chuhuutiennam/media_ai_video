package result

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestGestureKeywordMappingDecisionsVisibleAtInfo(t *testing.T) {
	for _, name := range []string{"mapped", "unmapped_gesture", "no_hands", "no_confirmed_reaction",
		"open_palm_pending", "open_palm_low_score", "late_snapshot", "duplicate_event", "invalid_destination"} {
		t.Run(name, func(t *testing.T) {
			core, logs := observer.New(zap.InfoLevel)
			restore := zap.ReplaceGlobals(zap.New(core))
			defer restore()
			var requests atomic.Int64
			srv := newKeywordH2CServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()
			cfg := DefaultGestureKeywordConfig()
			cfg.Mode = "keyword"
			sink := srv.newSink(srv.URL, time.Second, 0).WithGestureKeywords(cfg)
			r := keywordSnapshot("Wave", "event-1", 100)
			switch name {
			case "unmapped_gesture":
				r = keywordSnapshot("Unknown", "event-1", 100)
			case "no_hands":
				r.GestureResult.Hands = nil
			case "no_confirmed_reaction":
				r = keywordSnapshot("Thumb_Up", "", 100)
			case "open_palm_pending", "open_palm_low_score":
				r = keywordSnapshot("Open_Palm", "", 100)
				if name == "open_palm_low_score" {
					r.GestureResult.Hands[0].RawScore = 0.5
				}
			case "late_snapshot", "duplicate_event":
				if err := sink.Send(context.Background(), r); err != nil {
					t.Fatal(err)
				}
				logs.TakeAll()
				requests.Store(0)
				if name == "late_snapshot" {
					r = keywordSnapshot("Victory", "older-event", 50)
				}
			case "invalid_destination":
				r.MediaResources = nil
			}
			err := sink.Send(context.Background(), r)
			if (err != nil) != (name == "invalid_destination") {
				t.Fatalf("unexpected send error: %v", err)
			}
			message := "keyword: skipped"
			if name == "mapped" {
				message = "keyword: after mapping"
			}
			entries := logs.FilterMessage(message).All()
			if len(entries) != 1 {
				t.Fatalf("want one visible decision; got %+v", logs.All())
			}
			fields := entries[0].ContextMap()
			if fields["session_id"] != r.SessionID || fields["response_id"] != r.GestureResult.ResponseID ||
				fields["frame_id"] != r.GestureResult.FrameID {
				t.Fatalf("missing source identity: %+v", fields)
			}
			if name == "mapped" {
				if fields["gesture"] != "Wave" || fields["raw_label"] != "Wave" || fields["raw_score"] != 0.9 ||
					fields["track_id"] != uint64(1) || fields["event_id"] != "event-1" ||
					fields["topic_label"] != "EMOTION" || fields["topic_info"] != "congrat" || requests.Load() != 1 {
					t.Fatalf("missing mapping information: %+v", fields)
				}
			} else if fields["reason"] != name || requests.Load() != 0 {
				t.Fatalf("incorrect skip reason or unexpected MF request: %+v", fields)
			}
			if name == "unmapped_gesture" && (fields["gesture"] != "Unknown" || fields["track_id"] != uint64(1) || fields["raw_label"] != "Unknown") {
				t.Fatalf("missing unmapped source information: %+v", fields)
			}
		})
	}
}

// Legacy requests must not produce keyword mapping logs.
func TestGestureLegacyDoesNotLogKeywordMapping(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	restore := zap.ReplaceGlobals(zap.New(core))
	defer restore()
	srv := newKeywordH2CServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	sink := srv.newSink(srv.URL, time.Second, 0).WithGestureKeywords(DefaultGestureKeywordConfig())
	r := keywordSnapshot("Wave", "event-1", 100)
	if err := sink.Send(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	for _, entry := range logs.All() {
		if entry.Message == "keyword: after mapping" || entry.Message == "keyword: skipped" {
			t.Fatalf("legacy produced a keyword decision: %+v", entry)
		}
	}
}
