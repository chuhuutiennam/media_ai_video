package controlplane

import (
	"net/http"
	"strings"
	"testing"

	"media-ai-gateway/internal/ai"
	"media-ai-gateway/internal/result"
)

// Dịch vụ video khai báo bằng cấu hình (không có trong code): legs, effect và AI task lấy từ danh mục.
func TestVideoServiceCatalog_CustomServiceLegsAndTask(t *testing.T) {
	starter := &fakeStarter{}
	start, end := availableUDPRange(t, 4)
	s := newTestServerWithVideo(t, starter, start, end)
	s.cfg.VideoServices = map[string]VideoService{
		"blur_background": {Enabled: true, EffectType: "bg_blur", AITask: VideoTaskSegment},
	}

	w := postJSON(t, s, notifyPath(testCallID), SessionEvent{Event: "ANSWER", SelectedService: "Blur_Background"})
	assertStatus(t, w, http.StatusCreated)
	var resp SessionResponse
	decodeJSON(t, w, &resp)
	if resp.TCoreVideoRTPPort == 0 || resp.TAccessVideoRTPPort == 0 || resp.TCoreRTPPort != 0 {
		t.Fatalf("legs both: want 2 video ports and no audio, got %+v", resp)
	}
	if len(starter.sessions) != 2 || starter.videoStarted != 2 {
		t.Fatalf("started %d sessions / %d video, want 2/2", len(starter.sessions), starter.videoStarted)
	}
	for _, sess := range starter.sessions {
		defer s.sessionMgr.Close(sess.ID)
		if sess.VideoAITask != VideoTaskSegment || sess.EffectType != "bg_blur" || sess.Codec != "" || !sess.VideoEnabled {
			t.Fatalf("session %s: %+v", sess.ID, sess)
		}
	}
}

// Dịch vụ tắt (enabled: false) hoặc không có trong danh mục: ACK 200 {} và không tạo session.
func TestVideoServiceCatalog_DisabledOrUnknownServiceAcks(t *testing.T) {
	for _, service := range []string{"video_effects", "gesture_recognition", "fun_calling"} { // tắt, không còn là dịch vụ mặc định, lạ
		starter := &fakeStarter{}
		start, end := availableUDPRange(t, 4)
		s := newTestServerWithVideo(t, starter, start, end)
		s.cfg.VideoServices["video_effects"] = VideoService{Enabled: false, AITask: VideoTaskGesture}

		w := postJSON(t, s, notifyPath(testCallID), SessionEvent{Event: "ANSWER", SelectedService: service})
		assertStatus(t, w, http.StatusOK)
		if body := strings.TrimSpace(w.Body.String()); body != "{}" {
			t.Fatalf("%s: body %q, want {}", service, body)
		}
		if s.sessionMgr.Count() != 0 || len(starter.sessions) != 0 {
			t.Fatalf("%s: session created for disabled/unknown service", service)
		}
	}
}

// Kiểm tra AI theo dịch vụ khi ANSWER: dịch vụ không còn worker của chính nó (hoặc hết slot)
// → 503 trước khi tạo session; worker của dịch vụ khác không được tính.
func TestVideoServiceCatalog_NoAIWorkerForServiceReturns503(t *testing.T) {
	starter := &fakeStarter{}
	start, end := availableUDPRange(t, 6)
	s := newTestServerWithVideo(t, starter, start, end)
	reg := ai.NewWorkerRegistry(0)
	reg.Register(ai.WorkerInfo{ID: "video-service:video_effects:g:1", Addr: "g:1",
		Tasks: []string{VideoTaskGesture}, Services: []string{"video_effects"}, MaxStreams: 1,
		MediaTypes: []string{"video"}, InputFormats: []string{"jpeg"}, Protocols: []string{"video_segment"}})
	s.SetVideoWorkerRegistry(reg)

	expect503 := func(callID, service string) {
		t.Helper()
		w := postJSON(t, s, notifyPath(callID), SessionEvent{Event: "ANSWER", SelectedService: service})
		assertStatus(t, w, http.StatusServiceUnavailable)
		var e ErrorResponse
		decodeJSON(t, w, &e)
		if !strings.Contains(e.Error, service) || e.RetryAfterMs == 0 {
			t.Fatalf("503 body: %+v", e)
		}
	}

	// change_background không có worker riêng; worker gesture thuộc dịch vụ khác.
	expect503("call-a", "change_background")
	if s.sessionMgr.Count() != 0 {
		t.Fatal("503 left sessions alive")
	}
	waitPortsReleased(t, s)

	// video_effects còn slot → tạo session.
	w := postJSON(t, s, notifyPath("call-b"), SessionEvent{Event: "ANSWER", SelectedService: "video_effects"})
	assertStatus(t, w, http.StatusCreated)
	defer s.sessionMgr.Close("call-b-tcore")

	// Slot duy nhất đã bị giữ (như lúc dial) → cuộc gọi gesture tiếp theo bị từ chối.
	if _, err := reg.SelectVideoTask("video_effects", VideoTaskGesture, "jpeg", "video_segment"); err != nil {
		t.Fatal(err)
	}
	expect503("call-c", "video_effects")
}

