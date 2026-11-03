// Package monitor cung cấp goroutine định kỳ in connection info, metrics, và AI latency.
package monitor

import (
	"context"
	"time"

	"go.uber.org/zap"

	"media-ai-gateway/internal/ai"
	"media-ai-gateway/internal/pipeline"
	"media-ai-gateway/internal/result"
	"media-ai-gateway/internal/session"
)

// PortAllocator is the minimal interface for reading per-session RTP port pool state.
// Implemented by *controlplane.PortAllocator.
type PortAllocator interface {
	Total() int
	Available() int
}

// Monitor periodically logs connection info, key metrics, and AI latency via zap.
// All pointer fields are optional — pass nil to skip that subsystem.
type Monitor struct {
	Interval       time.Duration
	SessionMgr     *session.Manager
	Pool           *pipeline.WorkerPool
	AIMgr          *ai.Manager
	Dispatcher     *result.Dispatcher
	GRPCPool       *ai.SharedConnPool         // optional; nil if no gRPC target configured
	CallbackClient *result.CallbackHTTPClient // optional; nil if callback not configured
	PortAlloc      PortAllocator              // optional; nil if per-session port pool not configured
	SharedIngress  bool                       // true if shared UDP ingress is running
}

// Run starts the periodic logger. Emits one line immediately, then on each tick.
// Returns immediately if Interval <= 0. Blocks until ctx is cancelled.
func (m *Monitor) Run(ctx context.Context) {
	if m.Interval <= 0 {
		return
	}
	m.emit()
	t := time.NewTicker(m.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.emit()
		}
	}
}

func (m *Monitor) emit() {
	fields := make([]zap.Field, 0, 32)

	// Sessions
	if m.SessionMgr != nil {
		fields = append(fields,
			zap.Int("sessions_active", m.SessionMgr.Count()),
			zap.Int("sessions_max",    m.SessionMgr.MaxSessions()),
		)
	}

	// AI streams + latency
	if m.AIMgr != nil {
		st := m.AIMgr.Stats()
		fields = append(fields,
			zap.Int("ai_streams_active",       m.AIMgr.Count()),
			zap.Int("ai_streams_max",          m.AIMgr.MaxStreams()),
			zap.Uint64("ai_send_errors",       st.TotalSendErrors),
			zap.Uint64("ai_recv_errors",       st.TotalRecvErrors),
			zap.Uint64("ai_retries",           st.TotalRetries),
			zap.Int64("ai_first_result_avg_ms", st.AvgFirstResultMs()),
		)
	}

	// Audio worker pool
	if m.Pool != nil {
		ps := m.Pool.Stats()
		fields = append(fields,
			zap.Uint64("pool_submitted",     ps.Submitted),
			zap.Uint64("pool_processed",     ps.Processed),
			zap.Uint64("pool_dropped",       ps.Dropped),
			zap.Uint64("pool_decode_errors", ps.DecodeErrors),
			zap.Int("pool_queue_len",        m.Pool.QueueLen()),
		)
	}

	// Result dispatcher
	if m.Dispatcher != nil {
		ds := m.Dispatcher.Stats()
		fields = append(fields,
			zap.Uint64("disp_sent",         ds.Sent),
			zap.Uint64("disp_sent_final",   ds.SentFinal),
			zap.Uint64("disp_sent_partial", ds.SentPartial),
			zap.Uint64("disp_dropped",      ds.Dropped),
			zap.Uint64("disp_send_errors",  ds.SendErrors),
			zap.Int("disp_queue_len",       m.Dispatcher.QueueLen()),
		)
	}

	// AI gRPC connection state (one entry per worker address)
	if m.GRPCPool != nil {
		for _, addr := range m.GRPCPool.Addrs() {
			fields = append(fields,
				zap.String("ai_grpc_addr",  addr),
				zap.String("ai_grpc_state", m.GRPCPool.State(addr).String()),
			)
		}
	}

	// Callback H/2 preconnect state
	if m.CallbackClient != nil {
		st := m.CallbackClient.PreconnectState()
		fields = append(fields,
			zap.String("callback_url",       st.URL),
			zap.Bool("callback_connected",   st.Connected),
		)
		if st.Error != "" {
			fields = append(fields, zap.String("callback_error", st.Error))
		}
	}

	// RTP port pool
	if m.PortAlloc != nil {
		total := m.PortAlloc.Total()
		avail := m.PortAlloc.Available()
		fields = append(fields,
			zap.Int("rtp_ports_open",     total-avail),
			zap.Int("rtp_ports_capacity", total),
		)
	}
	fields = append(fields, zap.Bool("rtp_shared_ingress", m.SharedIngress))

	zap.L().Info("monitor.stats", fields...)
}
