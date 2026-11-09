package controlplane

import (
	"bytes"
	"strings"
	"testing"

	"media-ai-gateway/internal/jitter"
	"media-ai-gateway/internal/pipeline"
)

type jitterStatsCoord struct {
	Starter
	st jitter.Stats
}

func (c jitterStatsCoord) VideoJitterStats() jitter.Stats { return c.st }

// /metrics có số liệu jitter overflow và mất gói/chờ IDR của decoder video.
func TestVideoMetrics_LossAndOverflow(t *testing.T) {
	s := &Server{
		coord:     jitterStatsCoord{st: jitter.Stats{OverflowDropped: 3}},
		videoPool: pipeline.NewVideoWorkerPool(pipeline.VideoWorkerPoolConfig{Workers: 1, QueueSize: 1}),
	}
	var buf bytes.Buffer
	s.videoMetricsWrite(&buf)
	out := buf.String()
	for _, line := range []string{
		"media_ai_video_jitter_overflow_dropped_total 3",
		"# TYPE media_ai_video_decoder_packet_gaps_total counter",
		"media_ai_video_decoder_wait_idr_episodes_total 0",
		"media_ai_video_decoder_wait_idr_dropped_frames_total 0",
		"media_ai_video_decoder_wait_idr_ms_total 0",
		"# TYPE media_ai_video_decoder_sessions_waiting_idr gauge",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("missing %q", line)
		}
	}
}
