package session

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"media-ai-gateway/internal/pipeline"
)

// Sentinel errors.
var (
	ErrMaxSessions = errors.New("session: max sessions reached")
	ErrDuplicateID = errors.New("session: duplicate session ID")
	ErrNotFound    = errors.New("session: not found")
)

// ManagerConfig giữ cấu hình cho Manager.
type ManagerConfig struct {
	MaxSessions     int
	IdleTimeout     time.Duration
	PacketQueueSize int
	AudioQueueSize  int
	ResultQueueSize int
	GCInterval      time.Duration

	// Video: chỉ dùng khi SessionConfig.VideoEnabled == true.
	VideoPacketQueueSize int // RTP packet (H264) chờ vào decode
	VideoFrameQueueSize  int // JPEG-sampled frame chờ gửi AI
}

// DefaultConfig trả về cấu hình mặc định phù hợp với production.
func DefaultConfig() ManagerConfig {
	return ManagerConfig{
		MaxSessions:     10000,
		IdleTimeout:     30 * time.Second,
		PacketQueueSize: 128,
		AudioQueueSize:  128,
		ResultQueueSize: 64,
		GCInterval:      5 * time.Second,

		VideoPacketQueueSize: 256,
		VideoFrameQueueSize:  3,
	}
}

// SessionConfig giữ tham số để tạo session mới.
type SessionConfig struct {
	ID          string
	SourceType  string // "raw_rtp" | "webrtc"
	SSRC        uint32
	PayloadType uint8
	Codec       string
	SampleRate  int
	Channels    int
	Language    string
	Task        string
	CallbackURL string
	PeerID      string
	TrackID     string
	RemoteAddr  string // expected source UDP addr "ip:port" for raw_rtp addr-based routing

	// Video — set VideoEnabled=true khi call có m=video.
	VideoEnabled     bool
	VideoSSRC        uint32
	VideoPayloadType uint8
	VideoCodec       string
	VideoMaxWidth    int
	VideoMaxHeight   int
	VideoInferWidth  int
	VideoInferHeight int
	VideoInferFPS    int
	EffectType       string
}

// SessionPatch giữ các field có thể cập nhật sau khi session được tạo.
// Field rỗng/nil = không thay đổi.
type SessionPatch struct {
	RemoteAddr     string                   // cập nhật addrIndex khi thay đổi
	CallbackURL    string                   // cập nhật sink qua coordinator
	MediaResources *pipeline.MediaResources // H.248 termination info từ DCAS
}

// Manager quản lý lifecycle của tất cả media session.
//
// Mọi method đều goroutine-safe. Background GC được khởi chạy bởi Run().
type Manager struct {
	cfg            ManagerConfig
	mu             sync.RWMutex
	sessions       map[string]*Session
	ssrcIndex      map[uint32]string // SSRC       → session ID; chỉ chứa SSRC != 0
	videoSSRCIndex map[uint32]string // VideoSSRC  → session ID; chỉ chứa VideoSSRC != 0
	addrIndex      map[string]string // RemoteAddr → session ID; chỉ chứa RemoteAddr != ""
	peerIndex      map[string]string // PeerID     → session ID; chỉ chứa PeerID != ""
	trackIndex     map[string]string // TrackID    → session ID; chỉ chứa TrackID != ""

	createdTotal atomic.Uint64
	closedTotal  atomic.Uint64
}

// NewManager tạo Manager với config cho trước.
func NewManager(cfg ManagerConfig) *Manager {
	return &Manager{
		cfg:            cfg,
		sessions:       make(map[string]*Session),
		ssrcIndex:      make(map[uint32]string),
		videoSSRCIndex: make(map[uint32]string),
		addrIndex:      make(map[string]string),
		peerIndex:      make(map[string]string),
		trackIndex:     make(map[string]string),
	}
}

