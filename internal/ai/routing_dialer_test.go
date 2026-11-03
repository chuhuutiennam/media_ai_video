package ai

import (
	"context"
	"errors"
	"testing"
	"time"
)

// --- WorkerInfo.Supports ---

func TestWorkerInfo_Supports_ExactMatch(t *testing.T) {
	w := WorkerInfo{Languages: []string{"vi"}, Tasks: []string{"speech_to_text"}}
	if !w.Supports("vi", "speech_to_text") {
		t.Error("exact match should be supported")
	}
	if w.Supports("en", "speech_to_text") {
		t.Error("wrong language should not match")
	}
	if w.Supports("vi", "realtime_translation") {
		t.Error("wrong task should not match")
	}
}

func TestWorkerInfo_Supports_Wildcard(t *testing.T) {
	w := WorkerInfo{Languages: []string{"*"}, Tasks: []string{"*"}}
	if !w.Supports("vi", "speech_to_text") {
		t.Error("wildcard language+task should match anything")
	}
	if !w.Supports("zh", "realtime_translation") {
		t.Error("wildcard should match any language/task")
	}
}

func TestWorkerInfo_Supports_EmptyList(t *testing.T) {
	w := WorkerInfo{Languages: nil, Tasks: nil}
	if !w.Supports("vi", "speech_to_text") {
		t.Error("empty capability list should match everything")
	}
}

func TestWorkerInfo_Supports_MultipleLanguages(t *testing.T) {
	w := WorkerInfo{Languages: []string{"vi", "en"}, Tasks: []string{"speech_to_text"}}
	if !w.Supports("vi", "speech_to_text") {
		t.Error("vi should match")
	}
	if !w.Supports("en", "speech_to_text") {
		t.Error("en should match")
	}
	if w.Supports("zh", "speech_to_text") {
		t.Error("zh should not match")
	}
}

// --- WorkerInfo.loadScore / atCapacity ---

func TestWorkerInfo_AtCapacity(t *testing.T) {
	cases := []struct {
		max, active int
		want        bool
	}{
		{10, 10, true},
		{10, 9, false},
		{0, 100, false}, // unlimited
	}
	for _, c := range cases {
		w := WorkerInfo{MaxStreams: c.max, ActiveStreams: c.active}
		if got := w.atCapacity(); got != c.want {
			t.Errorf("atCapacity(%d/%d) = %v, want %v", c.active, c.max, got, c.want)
		}
	}
}

func TestWorkerInfo_LoadScore_NoGPU(t *testing.T) {
	w1 := WorkerInfo{MaxStreams: 10, ActiveStreams: 2}
	w2 := WorkerInfo{MaxStreams: 10, ActiveStreams: 8}
	if w1.loadScore() >= w2.loadScore() {
		t.Error("w1 (2/10) should have lower score than w2 (8/10)")
	}
}

func TestWorkerInfo_LoadScore_WithGPU(t *testing.T) {
	// same stream ratio but different GPU load
	w1 := WorkerInfo{MaxStreams: 10, ActiveStreams: 5, GPULoad: 0.1}
	w2 := WorkerInfo{MaxStreams: 10, ActiveStreams: 5, GPULoad: 0.9}
	if w1.loadScore() >= w2.loadScore() {
		t.Error("w1 (low GPU) should have lower score than w2 (high GPU)")
	}
}

func TestWorkerInfo_LoadScore_UnlimitedWorker(t *testing.T) {
	w := WorkerInfo{MaxStreams: 0, ActiveStreams: 9999}
	if w.loadScore() != 0 {
		t.Errorf("unlimited worker (MaxStreams=0) should have score 0, got %f", w.loadScore())
	}
}

// --- WorkerRegistry ---

func TestWorkerRegistry_Register_And_List(t *testing.T) {
	r := NewWorkerRegistry(0)
	r.Register(WorkerInfo{ID: "w1", Addr: "w1:50051", Languages: []string{"vi"}})
	r.Register(WorkerInfo{ID: "w2", Addr: "w2:50051", Languages: []string{"en"}})

	list := r.List()
	if len(list) != 2 {
		t.Fatalf("expected 2 workers, got %d", len(list))
	}
}

