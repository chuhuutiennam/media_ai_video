package controlplane

import (
	"net/http"
	"strings"
	"testing"
)

// Gateway chỉ video (audio.enabled=false): dịch vụ audio trả 400 để DCSF biết gửi nhầm gateway;
// không tạo session, không cấp cổng. Dịch vụ video vẫn chạy bình thường.
func TestAnswerAudioServiceRejectedWhenAudioDisabled(t *testing.T) {
	starter := &fakeStarter{}
	start, end := availableUDPRange(t, 4)
	s := newTestServerWithVideo(t, starter, start, end)
	s.cfg.AudioDisabled = true

	for _, service := range []string{"speech_to_text", "Realtime_Translation"} {
		w := postJSON(t, s, notifyPath(testCallID), SessionEvent{Event: "ANSWER", SelectedService: service, BearerCapability: "VIDEO"})
		assertStatus(t, w, http.StatusBadRequest)
		var e ErrorResponse
		decodeJSON(t, w, &e)
		if !strings.Contains(e.Error, service) || !strings.Contains(e.Error, "audio.enabled=false") {
			t.Fatalf("%s: error body %+v", service, e)
		}
		if s.sessionMgr.Count() != 0 || len(starter.sessions) != 0 || s.portAlloc.Available() != s.portAlloc.Total() {
			t.Fatalf("%s: rejected audio service created sessions or took ports", service)
		}
	}

	w := postJSON(t, s, notifyPath(testCallID), SessionEvent{Event: "ANSWER", SelectedService: "change_background"})
	assertStatus(t, w, http.StatusCreated)
}

// Mặc định (AudioDisabled=false) dịch vụ audio vẫn được nhận như cũ.
func TestAnswerAudioServiceAcceptedByDefault(t *testing.T) {
	s := newTestServer(t, &fakeStarter{})
	assertStatus(t, postJSON(t, s, notifyPath(testCallID), validAnswerReq()), http.StatusCreated)
}