// Create phân bổ Session mới. Trả về ErrMaxSessions hoặc ErrDuplicateID nếu bị từ chối.
func (m *Manager) Create(cfg SessionConfig) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.sessions) >= m.cfg.MaxSessions {
		return nil, ErrMaxSessions
	}
	if _, exists := m.sessions[cfg.ID]; exists {
		return nil, ErrDuplicateID
	}

	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now()

	s := &Session{
		ID:               cfg.ID,
		SourceType:       cfg.SourceType,
		SSRC:             cfg.SSRC,
		PayloadType:      cfg.PayloadType,
		Codec:            cfg.Codec,
		SampleRate:       cfg.SampleRate,
		Channels:         cfg.Channels,
		Language:         cfg.Language,
		Task:             cfg.Task,
		CallbackURL:      cfg.CallbackURL,
		PeerID:           cfg.PeerID,
		TrackID:          cfg.TrackID,
		RemoteAddr:       cfg.RemoteAddr,
		VideoEnabled:     cfg.VideoEnabled,
		VideoSSRC:        cfg.VideoSSRC,
		VideoPayloadType: cfg.VideoPayloadType,
		VideoCodec:       cfg.VideoCodec,
		VideoMaxWidth:    cfg.VideoMaxWidth,
		VideoMaxHeight:   cfg.VideoMaxHeight,
		VideoInferWidth:  cfg.VideoInferWidth,
		VideoInferHeight: cfg.VideoInferHeight,
		VideoInferFPS:    cfg.VideoInferFPS,
		EffectType:       cfg.EffectType,
		PacketQueue:      make(chan pipeline.MediaPacket, m.cfg.PacketQueueSize),
		AudioQueue:       make(chan pipeline.AudioChunk, m.cfg.AudioQueueSize),
		ResultQueue:      make(chan pipeline.RecognitionResult, m.cfg.ResultQueueSize),
		CreatedAt:        now,
		Ctx:              ctx,
		cancel:           cancel,
		lastPacketAt:     now,
		status:           StatusCreated,
	}
	if cfg.VideoEnabled {
		s.VideoMaskResultQueue = make(chan pipeline.RecognitionResult, m.cfg.ResultQueueSize)
		s.GestureResultQueue = make(chan pipeline.RecognitionResult, m.cfg.ResultQueueSize)
		s.VideoPacketQueue = make(chan pipeline.MediaPacket, m.cfg.VideoPacketQueueSize)
		s.VideoFrameQueue = make(chan pipeline.VideoFrame, m.cfg.VideoFrameQueueSize)
	}

	m.sessions[cfg.ID] = s
	if cfg.SSRC != 0 {
		m.ssrcIndex[cfg.SSRC] = cfg.ID
	}
	if cfg.VideoEnabled && cfg.VideoSSRC != 0 {
		m.videoSSRCIndex[cfg.VideoSSRC] = cfg.ID
	}
	if cfg.RemoteAddr != "" {
		m.addrIndex[cfg.RemoteAddr] = cfg.ID
	}
	if cfg.PeerID != "" {
		m.peerIndex[cfg.PeerID] = cfg.ID
	}
	if cfg.TrackID != "" {
		m.trackIndex[cfg.TrackID] = cfg.ID
	}
	m.createdTotal.Add(1)
	return s, nil
}

// Get trả về session theo ID. ok=false nếu không tìm thấy.
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	return s, ok
}

// GetBySSRC trả về session theo SSRC. ok=false nếu SSRC không được map.
func (m *Manager) GetBySSRC(ssrc uint32) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.ssrcIndex[ssrc]
	if !ok {
		return nil, false
	}
	s, ok := m.sessions[id]
	return s, ok
}

// GetByVideoSSRC trả về session theo Video SSRC. ok=false nếu không được map.
func (m *Manager) GetByVideoSSRC(ssrc uint32) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.videoSSRCIndex[ssrc]
	if !ok {
		return nil, false
	}
	s, ok := m.sessions[id]
	return s, ok
}

// GetByAddr trả về session theo remote UDP addr "ip:port".
// ok=false nếu addr không được map (hoặc addr == "").
func (m *Manager) GetByAddr(addr string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.addrIndex[addr]
	if !ok {
		return nil, false
	}
	s, ok := m.sessions[id]
	return s, ok
}

