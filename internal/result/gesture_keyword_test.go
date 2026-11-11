package result

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"media-ai-gateway/internal/pipeline"
)

// Exercise real HTTP/2 framing without binding a port or requiring a live MF.
type keywordH2CServer struct {
	URL       string
	transport *http2.Transport
	mu        sync.Mutex
	conns     []net.Conn
}

func newKeywordH2CServer(handler http.Handler) *keywordH2CServer {
	s := &keywordH2CServer{URL: "http://mf.test"}
	s.transport = &http2.Transport{AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, _, _ string, _ *tls.Config) (net.Conn, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			client, server := net.Pipe()
			s.mu.Lock()
			s.conns = append(s.conns, client, server)
			s.mu.Unlock()
			go (&http2.Server{}).ServeConn(server, &http2.ServeConnOpts{Handler: handler})
			return client, nil
		}}
	return s
}

func (s *keywordH2CServer) Close() {
	s.transport.CloseIdleConnections()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, conn := range s.conns {
		_ = conn.Close()
	}
}

func (s *keywordH2CServer) newSink(rawURL string, timeout time.Duration, retries int) *HTTPCallbackSink {
	return &HTTPCallbackSink{URL: rawURL, MaxRetry: retries, AttemptTimeout: timeout,
		RetryBackoff: 200 * time.Millisecond, client: &http.Client{Transport: s.transport, Timeout: timeout}}
}

func keywordSnapshot(label, event string, ms int64) pipeline.RecognitionResult {
	h := pipeline.GestureHand{TrackID: 1, Handedness: "Right", RawLabel: label, RawScore: .9,
		Center: []float64{.5, .5}, BBox: []float64{.3, .3, .7, .7}}
	if event != "" {
		h.ActiveReaction = &pipeline.Reaction{EventID: event, Gesture: label, EmojiKey: "wave", StartedAtMs: ms, ValidUntilMs: ms + 1500}
	}
	return pipeline.RecognitionResult{SessionID: "call-tcore", StreamID: "call:tcore:video", Kind: "video_gesture", AIService: "gesture_recognition",
		MediaResources: &pipeline.MediaResources{TCore: &pipeline.MediaResource{ContextID: "ctx-001", TerminationID: "t-1"}},
		GestureResult: &pipeline.GestureFrameResult{MediaEpoch: "media", RecognizerEpoch: "recognizer", ResponseID: fmt.Sprintf("recognizer/%d", ms),
			FrameID: ms + 1, TimestampMs: ms, RTPTimestamp: uint32(ms * 90), ExtendedRTPTimestamp: uint64(ms * 90),
			OriginalWidth: 640, OriginalHeight: 360, Status: "ok", Hands: []pipeline.GestureHand{h}}}
}

type keywordRequest struct {
	body      map[string]any
	path, key string
}

