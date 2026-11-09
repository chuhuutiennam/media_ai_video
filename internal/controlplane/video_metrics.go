package controlplane

import (
	"fmt"
	"io"

	"media-ai-gateway/internal/jitter"
)

func (s *Server) videoMetricsWrite(w io.Writer) {
	write := func(name, kind, help string, value uint64) {
		fmt.Fprintf(w, "# HELP media_ai_video_%s %s\n# TYPE media_ai_video_%s %s\nmedia_ai_video_%s %d\n",
			name, help, name, kind, name, value)
	}
	write("ingress_packets_total", "counter", "Video UDP datagrams read", s.videoIngress.Received.Load())
	write("ingress_parsed_total", "counter", "Valid video RTP datagrams parsed", s.videoIngress.Parsed.Load())
	write("ingress_parse_errors_total", "counter", "Invalid video RTP datagrams", s.videoIngress.ParseErrors.Load())
	write("ingress_queue_dropped_total", "counter", "Video RTP session queue drops", s.videoIngress.QueueDropped.Load())
	write("ingress_rtcp_dropped_total", "counter", "Video RTCP datagrams dropped before decode", s.videoIngress.RTCPDropped.Load())
	if source, ok := s.coord.(interface{ VideoJitterStats() jitter.Stats }); ok {
		st := source.VideoJitterStats()
		write("jitter_input_total", "counter", "Video packets passed to jitter", st.Pushed)
		write("jitter_received_total", "counter", "Video packets admitted to jitter", st.Received)
		write("jitter_released_total", "counter", "Video packets released by jitter", st.Released)
		write("jitter_dropped_total", "counter", "Video jitter packet drops", st.Dropped)
		write("jitter_lost_total", "counter", "Video missing sequence numbers declared lost", st.Lost)
		write("jitter_late_dropped_total", "counter", "Video packets rejected as too old on Push", st.LateDropped)
		write("jitter_duplicate_dropped_total", "counter", "Video old or duplicate packet drops", st.DuplicateDropped)
		write("jitter_output_dropped_total", "counter", "Video packets dropped at full jitter output", st.OutputDropped)
		write("jitter_overflow_dropped_total", "counter", "Video packets rejected because the jitter buffer held video.jitter.max_packets", st.OverflowDropped)
	}
	if s.videoPool != nil {
		st := s.videoPool.Stats()
		write("pool_sessions_active", "gauge", "Active video decode pipelines", uint64(s.videoPool.SessionCount()))
		write("pool_queue_len", "gauge", "Video RTP jobs pending", uint64(s.videoPool.QueueLen()))
		write("pool_submitted_total", "counter", "Video RTP jobs submitted", st.Submitted)
		write("pool_processed_total", "counter", "Video RTP jobs processed", st.Processed)
		write("pool_dropped_total", "counter", "Video RTP jobs dropped", st.Dropped)
		write("frames_total", "counter", "Task frames enqueued for video AI (JPEG)", st.Frames)
		write("frames_dropped_total", "counter", "Video frames dropped from AI input queue", st.FramesDropped)
		write("decode_errors_total", "counter", "Video decode or JPEG conversion errors", st.DecodeErrors)
		write("decoder_packet_gaps_total", "counter", "Video RTP sequence discontinuities seen by the H264 depacketizer", st.PacketGaps)
		write("decoder_wait_idr_episodes_total", "counter", "Times a video decoder started waiting for an IDR after packet loss", st.WaitIDREpisodes)
		write("decoder_wait_idr_dropped_frames_total", "counter", "Complete video access units discarded while waiting for an IDR", st.WaitIDRDroppedFrames)
		write("decoder_wait_idr_ms_total", "counter", "Milliseconds video decoders waited for an IDR after packet loss (packet arrival time)", st.WaitIDRMsTotal)
		write("decoder_sessions_waiting_idr", "gauge", "Video sessions waiting for an IDR after packet loss", uint64(s.videoPool.WaitingIDRSessions()))
	}
	if s.videoAI != nil {
		st := s.videoAI.Stats()
		write("ai_streams_active", "gauge", "Active video AI streams", uint64(s.videoAI.Count()))
		write("ai_frames_sent_total", "counter", "Video frames sent to AI", st.FramesSent)
		write("ai_results_received_total", "counter", "Video results received from AI", st.ResultsReceived)
		write("gesture_results_received_total", "counter", "Gesture snapshots received from AI", st.GestureResults)
		write("gesture_results_dropped_total", "counter", "Gesture snapshots dropped before shared result queue", st.GestureDropped)
		write("mask_results_dropped_total", "counter", "Mask snapshots dropped before shared result queue", st.MaskDropped)
		write("ai_send_errors_total", "counter", "Video AI send errors", st.SendErrors)
		write("ai_recv_errors_total", "counter", "Video AI receive errors", st.RecvErrors)
		write("ai_streams_stopped_total", "counter", "Video AI streams that stopped for good during a session (stream timeout, permanent error or retries exhausted)", st.StreamsStopped)
		write("ai_result_errors_total", "counter", "Video AI results with missing or unsuccessful masks", st.ResultErrors)
	}
}