// GetByPeerID trả về session theo PeerID (WebRTC peer connection ID).
// ok=false nếu không có session nào map với peerID đó.
func (m *Manager) GetByPeerID(peerID string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.peerIndex[peerID]
	if !ok {
		return nil, false
	}
	s, ok := m.sessions[id]
	return s, ok
}

// GetByTrackID trả về session theo TrackID (WebRTC track ID).
// ok=false nếu không có session nào map với trackID đó.
func (m *Manager) GetByTrackID(trackID string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.trackIndex[trackID]
	if !ok {
		return nil, false
	}
	s, ok := m.sessions[id]
	return s, ok
}

// Close đóng session theo ID, cancel Ctx của nó.
// Trả về false nếu session không tồn tại.
func (m *Manager) Close(id string) bool {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return false
	}
	s.setStatus(StatusClosing)
	delete(m.sessions, id)
	if s.SSRC != 0 {
		delete(m.ssrcIndex, s.SSRC)
	}
	if s.VideoEnabled && s.VideoSSRC != 0 {
		delete(m.videoSSRCIndex, s.VideoSSRC)
	}
	if s.RemoteAddr != "" {
		delete(m.addrIndex, s.RemoteAddr)
	}
	if s.PeerID != "" {
		delete(m.peerIndex, s.PeerID)
	}
	if s.TrackID != "" {
		delete(m.trackIndex, s.TrackID)
	}
	m.mu.Unlock()

	s.cancel()
	s.setStatus(StatusClosed)
	m.closedTotal.Add(1)
	return true
}

// Update cập nhật các field của session theo patch. Trả về ErrNotFound nếu session không tồn tại.
// Goroutine-safe.
func (m *Manager) Update(id string, patch SessionPatch) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, ErrNotFound
	}
	if patch.RemoteAddr != "" && patch.RemoteAddr != s.RemoteAddr {
		if s.RemoteAddr != "" {
			delete(m.addrIndex, s.RemoteAddr)
		}
		s.RemoteAddr = patch.RemoteAddr
		m.addrIndex[patch.RemoteAddr] = id
	}
	if patch.CallbackURL != "" {
		s.CallbackURL = patch.CallbackURL
	}
	if patch.MediaResources != nil {
		s.MediaResources = patch.MediaResources
	}
	return s, nil
}

// Count trả về số session đang active.
func (m *Manager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// MaxSessions trả về giới hạn session tối đa (dùng để tính % sử dụng).
func (m *Manager) MaxSessions() int { return m.cfg.MaxSessions }

// CreatedTotal trả về tổng số session đã được tạo kể từ khi khởi động.
func (m *Manager) CreatedTotal() uint64 { return m.createdTotal.Load() }

// ClosedTotal trả về tổng số session đã bị đóng kể từ khi khởi động.
func (m *Manager) ClosedTotal() uint64 { return m.closedTotal.Load() }

// TotalPacketQueueDropped tổng hợp PacketQueueDropped từ tất cả session đang active.
func (m *Manager) TotalPacketQueueDropped() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var total uint64
	for _, s := range m.sessions {
		total += s.PacketQueueDropped.Load()
	}
	return total
}

// TotalVideoPacketQueueDropped tổng hợp VideoPacketQueueDropped từ tất cả session đang active.
func (m *Manager) TotalVideoPacketQueueDropped() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var total uint64
	for _, s := range m.sessions {
		total += s.VideoPacketQueueDropped.Load()
	}
	return total
}

// Run khởi động vòng lặp GC nền, kiểm tra idle timeout theo GCInterval.
// Blocks cho đến khi ctx bị cancel.
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(m.cfg.GCInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			m.gc(t)
		}
	}
}

// gc thu hồi các session đã idle quá IdleTimeout.
// Dùng RLock để scan, sau đó Close() từng session (Close dùng Lock riêng).
func (m *Manager) gc(now time.Time) {
	m.mu.RLock()
	var expired []string
	for id, s := range m.sessions {
		if s.isIdle(now, m.cfg.IdleTimeout) {
			expired = append(expired, id)
		}
	}
	m.mu.RUnlock()

	for _, id := range expired {
		m.Close(id)
	}
}
