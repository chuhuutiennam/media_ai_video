package config

import "testing"

func TestGestureKeywordConfigDefaultsOverridesAndValidation(t *testing.T) {
	cfg, err := Load(writeTemp(t, `callback:
  gesture_keywords:
    mode: keyword
    base_url: http://127.0.0.1:9999
    mapping:
      Thumb_Up: {topic_label: EMOTION, topic_info: congrat}
`))
	if err != nil {
		t.Fatal(err)
	}
	k := cfg.ToCoordinatorConfig().GestureKeywords
	if len(k.Mapping) != 8 || k.Mapping["Wave"].TopicInfo != "congrat" || k.Mapping["Thumb_Up"].TopicInfo != "congrat" || k.Mode != "keyword" {
		t.Fatalf("default mapping or override lost: %+v", k)
	}
	for _, invalid := range []string{
		"mode: typo", "base_url: ftp://mf", "base_url: http://mf/results", "cache_size: -1", "cache_size: 0",
		"open_palm_min_score: .nan", "open_palm_hold_ms: -1", "mapping: {Wave: {topic_info: ''}}",
		"mapping: {None: {topic_label: EMOTION, topic_info: none}}",
	} {
		if _, err := Load(writeTemp(t, "callback:\n  gesture_keywords:\n    "+invalid+"\n")); err == nil {
			t.Errorf("accepted %s", invalid)
		}
	}
	if Default().Callback.GestureKeywords.Mode != "legacy" {
		t.Fatal("legacy default changed")
	}
	profile, err := Load("../../config/gateway-video-ims.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !profile.ToDispatcherConfig().OrderedGestures {
		t.Fatal("IMS keyword profile did not enable ordered delivery")
	}
	services := profile.ToVideoServices()
	if len(services) != 2 || services["change_background"].AITask != "video_segment" ||
		services["video_effects"].AITask != "gesture_recognition" || !services["video_effects"].SingleLeg {
		t.Fatal("keyword mapping changed the one-service/one-task catalog", services)
	}
	if profile.ToCoordinatorConfig().CallbackRetryBackoff == 0 {
		t.Fatal("configured callback retry backoff was not propagated")
	}
}