func TestWorkerRegistry_Register_Overwrite(t *testing.T) {
	r := NewWorkerRegistry(0)
	r.Register(WorkerInfo{ID: "w1", ActiveStreams: 1})
	r.Register(WorkerInfo{ID: "w1", ActiveStreams: 5})

	list := r.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 worker, got %d", len(list))
	}
	if list[0].ActiveStreams != 5 {
		t.Errorf("expected updated ActiveStreams=5, got %d", list[0].ActiveStreams)
	}
}

func TestWorkerRegistry_Deregister(t *testing.T) {
	r := NewWorkerRegistry(0)
	r.Register(WorkerInfo{ID: "w1"})
	r.Deregister("w1")

	if r.Len() != 0 {
		t.Errorf("expected 0 workers after deregister, got %d", r.Len())
	}
}

func TestWorkerRegistry_TTL_ExpiresStaleWorker(t *testing.T) {
	r := NewWorkerRegistry(50 * time.Millisecond)
	r.Register(WorkerInfo{ID: "w1", Addr: "w1:50051"})

	// Fresh: visible.
	if list := r.List(); len(list) != 1 {
		t.Fatalf("expected 1 fresh worker, got %d", len(list))
	}

	time.Sleep(80 * time.Millisecond)

	// Stale: hidden from List and Select.
	if list := r.List(); len(list) != 0 {
		t.Errorf("expected 0 workers after TTL, got %d", len(list))
	}
	if r.Len() != 1 {
		t.Errorf("Len should still include stale workers, got %d", r.Len())
	}
}

func TestWorkerRegistry_Select_PicksLeastLoaded(t *testing.T) {
	r := NewWorkerRegistry(0)
	r.Register(WorkerInfo{ID: "heavy", Addr: "heavy:50051",
		Languages: []string{"vi"}, Tasks: []string{"speech_to_text"},
		MaxStreams: 10, ActiveStreams: 8})
	r.Register(WorkerInfo{ID: "light", Addr: "light:50051",
		Languages: []string{"vi"}, Tasks: []string{"speech_to_text"},
		MaxStreams: 10, ActiveStreams: 2})

	w, err := r.Select("vi", "speech_to_text")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if w.ID != "light" {
		t.Errorf("expected least-loaded 'light', got %q", w.ID)
	}
}

func TestWorkerRegistry_Select_FiltersUnsupportedLanguage(t *testing.T) {
	r := NewWorkerRegistry(0)
	r.Register(WorkerInfo{ID: "en-worker", Addr: "en:50051",
		Languages: []string{"en"}, Tasks: []string{"speech_to_text"}})

	_, err := r.Select("vi", "speech_to_text")
	if !errors.Is(err, ErrNoWorkerAvailable) {
		t.Errorf("expected ErrNoWorkerAvailable, got %v", err)
	}
}

func TestWorkerRegistry_Select_FiltersUnsupportedTask(t *testing.T) {
	r := NewWorkerRegistry(0)
	r.Register(WorkerInfo{ID: "stt-only", Addr: "stt:50051",
		Languages: []string{"vi"}, Tasks: []string{"speech_to_text"}})

	_, err := r.Select("vi", "realtime_translation")
	if !errors.Is(err, ErrNoWorkerAvailable) {
		t.Errorf("expected ErrNoWorkerAvailable, got %v", err)
	}
}

func TestWorkerRegistry_Select_SkipsAtCapacity(t *testing.T) {
	r := NewWorkerRegistry(0)
	r.Register(WorkerInfo{ID: "full", Addr: "full:50051",
		Languages: []string{"vi"}, Tasks: []string{"speech_to_text"},
		MaxStreams: 5, ActiveStreams: 5}) // at capacity

	_, err := r.Select("vi", "speech_to_text")
	if !errors.Is(err, ErrNoWorkerAvailable) {
		t.Errorf("expected ErrNoWorkerAvailable for full worker, got %v", err)
	}
}

func TestWorkerRegistry_Select_WildcardWorker(t *testing.T) {
	r := NewWorkerRegistry(0)
	r.Register(WorkerInfo{ID: "any", Addr: "any:50051",
		Languages: []string{"*"}, Tasks: []string{"*"}})

	w, err := r.Select("zh", "realtime_translation")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if w.ID != "any" {
		t.Errorf("expected wildcard worker 'any', got %q", w.ID)
	}
}