func TestGestureKeywordAllMappingsAndOpenPalm(t *testing.T) {
	var got []keywordRequest
	srv := newKeywordH2CServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 || r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("invalid HTTP/2 request")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		got = append(got, keywordRequest{body, r.URL.EscapedPath(), r.Header.Get("Idempotency-Key")})
		w.WriteHeader(200)
	}))
	defer srv.Close()
	cfg := DefaultGestureKeywordConfig()
	cfg.Mode = "keyword"
	sink := srv.newSink(srv.URL+"/results/tCore", time.Second, 0).WithGestureKeywords(cfg)
	want := map[string]string{"Closed_Fist": "determination", "Pointing_Up": "attention", "Thumb_Down": "disapprove", "Thumb_Up": "approve", "Victory": "celebrate", "ILoveYou": "love", "Wave": "congrat"}
	ms := int64(0)
	for _, label := range []string{"Closed_Fist", "Pointing_Up", "Thumb_Down", "Thumb_Up", "Victory", "ILoveYou", "Wave"} {
		r := keywordSnapshot(label, label+"-event", ms)
		if err := sink.Send(context.Background(), r); err != nil {
			t.Fatal(err)
		}
		request := got[len(got)-1]
		if len(request.body) != 2 || request.body["topic_label"] != "EMOTION" || request.body["topic_info"] != want[label] || request.key == "" ||
			request.path != "/nmf-mrm/v2/contexts/ctx-001/termination/t-1/keyword" {
			t.Fatalf("%s: %+v", label, request)
		}
		// Same event with a new response ID and an extended TTL remains one effect.
		r = keywordSnapshot(label, label+"-event", ms+50)
		if err := sink.Send(context.Background(), r); err != nil {
			t.Fatal(err)
		}
		ms += 100
	}
	if len(got) != 7 {
		t.Fatalf("duplicate effects: %d", len(got))
	}
	for _, offset := range []int64{0, 100, 200, 300} {
		if err := sink.Send(context.Background(), keywordSnapshot("Open_Palm", "", ms+offset)); err != nil {
			t.Fatal(err)
		}
		if offset < 200 && len(got) != 7 {
			t.Fatal("open palm emitted before hold")
		}
	}
	if len(got) != 8 || got[7].body["topic_info"] != "hello" {
		t.Fatalf("missing open palm: %+v", got)
	}
	for _, label := range []string{"None", "Unknown"} {
		ms += 500
		if err := sink.Send(context.Background(), keywordSnapshot(label, "unknown", ms)); err != nil {
			t.Fatal(err)
		}
	}
	r := keywordSnapshot("Wave", "", ms+1)
	r.GestureResult.Hands = nil
	if err := sink.Send(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if len(got) != 8 {
		t.Fatal("empty/unknown hands emitted a keyword")
	}
}

func TestGestureKeywordRetriesPartialDeliveryAndOrdering(t *testing.T) {
	var got []keywordRequest
	failed := true
	srv := newKeywordH2CServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, keywordRequest{body: body, key: r.Header.Get("Idempotency-Key")})
		if body["topic_info"] == "congrat" && failed {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	cfg := DefaultGestureKeywordConfig()
	cfg.Mode = "keyword"
	cfg.CacheSize = 8
	sink := srv.newSink(srv.URL, time.Second, 1).WithGestureKeywords(cfg)
	sink.RetryBackoff = time.Millisecond
	r := keywordSnapshot("Thumb_Up", "thumb-event", 100)
	wave := keywordSnapshot("Wave", "wave-event", 100).GestureResult.Hands[0]
	wave.TrackID = 2
	r.GestureResult.Hands = append(r.GestureResult.Hands, wave)
	if err := sink.Send(context.Background(), r); err == nil {
		t.Fatal("expected failed wave delivery")
	}
	if len(got) != 3 || got[1].key != got[2].key {
		t.Fatal("retry did not reuse idempotency key", got)
	}
	failed = false
	if err := sink.Send(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[3].body["topic_info"] != "congrat" || got[3].key != got[1].key {
		t.Fatal("partial delivery replayed thumb or changed wave key", got)
	}
	older := keywordSnapshot("Victory", "old", 50)
	if err := sink.Send(context.Background(), older); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatal("late snapshot regressed effect")
	}
	for i := int64(101); i < 120; i++ {
		if err := sink.Send(context.Background(), keywordSnapshot("Wave", fmt.Sprint(i), i)); err != nil {
			t.Fatal(err)
		}
	}
	if len(sink.keywords.seen) > 8 {
		t.Fatal("unbounded dedup cache")
	}
}

