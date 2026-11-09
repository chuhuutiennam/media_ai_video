package coordinator

import (
	"testing"

	"media-ai-gateway/internal/jitter"
)

func TestVideoJitterConfig(t *testing.T) {
	audio := jitter.Config{BufferMs: 60, MaxLateMs: 120, PacketTimeMs: 20}
	video := jitter.Config{BufferMs: 80, MaxLateMs: 200, PacketTimeMs: 20, SkipGaps: true, MaxPackets: 1024}
	for _, tc := range []struct {
		name    string
		cfg     Config
		want    jitter.Config
		wantOut int
	}{
		{"video riêng", Config{JitterConfig: audio, JitterOutSize: 16, VideoJitterConfig: video, VideoJitterOutSize: 1024}, video, 1024},
		{"chưa cấu hình video → như audio", Config{JitterConfig: audio, JitterOutSize: 32}, audio, 32},
		{"không có cỡ hàng đợi → 16", Config{JitterConfig: audio}, audio, 16},
	} {
		c := &Coordinator{cfg: tc.cfg}
		got, out := c.videoJitterConfig()
		if got != tc.want || out != tc.wantOut {
			t.Errorf("%s: got %+v out=%d, want %+v out=%d", tc.name, got, out, tc.want, tc.wantOut)
		}
	}
}
