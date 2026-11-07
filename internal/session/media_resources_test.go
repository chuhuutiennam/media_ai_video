package session

import (
	"strconv"
	"testing"
	"time"

	"media-ai-gateway/internal/pipeline"
)

// ctrl-result (Manager.Update) ghi MediaResources trong khi result pump đọc ở mỗi kết quả.
// Chạy với -race: trước đây field thường bị race detector báo (manager.go Update vs
// coordinator.pumpResults).
func TestSessionMediaResourcesConcurrentUpdate(t *testing.T) {
	mgr := NewManager(ManagerConfig{MaxSessions: 1, PacketQueueSize: 1, AudioQueueSize: 1, ResultQueueSize: 1,
		IdleTimeout: time.Minute, GCInterval: time.Minute})
	sess, err := mgr.Create(SessionConfig{ID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close("s")
	if sess.MediaResources() != nil {
		t.Fatal("no ctrl-result yet: MediaResources must be nil")
	}

	const updates = 500
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < updates; i++ {
			if _, err := mgr.Update("s", SessionPatch{MediaResources: &pipeline.MediaResources{
				TCore: &pipeline.MediaResource{TerminationID: strconv.Itoa(i)}}}); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	reads := 0
	for running := true; running; reads++ {
		select {
		case <-done:
			running = false
		default:
		}
		if mr := sess.MediaResources(); mr != nil && mr.TCore == nil {
			t.Fatal("torn MediaResources")
		}
	}
	if got := sess.MediaResources().TCore.TerminationID; got != strconv.Itoa(updates-1) {
		t.Fatalf("last update lost: %s", got)
	}
	t.Logf("%d concurrent reads", reads)
}