func TestGestureKeywordMultiHandCoalescingAndConcurrentSnapshots(t *testing.T) {
	var mu sync.Mutex
	count := 0
	srv := newKeywordH2CServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { mu.Lock(); count++; mu.Unlock(); w.WriteHeader(200) }))
	defer srv.Close()
	cfg := DefaultGestureKeywordConfig()
	cfg.Mode = "keyword"
	sink := srv.newSink(srv.URL, time.Second, 0).WithGestureKeywords(cfg)
	r := keywordSnapshot("Wave", "first", 0)
	second := keywordSnapshot("Wave", "second", 0).GestureResult.Hands[0]
	second.TrackID = 2
	r.GestureResult.Hands = append(r.GestureResult.Hands, second)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sink.Send(context.Background(), r); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Fatalf("same effect repeated %d times", count)
	}
}

func TestGestureKeywordURLAndLegacyCompatibility(t *testing.T) {
	r := keywordSnapshot("Wave", "event", 0)
	r.MediaResources = &pipeline.MediaResources{TAccess: &pipeline.MediaResource{ContextID: "ctx/002", TerminationID: "t 2"}}
	want := "http://mf/nmf-mrm/v2/contexts/ctx%2F002/termination/t%202/keyword"
	if got, err := keywordURL("http://old/results/tAccess", "http://mf", r); err != nil || got != want {
		t.Fatal(got, err)
	}
	if got, err := keywordURL(want, "http://ignored", r); err != nil || got != want {
		t.Fatal(got, err)
	}
	if _, err := keywordURL("http://mf/nmf-mrm/v2/contexts/wrong/termination/t/keyword", "", r); err == nil {
		t.Fatal("accepted mismatched resource URL")
	}
	r.MediaResources = nil
	if _, err := keywordURL("http://mf", "", r); err == nil {
		t.Fatal("accepted missing resource")
	}
	empty := keywordSnapshot("", "", 0)
	empty.MediaResources, empty.GestureResult.Hands = nil, nil
	cfgEmpty := DefaultGestureKeywordConfig()
	cfgEmpty.Mode = "keyword"
	if err := NewHTTPCallbackSink("http://mf", time.Second, 0).WithGestureKeywords(cfgEmpty).Send(context.Background(), empty); err != nil {
		t.Fatal("empty snapshot should not need a keyword destination", err)
	}
	var got []httpCallbackBody
	srv := newKeywordH2CServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/results/tCore" || r.Header.Get("Idempotency-Key") != "" {
			t.Error("changed legacy route/headers")
		}
		var body httpCallbackBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, body)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	sink := srv.newSink(srv.URL+"/results/tCore", time.Second, 0).WithGestureKeywords(DefaultGestureKeywordConfig())
	if err := sink.Send(context.Background(), keywordSnapshot("Wave", "event", 0)); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultGestureKeywordConfig()
	cfg.Mode = "keyword"
	sink.WithGestureKeywords(cfg)
	if err := sink.Send(context.Background(), pipeline.RecognitionResult{Kind: "video_mask", VideoMask: &pipeline.VideoMask{FrameID: 1}, IsFinal: true}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].EventType != "video.gesture.result" || got[0].GestureResult == nil || got[1].EventType != "video.mask.result" {
		t.Fatal("legacy/mask changed", got)
	}
}

func TestGestureKeywordSingleLegUsesMFTAccessResource(t *testing.T) {
	r := keywordSnapshot("Thumb_Up", "event", 0)
	r.MediaResources.TAccess = &pipeline.MediaResource{ContextID: "ctx-access", TerminationID: "term-access"}
	want := "http://mf/nmf-mrm/v2/contexts/ctx-access/termination/term-access/keyword"
	if got, err := keywordURL("http://mf/results/tAccess", "", r); err != nil || got != want {
		t.Fatalf("single-leg destination: got %q, err %v", got, err)
	}
	if _, err := keywordURL("http://mf/nmf-mrm/v2/contexts/ctx-001/termination/t-1/keyword", "", r); err == nil {
		t.Fatal("accepted tCore keyword URL for a single-leg service targeting tAccess")
	}
	// Do not silently route to the source termination if MF resources are missing.
	r.MediaResources.TAccess = &pipeline.MediaResource{}
	if _, err := keywordURL("http://mf/results/tAccess", "", r); err == nil {
		t.Fatal("accepted incomplete MF tAccess resource")
	}
}

