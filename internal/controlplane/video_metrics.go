package controlplane

import (
	"fmt"
	"io"
)

func (s *Server) videoMetricsWrite(w io.Writer) {
	write := func(name, kind, help string, value uint64) {
		fmt.Fprintf(w, "# HELP media_ai_video_%s %s\n# TYPE media_ai_video_%s %s\nmedia_ai_video_%s %d\n",
			name, help, name, kind, name, value)
	}
	if s.videoPool != nil {
		st := s.videoPool.Stats()
		write("pool_sessions_active", "gauge", "Active video decode pipelines", uint64(s.videoPool.SessionCount()))
		write("pool_queue_len", "gauge", "Video RTP jobs pending", uint64(s.videoPool.QueueLen()))
		write("pool_submitted_total", "counter", "Video RTP jobs submitted", st.Submitted)
		write("pool_processed_total", "counter", "Video RTP jobs processed", st.Processed)
		write("pool_dropped_total", "counter", "Video RTP jobs dropped", st.Dropped)
		write("frames_total", "counter", "JPEG frames enqueued for video AI", st.Frames)
		write("frames_dropped_total", "counter", "Video frames dropped from AI input queue", st.FramesDropped)
		write("decode_errors_total", "counter", "Video decode or JPEG conversion errors", st.DecodeErrors)
	}
	if s.videoAI != nil {
		st := s.videoAI.Stats()
		write("ai_streams_active", "gauge", "Active video AI streams", uint64(s.videoAI.Count()))
		write("ai_frames_sent_total", "counter", "Video frames sent to AI", st.FramesSent)
		write("ai_results_received_total", "counter", "Video results received from AI", st.ResultsReceived)
		write("ai_send_errors_total", "counter", "Video AI send errors", st.SendErrors)
		write("ai_recv_errors_total", "counter", "Video AI receive errors", st.RecvErrors)
		write("ai_result_errors_total", "counter", "Video AI results with missing or unsuccessful masks", st.ResultErrors)
	}
}
