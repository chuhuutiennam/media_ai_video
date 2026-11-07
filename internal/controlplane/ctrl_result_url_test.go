package controlplane

import (
	"net/http"
	"strings"
	"testing"
)

func TestValidCallbackURL(t *testing.T) {
	for raw, want := range map[string]bool{
		"http://mf/results/tAccess":          true,
		"https://mf.example:8443/x?term=t-1": true,
		"HTTP://MF/x":                        true,
		"":                                   false,
		"ftp://mf/x":                         false,
		"/results/tAccess":                   false,
		"mf/results":                         false,
		"http://":                            false,
		"http://user:pw@mf/x":                false,
		"http://mf/" + strings.Repeat("a", maxCallbackURLLen): false,
	} {
		if got := validCallbackURL(raw); got != want {
			t.Errorf("validCallbackURL(%q) = %v, want %v", raw, got, want)
		}
	}
}

// URL sai ở bất kỳ field nào → 400, không áp dụng gì (callback, callback theo task, termination).
// Rỗng giữ nghĩa cũ và vẫn được nhận.
func TestCtrlResultRejectsInvalidCallbackURL(t *testing.T) {
	valid := func() *MediaResources {
		return &MediaResources{
			TCore: MediaResourceInfo{ContextID: "ctx", Termination: TerminationInfo{TerminationID: "t-core"},
				CallbackURL: "http://mf/results/tCore", CallbackURLs: map[string]string{"video_segment": "http://mf/seg"}},
			TAccess: MediaResourceInfo{ContextID: "ctx", Termination: TerminationInfo{TerminationID: "t-acc"},
				CallbackURL: "http://mf/results/tAccess"},
		}
	}
	for field, mutate := range map[string]func(*MediaResources){
		"tCore.callbackUrl":                func(m *MediaResources) { m.TCore.CallbackURL = "ftp://mf/x" },
		"tAccess.callbackUrl":              func(m *MediaResources) { m.TAccess.CallbackURL = "/results/tAccess" },
		"tCore.callbackUrls.video_segment": func(m *MediaResources) { m.TCore.CallbackURLs["video_segment"] = "http://user:pw@mf/x" },
		"tAccess.callbackUrls.gesture_recognition": func(m *MediaResources) {
			m.TAccess.CallbackURLs = map[string]string{"gesture_recognition": "http://"}
		},
	} {
		f := &singleLegStarter{taskCallbackStarter: taskCallbackStarter{urls: map[string]string{}}, fallback: map[string]string{}}
		s := newTestServer(t, f)
		assertStatus(t, postJSON(t, s, notifyPath(testCallID), validAnswerReq()), http.StatusCreated)
		mr := valid()
		mutate(mr)
		w := postJSON(t, s, ctrlResultPath(testCallID), CtrlResultRequest{CallID: testCallID, MediaResources: mr})
		assertStatus(t, w, http.StatusBadRequest)
		var e ErrorResponse
		decodeJSON(t, w, &e)
		if !strings.Contains(e.Error, "invalid "+field) {
			t.Errorf("%s: error %q", field, e.Error)
		}
		sess, _ := s.sessionMgr.Get(testCallID + "-tcore")
		if len(f.fallback) != 0 || len(f.urls) != 0 || sess.MediaResources() != nil {
			t.Errorf("%s: invalid ctrl-result partially applied: fallback %v, tasks %v", field, f.fallback, f.urls)
		}

		// Cùng cuộc gọi, bản tin hợp lệ (kể cả callbackUrls rỗng = về URL mặc định) được nhận.
		mr = valid()
		mr.TAccess.CallbackURLs = map[string]string{"video_segment": ""}
		assertStatus(t, postJSON(t, s, ctrlResultPath(testCallID), CtrlResultRequest{CallID: testCallID, MediaResources: mr}), http.StatusOK)
		if f.fallback[testCallID+"-tcore"] != "http://mf/results/tCore" || sess.MediaResources() == nil {
			t.Errorf("%s: valid ctrl-result not applied: %v", field, f.fallback)
		}
	}
}