func TestGestureKeywordOpenPalmResetsAndReactionTakesPriority(t *testing.T) {
	var count atomic.Int64
	srv := newKeywordH2CServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	cfg := DefaultGestureKeywordConfig()
	cfg.Mode = "keyword"
	sink := srv.newSink(srv.URL, time.Second, 0).WithGestureKeywords(cfg)
	for _, step := range []struct {
		ms           int64
		label, event string
		want         int64
	}{
		{0, "Open_Palm", "", 0}, {100, "Open_Palm", "", 0},
		{150, "", "", 0}, // Losing the hand cancels the pending hold.
		{200, "Open_Palm", "", 0}, {300, "Open_Palm", "", 0}, {400, "Open_Palm", "", 1},
		{1000, "Open_Palm", "", 1}, // Gap > 500 ms requires a fresh hold.
		{1100, "Open_Palm", "", 1}, {1200, "Open_Palm", "", 2},
		{1300, "None", "", 2}, // A changed label resets the confirmed palm.
		{1400, "Open_Palm", "", 2},
		{1500, "Wave", "wave-event", 3}, // AI reaction wins over raw Open_Palm.
		{1600, "Open_Palm", "", 3}, {1700, "Open_Palm", "", 3}, {1800, "Open_Palm", "", 4},
	} {
		r := keywordSnapshot(step.label, step.event, step.ms)
		if step.label == "" {
			r.GestureResult.Hands = nil
		} else if step.event != "" {
			r.GestureResult.Hands[0].RawLabel = "Open_Palm"
		}
		if err := sink.Send(context.Background(), r); err != nil {
			t.Fatal(err)
		}
		if got := count.Load(); got != step.want {
			t.Fatalf("at %d ms: got %d effects, want %d", step.ms, got, step.want)
		}
	}
}

func TestGestureKeywordRejectsInvalidSnapshotBeforeSending(t *testing.T) {
	var count atomic.Int64
	srv := newKeywordH2CServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	cfg := DefaultGestureKeywordConfig()
	cfg.Mode = "keyword"
	sink := srv.newSink(srv.URL, time.Second, 0).WithGestureKeywords(cfg)
	for _, mutate := range []func(*pipeline.GestureFrameResult){
		func(g *pipeline.GestureFrameResult) { g.Status = "error" },
		func(g *pipeline.GestureFrameResult) { g.Hands[0].ActiveReaction.ValidUntilMs = g.TimestampMs },
		func(g *pipeline.GestureFrameResult) { g.MediaEpoch = "" },
	} {
		r := keywordSnapshot("Wave", "event", 100)
		mutate(r.GestureResult)
		if err := sink.Send(context.Background(), r); err == nil {
			t.Fatal("invalid snapshot was accepted")
		}
	}
	if count.Load() != 0 {
		t.Fatal("invalid snapshot sent a keyword")
	}
}

func TestGestureKeywordLegacyDeadLetterCompatibility(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			srv := newKeywordH2CServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			defer srv.Close()
			sink := srv.newSink(srv.URL, time.Second, 0).WithGestureKeywords(DefaultGestureKeywordConfig())
			deadLetters := 0
			sink.DeadLetter = NewSinkFunc("capture", func(context.Context, pipeline.RecognitionResult) error {
				deadLetters++
				return nil
			})
			err := sink.Send(context.Background(), keywordSnapshot("Wave", "event", 0))
			if status == http.StatusBadRequest {
				if err == nil || deadLetters != 0 {
					t.Fatal("legacy 4xx must return immediately without dead-letter forwarding")
				}
			} else if err != nil || deadLetters != 1 {
				t.Fatal("legacy exhausted retries must reach the dead-letter sink", err)
			}
		})
	}
}