func TestWorkerRegistry_Select_EmptyRegistry(t *testing.T) {
	r := NewWorkerRegistry(0)
	_, err := r.Select("vi", "speech_to_text")
	if !errors.Is(err, ErrNoWorkerAvailable) {
		t.Errorf("expected ErrNoWorkerAvailable on empty registry, got %v", err)
	}
}

func TestWorkerRegistry_Select_PreferUnlimited(t *testing.T) {
	// Worker with MaxStreams=0 should score 0 → preferred over limited worker.
	r := NewWorkerRegistry(0)
	r.Register(WorkerInfo{ID: "limited", Addr: "limited:50051",
		Languages: []string{"vi"}, Tasks: []string{"speech_to_text"},
		MaxStreams: 10, ActiveStreams: 1})
	r.Register(WorkerInfo{ID: "unlimited", Addr: "unlimited:50051",
		Languages: []string{"vi"}, Tasks: []string{"speech_to_text"},
		MaxStreams: 0, ActiveStreams: 100}) // MaxStreams=0 → score=0

	w, err := r.Select("vi", "speech_to_text")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if w.ID != "unlimited" {
		t.Errorf("expected unlimited worker (score=0), got %q", w.ID)
	}
}

// --- RoutingDialer ---

func TestRoutingDialer_Dial_RoutesToCorrectWorker(t *testing.T) {
	r := NewWorkerRegistry(0)
	r.Register(WorkerInfo{
		ID: "vi-worker", Addr: "vi-worker:50051",
		Languages: []string{"vi"}, Tasks: []string{"speech_to_text"},
	})

	var dialedAddr string
	dialFn := func(ctx context.Context, workerAddr, sessionID, streamID, language, task string) (StreamClient, error) {
		dialedAddr = workerAddr
		return newFakeClient(ctx), nil
	}

	d := NewRoutingDialer(r, dialFn)
	ctx := context.Background()
	_, err := d.Dial(ctx, "sess-1", "stream-1", "vi", "speech_to_text")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if dialedAddr != "vi-worker:50051" {
		t.Errorf("dialed %q, want %q", dialedAddr, "vi-worker:50051")
	}
}

func TestRoutingDialer_Dial_NoWorker_ReturnsErr(t *testing.T) {
	r := NewWorkerRegistry(0) // empty
	d := NewRoutingDialer(r, nil)

	_, err := d.Dial(context.Background(), "sess-1", "stream-1", "vi", "speech_to_text")
	if err == nil {
		t.Fatal("expected error when no workers available")
	}
	if !errors.Is(err, ErrNoWorkerAvailable) {
		t.Errorf("expected ErrNoWorkerAvailable wrapped, got %v", err)
	}
}

func TestRoutingDialer_Dial_PropagatesDialError(t *testing.T) {
	r := NewWorkerRegistry(0)
	r.Register(WorkerInfo{ID: "w1", Addr: "w1:50051",
		Languages: []string{"vi"}, Tasks: []string{"speech_to_text"}})

	dialErr := errors.New("connection refused")
	dialFn := func(ctx context.Context, workerAddr, sessionID, streamID, language, task string) (StreamClient, error) {
		return nil, dialErr
	}

	d := NewRoutingDialer(r, dialFn)
	_, err := d.Dial(context.Background(), "sess-1", "stream-1", "vi", "speech_to_text")
	if !errors.Is(err, dialErr) {
		t.Errorf("expected wrapped dialErr, got %v", err)
	}
}

func TestRoutingDialer_Dial_PassesAllParams(t *testing.T) {
	r := NewWorkerRegistry(0)
	r.Register(WorkerInfo{ID: "w1", Addr: "w1:50051",
		Languages: []string{"en"}, Tasks: []string{"realtime_translation"}})

	var gotSession, gotStream, gotLang, gotTask string
	dialFn := func(ctx context.Context, workerAddr, sessionID, streamID, language, task string) (StreamClient, error) {
		gotSession = sessionID
		gotStream = streamID
		gotLang = language
		gotTask = task
		return newFakeClient(ctx), nil
	}

	d := NewRoutingDialer(r, dialFn)
	d.Dial(context.Background(), "my-session", "my-stream", "en", "realtime_translation")

	if gotSession != "my-session" || gotStream != "my-stream" ||
		gotLang != "en" || gotTask != "realtime_translation" {
		t.Errorf("params not forwarded correctly: session=%q stream=%q lang=%q task=%q",
			gotSession, gotStream, gotLang, gotTask)
	}
}
