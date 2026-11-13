package coordinator

// Regression test: session dùng lại ID (callId) của session vừa đóng. Dọn dẹp của session cũ chạy
// bất đồng bộ và chỉ theo sess.ID; trước đây nó có thể xoá callback, đóng AI stream hoặc gỡ
// pipeline của session mới cùng ID (tái hiện ~80% số lần với dùng lại ngay sau Close).

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"media-ai-gateway/internal/pipeline"
	"media-ai-gateway/internal/session"
)

func TestReusedSessionIDKeepsNewSessionResources(t *testing.T) {
	var mu sync.Mutex
	got := map[string]int{}
	srv := newH2CTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got[r.URL.Path]++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	coord, _, dialer := newVideoStack(t)
	mgr := newVideoMgr()
	create := func(id, gen string) *session.Session {
		t.Helper()
		s, err := mgr.Create(session.SessionConfig{ID: id, SourceType: "raw_rtp", CallbackURL: srv.URL + "/" + id + "/" + gen,
			VideoEnabled: true, VideoInferWidth: 64, VideoInferHeight: 64, VideoInferFPS: 30, EffectType: "bg_blur"})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	const n = 100
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("call-%d-tcore", i)
		old := create(id, "old")
		if _, err := coord.Start(old); err != nil {
			t.Fatal(err)
		}
		if err := coord.StartVideo(old); err != nil {
			t.Fatal(err)
		}
		mgr.Close(id)            // cuộc gọi cũ kết thúc
		cur := create(id, "new") // callId dùng lại ngay
		if _, err := coord.Start(cur); err != nil {
			t.Fatalf("lần %d: Start session mới: %v", i, err)
		}
		if err := coord.StartVideo(cur); err != nil {
			t.Fatalf("lần %d: StartVideo session mới: %v", i, err)
		}
		time.Sleep(5 * time.Millisecond) // cho dọn dẹp muộn (nếu còn) của session cũ cơ hội chạy
		if c := dialer.client(id); c == nil || c.ctx.Err() != nil {
			t.Fatalf("lần %d: AI stream của session mới bị đóng", i)
		}
		cur.VideoMaskResultQueue <- pipeline.RecognitionResult{SessionID: id, Kind: "video_mask", IsFinal: true,
			VideoMask: &pipeline.VideoMask{FrameID: 1, Status: "ok"}}
		deadline := time.Now().Add(time.Second)
		for {
			mu.Lock()
			delivered := got["/"+id+"/new"]
			mu.Unlock()
			if delivered > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("lần %d: callback của session mới bị xoá (kết quả không tới)", i)
			}
			time.Sleep(2 * time.Millisecond)
		}
		mgr.Close(id)
	}
}

// Session cũ chưa dọn xong quá SessionReuseWait: Start session mới trả lỗi và không đăng ký gì.
func TestReusedSessionIDWaitTimeout(t *testing.T) {
	coord, _, _ := newVideoStack(t)
	coord.cfg.SessionReuseWait = 50 * time.Millisecond
	mgr := newVideoMgr()
	old, err := mgr.Create(session.SessionConfig{ID: "stuck-tcore", SourceType: "raw_rtp"})
	if err != nil {
		t.Fatal(err)
	}
	release, err := coord.acquireLife(old) // giả lập phần dọn dẹp chưa xong của session cũ
	if err != nil {
		t.Fatal(err)
	}
	mgr.Close(old.ID)
	cur, err := mgr.Create(session.SessionConfig{ID: "stuck-tcore", SourceType: "raw_rtp", CallbackURL: "http://127.0.0.1:1/cb"})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := coord.Start(cur); err == nil || !strings.Contains(err.Error(), "still releasing") {
		t.Fatalf("Start = %v, want timeout error", err)
	}
	if waited := time.Since(start); waited < 50*time.Millisecond || waited > time.Second {
		t.Fatalf("waited %v, want ~50ms", waited)
	}
	release()
	// Dọn xong thì session mới khởi động được.
	if _, err := coord.Start(cur); err != nil {
		t.Fatalf("Start after release: %v", err)
	}
	mgr.Close(cur.ID)
}

// ID đang thuộc một session còn sống (chưa đóng) khác instance: từ chối ngay, không chờ.
func TestReusedSessionIDRejectsLiveOwner(t *testing.T) {
	coord, _, _ := newVideoStack(t)
	live := &session.Session{ID: "dup-tcore", Ctx: t.Context()}
	release, err := coord.acquireLife(live)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	other := &session.Session{ID: "dup-tcore", Ctx: t.Context()}
	start := time.Now()
	if _, err := coord.acquireLife(other); err == nil || !strings.Contains(err.Error(), "another live session") {
		t.Fatalf("acquireLife = %v, want live-owner error", err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("waited for a live owner instead of failing fast")
	}
}
