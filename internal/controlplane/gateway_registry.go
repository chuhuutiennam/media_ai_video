package controlplane

import (
	"sync"
	"time"
)

// GatewayInfo mô tả trạng thái tài nguyên của một gateway node.
// Được cập nhật định kỳ qua heartbeat hoặc khi node tự đăng ký.
type GatewayInfo struct {
	ID              string  `json:"id"`
	Addr            string  `json:"addr"`             // HTTP/2 control plane address
	RTPPublicIP     string  `json:"rtp_public_ip,omitempty"`
	Sessions        int     `json:"sessions"`
	MaxSessions     int     `json:"max_sessions"`
	AIStreams        int     `json:"ai_streams"`
	MaxAIStreams     int     `json:"max_ai_streams"`
	CPUUsage        float64 `json:"cpu_usage,omitempty"`        // 0..1
	AudioQueueUsage float64 `json:"audio_queue_usage,omitempty"` // 0..1
	RTPPortStart    int     `json:"rtp_port_start,omitempty"`
	RTPPortEnd      int     `json:"rtp_port_end,omitempty"`
	UsedPorts       int     `json:"used_ports,omitempty"`
	LastHeartbeatMs int64   `json:"last_heartbeat_ms,omitempty"` // Unix ms reported by sender
	UpdatedAt       time.Time `json:"-"`                         // set server-side by Register()
}

// sessionLoad trả về tỷ lệ sessions / maxSessions (0..1).
func (g GatewayInfo) sessionLoad() float64 {
	if g.MaxSessions == 0 {
		return 1
	}
	return float64(g.Sessions) / float64(g.MaxSessions)
}

// GatewayRegistry lưu trữ danh sách các gateway nodes và trạng thái của chúng.
// Thread-safe. Các node không cập nhật trong TTL sẽ bị ẩn khỏi List.
type GatewayRegistry struct {
	mu    sync.RWMutex
	nodes map[string]GatewayInfo
	ttl   time.Duration // 0 = không expire
}

// NewGatewayRegistry tạo registry mới.
// ttl là thời gian tối đa một node có thể không cập nhật trước khi bị coi là stale.
// ttl = 0 vô hiệu hoá expire (dùng cho single-node hoặc test).
func NewGatewayRegistry(ttl time.Duration) *GatewayRegistry {
	return &GatewayRegistry{
		nodes: make(map[string]GatewayInfo),
		ttl:   ttl,
	}
}

// Register thêm hoặc cập nhật thông tin của một gateway node.
func (r *GatewayRegistry) Register(info GatewayInfo) {
	info.UpdatedAt = time.Now()
	r.mu.Lock()
	r.nodes[info.ID] = info
	r.mu.Unlock()
}

// Deregister xóa gateway node khỏi registry.
func (r *GatewayRegistry) Deregister(id string) {
	r.mu.Lock()
	delete(r.nodes, id)
	r.mu.Unlock()
}

// Get trả về thông tin của gateway node theo ID.
func (r *GatewayRegistry) Get(id string) (GatewayInfo, bool) {
	r.mu.RLock()
	info, ok := r.nodes[id]
	r.mu.RUnlock()
	return info, ok
}

// List trả về tất cả gateway nodes còn fresh (trong TTL).
// Thứ tự không được đảm bảo.
func (r *GatewayRegistry) List() []GatewayInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	now := time.Now()
	out := make([]GatewayInfo, 0, len(r.nodes))
	for _, info := range r.nodes {
		if r.ttl > 0 && now.Sub(info.UpdatedAt) > r.ttl {
			continue // stale
		}
		out = append(out, info)
	}
	return out
}

// Len trả về số node đang được theo dõi (bao gồm cả stale).
func (r *GatewayRegistry) Len() int {
	r.mu.RLock()
	n := len(r.nodes)
	r.mu.RUnlock()
	return n
}
