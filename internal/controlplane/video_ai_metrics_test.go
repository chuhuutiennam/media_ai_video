package controlplane

import (
	"bytes"
	"strings"
	"testing"

	"media-ai-gateway/internal/ai"
)

func TestVideoMetrics_AIStreamsStopped(t *testing.T) {
	s := &Server{videoAI: ai.NewVideoManager(ai.Config{MaxActiveStreams: 1}, nil)}
	var buf bytes.Buffer
	s.videoMetricsWrite(&buf)
	for _, line := range []string{
		"# TYPE media_ai_video_ai_streams_stopped_total counter",
		"media_ai_video_ai_streams_stopped_total 0",
	} {
		if !strings.Contains(buf.String(), line) {
			t.Errorf("missing %q", line)
		}
	}
}
