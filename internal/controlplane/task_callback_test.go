package controlplane

import (
	"net/http"
	"testing"

	"media-ai-gateway/internal/result"
)

type taskCallbackStarter struct {
	fakeStarter
	urls map[string]string
}

func (f *taskCallbackStarter) UpdateTaskCallbackSink(session, task, url string) *result.HTTPCallbackSink {
	f.urls[session+"/"+task] = url
	return nil
}
func TestCtrlResultTaskCallbackURLs(t *testing.T) {
	f := &taskCallbackStarter{urls: make(map[string]string)}
	s := newTestServer(t, f)
	assertStatus(t, postJSON(t, s, notifyPath(testCallID), validAnswerReq()), http.StatusCreated)
	req := CtrlResultRequest{CallID: testCallID, MediaResources: &MediaResources{
		TCore: MediaResourceInfo{CallbackURL: "http://mf/fallback", CallbackURLs: map[string]string{
			"video_segment": "http://mf/mask/tcore", "gesture_recognition": "http://mf/gesture/tcore",
		}},
		TAccess: MediaResourceInfo{CallbackURLs: map[string]string{"gesture_recognition": "http://mf/gesture/taccess"}},
	}}
	assertStatus(t, postJSON(t, s, ctrlResultPath(testCallID), req), http.StatusOK)
	if len(f.urls) != 3 || f.urls[testCallID+"-tcore/video_segment"] != "http://mf/mask/tcore" || f.urls[testCallID+"-taccess/gesture_recognition"] != "http://mf/gesture/taccess" {
		t.Fatal(f.urls)
	}
	req.MediaResources.TCore.CallbackURLs = map[string]string{"gesture_recognition": ""}
	assertStatus(t, postJSON(t, s, ctrlResultPath(testCallID), req), http.StatusOK)
	if f.urls[testCallID+"-tcore/gesture_recognition"] != "" {
		t.Fatal("clear override was not propagated")
	}
	f.urls = make(map[string]string)
	req.MediaResources.TAccess.CallbackURLs = map[string]string{"typo": "http://bad"}
	assertStatus(t, postJSON(t, s, ctrlResultPath(testCallID), req), http.StatusBadRequest)
	if len(f.urls) != 0 {
		t.Fatal("invalid request partially applied")
	}
}