// singleLegStarter ghi lại callback mặc định và callback theo task mà ctrl-result áp cho từng session.
type singleLegStarter struct {
	taskCallbackStarter
	fallback map[string]string
}

func (f *singleLegStarter) UpdateCallbackSink(sessID, url string) *result.HTTPCallbackSink {
	f.fallback[sessID] = url
	return nil
}

// video_effects chỉ leg tcore: ctrl-result single-leg gửi kết quả gesture của tcore về
// callbackUrl của tAccess, session tcore mang cả resource tCore và tAccess. callbackUrls
// (override theo task) vẫn lấy từ tCore; callbackUrls trong tAccess bị bỏ qua.
func TestVideoEffects_CtrlResultSingleLeg(t *testing.T) {
	f := &singleLegStarter{taskCallbackStarter: taskCallbackStarter{urls: map[string]string{}}, fallback: map[string]string{}}
	start, end := availableUDPRange(t, 4)
	s := newTestServerWithVideo(t, f, start, end)
	assertStatus(t, postJSON(t, s, notifyPath(testCallID), SessionEvent{Event: "ANSWER", SelectedService: "video_effects"}), http.StatusCreated)

	req := CtrlResultRequest{CallID: testCallID, MediaResources: &MediaResources{
		TCore: MediaResourceInfo{ContextID: "ctx-tCore", Termination: TerminationInfo{TerminationID: "term-tCore"},
			CallbackURL: "http://mf/results/tCore", CallbackURLs: map[string]string{"gesture_recognition": "http://mf-gesture/results/tCore"}},
		TAccess: MediaResourceInfo{ContextID: "ctx-tAccess", Termination: TerminationInfo{TerminationID: "term-tAccess"},
			CallbackURL: "http://mf/results/tAccess", CallbackURLs: map[string]string{"gesture_recognition": "http://mf-gesture/results/tAccess"}},
	}}
	assertStatus(t, postJSON(t, s, ctrlResultPath(testCallID), req), http.StatusOK)

	tcore := testCallID + "-tcore"
	if len(f.fallback) != 1 || f.fallback[tcore] != "http://mf/results/tAccess" {
		t.Fatalf("fallback callbacks: %+v, want only tcore → tAccess callbackUrl", f.fallback)
	}
	if len(f.urls) != 1 || f.urls[tcore+"/gesture_recognition"] != "http://mf-gesture/results/tCore" {
		t.Fatalf("task callbacks: %+v, want only tcore gesture override from tCore", f.urls)
	}
	sess, ok := s.sessionMgr.Get(tcore)
	if !ok {
		t.Fatal("tcore session missing")
	}
	mr := sess.MediaResources
	if sess.CallbackURL != "http://mf/results/tAccess" || mr == nil || mr.TCore == nil || mr.TAccess == nil ||
		mr.TCore.TerminationID != "term-tCore" || mr.TAccess.TerminationID != "term-tAccess" {
		t.Fatalf("tcore session: callback %q, resources %+v", sess.CallbackURL, mr)
	}
	if _, ok := s.sessionMgr.Get(testCallID + "-taccess"); ok {
		t.Fatal("single-leg service has a taccess session")
	}
}
