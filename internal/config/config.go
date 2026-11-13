// Package config quản lý cấu hình runtime cho Media AI Gateway.
//
// Load() đọc YAML config file; Default() trả về giá trị mặc định sẵn sàng cho production.
// Các phương thức ToXxxConfig() chuyển đổi sang config struct của từng component.
//
// Thứ tự ưu tiên: YAML file > Default.
// Field thiếu trong YAML giữ nguyên giá trị Default.
package config

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"media-ai-gateway/internal/ai"
	"media-ai-gateway/internal/controlplane"
	"media-ai-gateway/internal/coordinator"
	"media-ai-gateway/internal/debug/pcap"
	rawrtp "media-ai-gateway/internal/ingress/rawrtp"
	webrtcingress "media-ai-gateway/internal/ingress/webrtc"
	"media-ai-gateway/internal/jitter"
	"media-ai-gateway/internal/pipeline"
	"media-ai-gateway/internal/result"
	"media-ai-gateway/internal/session"
)

// GatewayConfig là cấu hình đầy đủ cho Media AI Gateway.
type GatewayConfig struct {
	Gateway  GatewaySection  `yaml:"gateway"`
	Server   ServerSection   `yaml:"server"`
	RTP      RTPSection      `yaml:"rtp"`
	WebRTC   WebRTCSection   `yaml:"webrtc"`
	Session  SessionSection  `yaml:"session"`
	Pipeline PipelineSection `yaml:"pipeline"`
	Audio    AudioSection    `yaml:"audio"`
	Video    VideoSection    `yaml:"video"`
	AI       AISection       `yaml:"ai"`
	Result   ResultSection   `yaml:"result"`
	Callback CallbackSection `yaml:"callback"`
	DCSF     DCSFSection     `yaml:"dcsf"`
	Log      LogSection      `yaml:"log"`
	PCAP     PCAPSection     `yaml:"pcap"`
}

// DCSFHostConfig mô tả một DCSF endpoint cần pre-warm H/2 connection.
type DCSFHostConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

// DCSFSection cấu hình outbound HTTP/2 pool đến DCSF (CALL_CTRL).
type DCSFSection struct {
	// Hosts là danh sách DCSF endpoint cần tạo pool và pre-warm khi khởi động.
	// callbackUrl trong ANSWER event xác định host:port → chọn pool tương ứng.
	Hosts []DCSFHostConfig `yaml:"hosts"`

	// CallControlTimeoutMs là timeout cho POST /call-control đến DCSF.
	// Cần đủ lớn vì duplicate action khiến DCSF block chờ CALL_RESULT từ IMS-AS.
	CallControlTimeoutMs int `yaml:"call_control_timeout_ms"`
}

// GatewaySection định danh node và bật/tắt ingress protocol.
type GatewaySection struct {
	Name           string `yaml:"name"`
	ID             string `yaml:"id"`
	RawRTPEnabled  bool   `yaml:"raw_rtp_enabled"`
	WebRTCEnabled  bool   `yaml:"webrtc_enabled"`
	MockResultPump bool   `yaml:"mock_result_pump"` // bật giả lập ASR result mỗi 1s (chỉ dùng cho test)
}

// ServerSection cấu hình HTTP/2 control plane.
type ServerSection struct {
	HTTPAddr           string `yaml:"http_addr"`
	PublicURL          string `yaml:"public_url"` // địa chỉ public của DCAS, dùng trong callbackUrl gửi DCSF
	CertFile           string `yaml:"cert_file"`
	KeyFile            string `yaml:"key_file"`
	MetricsAddr        string `yaml:"metrics_addr"` // "" = serve /metrics tại HTTPAddr
	ShutdownTimeoutSec int    `yaml:"shutdown_timeout_sec"`
	// WriteTimeoutSec: thời gian tối đa từ lúc đọc xong request tới lúc ghi xong response
	// (http.Server.WriteTimeout). Response trễ hơn bị bỏ — client không nhận được gì. Mặc định 10.
	WriteTimeoutSec int `yaml:"write_timeout_sec"`
}

// RTPSection cấu hình Raw RTP UDP ingress.
type RTPSection struct {
	ListenAddr       string `yaml:"listen_addr"`
	PublicIP         string `yaml:"public_ip"`
	BindIP           string `yaml:"bind_ip"`
	PortStart        int    `yaml:"port_start"`
	PortEnd          int    `yaml:"port_end"`
	SocketReadBuffer int    `yaml:"socket_read_buffer"`
	// EvenPorts (mặc định true): chỉ cấp cổng chẵn trong [port_start, port_end], cổng lẻ kế
	// tiếp dành cho RTCP (RFC 3550) → số cổng dùng được = một nửa dải; port_start phải chẵn.
	EvenPorts bool `yaml:"even_ports"`
	// PortReuseDelayMs (mặc định 5000): cổng vừa trả (DELETE, idle, CALL_CTRL lỗi) chỉ được cấp
	// lại sau N ms, cổng trả trước được cấp trước (FIFO) — RTP còn đến của cuộc gọi cũ không lọt
	// vào cuộc gọi mới. Cổng đang chờ không tính là trống. 0 = tắt thời gian chờ (vẫn FIFO).
	PortReuseDelayMs int `yaml:"port_reuse_delay_ms"`
}

// WebRTCSection cấu hình WebRTC ingress và ICE.
type WebRTCSection struct {
	Enabled        bool         `yaml:"enabled"`
	STUNServers    []string     `yaml:"stun_servers"`
	TURNServers    []TURNServer `yaml:"turn_servers"`
	NAT1To1IPs     []string     `yaml:"nat1to1_ips"`
	ICEPortMin     uint16       `yaml:"ice_port_min"`
	ICEPortMax     uint16       `yaml:"ice_port_max"`
	DCProxyURL     string       `yaml:"dc_proxy_url"`
	DCUDPProxyAddr string       `yaml:"dc_udp_proxy_addr"`
}

// TURNServer mô tả một TURN server.
type TURNServer struct {
	URLs       []string `yaml:"urls"`
	Username   string   `yaml:"username"`
	Credential string   `yaml:"credential"`
}

// SessionSection giới hạn tài nguyên per-session.
type SessionSection struct {
	MaxSessions           int `yaml:"max_sessions"`
	IdleTimeoutSec        int `yaml:"idle_timeout_sec"`
	GCIntervalSec         int `yaml:"gc_interval_sec"`
	PerSessionPacketQueue int `yaml:"per_session_packet_queue"`
	PerSessionAudioQueue  int `yaml:"per_session_audio_queue"`
	PerSessionResultQueue int `yaml:"per_session_result_queue"`
	PerSessionEgressQueue int `yaml:"per_session_egress_queue"` // egress pacer buffer (frames); 200 frame = 4s tại 20ms/frame
	// Session mới dùng lại ID (callId) của session vừa đóng phải chờ session cũ giải phóng xong
	// tài nguyên (callback, pool, AI stream) — tối đa N ms, quá thì tạo session mới lỗi. 1..60000.
	ReuseWaitMs int `yaml:"reuse_wait_ms"`
}

// PipelineSection cấu hình audio worker pool và jitter buffer.
type PipelineSection struct {
	AudioWorkerCount   int `yaml:"audio_worker_count"`
	AudioJobQueueSize  int `yaml:"audio_job_queue_size"`
	JitterBufferMs     int `yaml:"jitter_buffer_ms"`
	MaxPacketLateMs    int `yaml:"max_packet_late_ms"`
	PacketTimeMs       int `yaml:"packet_time_ms"`
	JitterOutQueueSize int `yaml:"jitter_out_queue_size"` // cap channel jitter buffer → WorkerPool
}

// AudioSection cấu hình output audio format và chunk size.
type AudioSection struct {
	// Enabled (mặc định true): nhận dịch vụ audio (speech_to_text, realtime_translation). false:
	// gateway chỉ video — ANSWER dịch vụ audio trả 400, không tạo session; ai.grpc_target phải rỗng.
	Enabled          bool   `yaml:"enabled"`
	OutputSampleRate int    `yaml:"output_sample_rate"`
	OutputChannels   int    `yaml:"output_channels"`
	ChunkMs          int    `yaml:"chunk_ms"`
	PCMDumpDir       string `yaml:"pcm_dump_dir"` // "" = tắt; "data/output/pcm" = ghi PCM decode ra file để kiểm tra
}

// VideoSection cấu hình video ingest + decode + AI segmentation
type VideoSection struct {
	Gesture           GestureSection `yaml:"gesture"`
	Enabled           bool           `yaml:"enabled"`
	WorkerCount       int            `yaml:"worker_count"`
	JobQueueSize      int            `yaml:"job_queue_size"`
	MaxWidth          int            `yaml:"max_width"`
	MaxHeight         int            `yaml:"max_height"`
	InferWidth        int            `yaml:"infer_width"`
	InferHeight       int            `yaml:"infer_height"`
	InferFPS          int            `yaml:"infer_fps"`
	JPEGQuality       int            `yaml:"jpeg_quality"`
	DefaultEffectType string         `yaml:"default_effect_type"` // "bg_blur" | "bg_replace" | "bg_remove"
	FrameQueueSize    int            `yaml:"frame_queue_size"`
	AI                VideoAISection `yaml:"ai"`

	// Services: danh mục dịch vụ video, mỗi dịch vụ đúng một AI service. Rỗng = config cũ
	// (video.ai.workers / grpc_target / engine_target + video.gesture.enabled).
	Services map[string]VideoServiceSection `yaml:"services"`

	Jitter VideoJitterSection `yaml:"jitter"`
}

// VideoJitterSection: jitter buffer riêng cho RTP video từ MF.
// Enabled=false: video dùng chung pipeline.* và session.per_session_packet_queue với audio,
// thuật toán audio (hạn chờ tính từ lần nhả gần nhất, mỗi seq thiếu chờ một cửa sổ).
type VideoJitterSection struct {
	Enabled         bool `yaml:"enabled"`
	BufferMs        int  `yaml:"buffer_ms"`         // chờ gói đảo thứ tự/thiếu, tính từ lúc gói sau khoảng trống đến
	MaxLateMs       int  `yaml:"max_late_ms"`       // gói nằm trong hàng đợi trước jitter quá lâu thì bỏ
	PacketTimeMs    int  `yaml:"packet_time_ms"`    // chu kỳ flush
	PacketQueueSize int  `yaml:"packet_queue_size"` // listener → jitter, mỗi session
	OutQueueSize    int  `yaml:"out_queue_size"`    // jitter → video worker pool, mỗi session
	MaxPackets      int  `yaml:"max_packets"`       // trần gói chờ trong jitter mỗi session
}

// GestureSection: video.gesture — chỉ dùng cho config cũ và AI Engine (không có video.services).
// Có video.services thì thông số gesture nằm ở video.services.<tên>.gesture.
type GestureSection struct {
	Enabled        bool `yaml:"enabled"`
	GestureProfile `yaml:",inline"`
}

// VideoAISection cấu hình AI Stream Manager cho nhánh video.
type VideoAISection struct {
	GRPCTarget       string `yaml:"grpc_target"`
	MaxActiveStreams int    `yaml:"max_active_streams"`
	StreamTimeoutSec int    `yaml:"stream_timeout_sec"` // 0 (mặc định) = stream AI sống theo cuộc gọi; > 0 = giới hạn cứng
	MaxRetry         int    `yaml:"max_retry"`          // số lần mở lại stream liên tiếp khi lỗi (đặt lại khi có kết quả)
	RetryBackoffMs   int    `yaml:"retry_backoff_ms"`

	EngineTarget       string               `yaml:"engine_target"`
	SourceMaxDimension int                  `yaml:"source_max_dimension"`
	OpenTimeoutMs      int                  `yaml:"open_timeout_ms"`
	SendTimeoutMs      int                  `yaml:"send_timeout_ms"`
	Workers            []VideoWorkerSection `yaml:"workers"` // Explicit task routes; overrides grpc_target when nonempty.

	// Chỉ áp dụng khi chọn AI qua registry (video.services, video.ai.workers, video.ai.grpc_target);
	// không áp dụng video.ai.engine_target.
	Failover            bool `yaml:"failover"`              // mặc định true: bỏ qua target đang lỗi kết nối; mở stream lỗi thì thử target khác cùng dịch vụ
	ReadinessRequiresAI bool `yaml:"readiness_requires_ai"` // mặc định false: /health/ready 503 khi một dịch vụ video đang bật không còn target kết nối được
}

// AISection cấu hình kết nối tới AI worker và gRPC stream.
type AISection struct {
	GRPCTarget          string `yaml:"grpc_target"`
	MaxActiveStreams    int    `yaml:"max_active_streams"`
	PerStreamQueueSize  int    `yaml:"per_stream_queue_size"`
	SendTimeoutMs       int    `yaml:"send_timeout_ms"`
	StreamTimeoutSec    int    `yaml:"stream_timeout_sec"`
	MaxRetry            int    `yaml:"max_retry"`
	RetryBackoffMs      int    `yaml:"retry_backoff_ms"`
	KeepaliveTimeSec    int    `yaml:"keepalive_time_sec"`    // gửi HTTP/2 PING sau N giây idle; 0 = tắt
	KeepaliveTimeoutSec int    `yaml:"keepalive_timeout_sec"` // đóng conn nếu không PONG trong N giây

	// Áp dụng cho mọi kết nối gRPC trong pool: ai.grpc_target, video.ai.engine_target, video.ai.workers.
	WatchReconnect        bool `yaml:"watch_reconnect"`          // theo dõi kết nối: IDLE → nối lại ngay; log lost/restored
	ReconnectMaxBackoffMs int  `yaml:"reconnect_max_backoff_ms"` // chờ tối đa giữa 2 lần gRPC tự nối lại; 0 = mặc định gRPC (120s), hoặc 1000..120000
}

// ResultSection cấu hình Result Dispatcher.
type ResultSection struct {
	DispatcherWorkers   int  `yaml:"dispatcher_workers"` // per task lane (default, video_segment, gesture_recognition)
	QueueSize           int  `yaml:"queue_size"`         // dispatcher capacity per task lane
	DropPartialWhenFull bool `yaml:"drop_partial_when_full"`
	SendTimeoutMs       int  `yaml:"send_timeout_ms"`
}

// CallbackSection cấu hình HTTP callback sink.
type CallbackSection struct {
	GestureKeywords   result.GestureKeywordConfig `yaml:"gesture_keywords"`
	URL               string                      `yaml:"url"` // optional: pre-connect target khi app khởi động
	TimeoutMs         int                         `yaml:"timeout_ms"`
	MaxRetry          int                         `yaml:"max_retry"`
	RetryBackoffMs    int                         `yaml:"retry_backoff_ms"`
	ReadIdleTimeoutMs int                         `yaml:"read_idle_timeout_ms"` // gửi H/2 PING sau N ms idle; 0 = không ping
	PingTimeoutMs     int                         `yaml:"ping_timeout_ms"`      // đóng conn nếu không nhận PONG sau N ms
}

// PCAPSection cấu hình PCAP packet capture (Linux only, AF_PACKET).
// Port range được lấy từ RTP config (rtp.port_start / rtp.port_end).
// enabled: false theo mặc định — chỉ bật khi cần debug.
type PCAPSection struct {
	Enabled   bool   `yaml:"enabled"`
	Interface string `yaml:"interface"`  // e.g. "eth0"
	OutputDir string `yaml:"output_dir"` // e.g. "/var/log/media-ai-gateway/pcap"
	QueueSize int    `yaml:"queue_size"` // bounded capture queue per direction; default 8192
	SnapLen   int    `yaml:"snaplen"`    // max bytes per packet; default 65535
	Rotate    struct {
		MaxSizeMB int `yaml:"max_size_mb"` // rotate when file exceeds this; 0 = no rotation
		MaxFiles  int `yaml:"max_files"`   // keep at most N files per direction; 0 = unlimited
	} `yaml:"rotate"`
}

// LogSection cấu hình logging và periodic monitor.
type LogSection struct {
	Level              string `yaml:"level"`                // debug | info | warn | error
	Format             string `yaml:"format"`               // json | text
	MonitorIntervalSec int    `yaml:"monitor_interval_sec"` // in chu kỳ stats; 0 = tắt
}

// Default trả về GatewayConfig với giá trị mặc định sẵn sàng cho production.
func Default() GatewayConfig {
	return GatewayConfig{
		Gateway: GatewaySection{
			Name:          "media-ai-gateway",
			RawRTPEnabled: true,
			WebRTCEnabled: true,
		},
		Server: ServerSection{
			HTTPAddr:           ":8080",
			MetricsAddr:        "",
			ShutdownTimeoutSec: 10,
			WriteTimeoutSec:    10,
		},
		RTP: RTPSection{
			ListenAddr:       ":5004",
			SocketReadBuffer: 4 * 1024 * 1024,
			EvenPorts:        true,
			PortReuseDelayMs: 5000,
		},
		WebRTC: WebRTCSection{
			Enabled:     true,
			STUNServers: []string{"stun:stun.l.google.com:19302"},
		},
		Session: SessionSection{
			MaxSessions:           10000,
			IdleTimeoutSec:        30,
			GCIntervalSec:         5,
			PerSessionPacketQueue: 128,
			PerSessionAudioQueue:  128,
			PerSessionResultQueue: 64,
			PerSessionEgressQueue: 200, // 200 × 20ms = 4s — đủ cover utterance dài nhất thực tế
			ReuseWaitMs:           2000,
		},
		Pipeline: PipelineSection{
			AudioWorkerCount:   16, // unused — per-session goroutine model; kept for config compat
			AudioJobQueueSize:  32, // per-session job queue (packets đang chờ xử lý per session)
			JitterBufferMs:     60,
			MaxPacketLateMs:    120,
			PacketTimeMs:       20,
			JitterOutQueueSize: 16,
		},
		Audio: AudioSection{
			Enabled:          true,
			OutputSampleRate: 16000,
			OutputChannels:   1,
			ChunkMs:          100,
		},
		Video: VideoSection{
			Gesture:           GestureSection{GestureProfile: GestureProfile{InferFPS: 15, MaxDimension: 640, MaxHands: 4, FrameQueueSize: 2}},
			Enabled:           false,
			WorkerCount:       8,
			JobQueueSize:      1024,
			MaxWidth:          1920,
			MaxHeight:         1080,
			InferWidth:        256,
			InferHeight:       256,
			InferFPS:          5,
			JPEGQuality:       80,
			DefaultEffectType: "bg_blur",
			FrameQueueSize:    3, // queue ngắn, drop-oldest — ưu tiên latency hơn đầy đủ
			AI: VideoAISection{
				SourceMaxDimension: 640,
				OpenTimeoutMs:      5000,
				SendTimeoutMs:      5000,
				MaxActiveStreams:   500, // khớp ai.max_active_video_streams
				StreamTimeoutSec:   0,   // stream sống theo cuộc gọi (300 trước đây cắt AI sau 5 phút)
				MaxRetry:           3,
				RetryBackoffMs:     1000,
				Failover:           true,
			},
			Jitter: VideoJitterSection{
				Enabled:         true,
				BufferMs:        80,
				MaxLateMs:       200,
				PacketTimeMs:    20,
				PacketQueueSize: 1024, // một frame video gồm nhiều gói RTP
				OutQueueSize:    1024, // IDR có thể hàng trăm gói, nhả trong một lần flush
				MaxPackets:      1024,
			},
		},
		AI: AISection{
			GRPCTarget:          "ai-router:50051",
			MaxActiveStreams:    1000,
			PerStreamQueueSize:  20,
			SendTimeoutMs:       500,
			StreamTimeoutSec:    300,
			MaxRetry:            3,
			RetryBackoffMs:      1000,
			KeepaliveTimeSec:    30,
			KeepaliveTimeoutSec: 10,
			WatchReconnect:      true,
		},
		Result: ResultSection{
			DispatcherWorkers:   16,
			QueueSize:           4096,
			DropPartialWhenFull: true,
			SendTimeoutMs:       2000,
		},
		Callback: CallbackSection{
			GestureKeywords:   result.DefaultGestureKeywordConfig(),
			TimeoutMs:         1000,
			MaxRetry:          3,
			RetryBackoffMs:    200,
			ReadIdleTimeoutMs: 30000,
			PingTimeoutMs:     15000,
		},
		DCSF: DCSFSection{
			CallControlTimeoutMs: 30000,
		},
		Log: LogSection{
			Level:              "info",
			Format:             "json",
			MonitorIntervalSec: 60,
		},
	}
}

// validateAnswerDeadline: ANSWER mở stream AI video đồng bộ, mỗi leg chờ tối đa
// video.ai.open_timeout_ms; tổng phải nhỏ hơn server.write_timeout_sec, nếu không response
// (kể cả lỗi) bị HTTP server bỏ và DCSF không nhận được gì. open_timeout_ms = 0: không giới hạn
// (chỉ cho phép khi chọn AI qua registry; Engine bắt buộc >= 1).
func (c GatewayConfig) validateAnswerDeadline() error {
	if c.Server.WriteTimeoutSec < 1 {
		return fmt.Errorf("server.write_timeout_sec must be >= 1, got %d", c.Server.WriteTimeoutSec)
	}
	if c.Video.AI.OpenTimeoutMs < 0 {
		return fmt.Errorf("video.ai.open_timeout_ms must be >= 0, got %d", c.Video.AI.OpenTimeoutMs)
	}
	if !c.Video.Enabled || c.Video.AI.OpenTimeoutMs == 0 {
		return nil
	}
	legs := 1
	for _, s := range c.ToVideoServices() {
		if s.Enabled && !s.SingleLeg {
			legs = 2
		}
	}
	if legs*c.Video.AI.OpenTimeoutMs >= c.Server.WriteTimeoutSec*1000 {
		return fmt.Errorf("video.ai.open_timeout_ms (%d) x video legs (%d) must be less than server.write_timeout_sec (%ds) so ANSWER can reply",
			c.Video.AI.OpenTimeoutMs, legs, c.Server.WriteTimeoutSec)
	}
	return nil
}

// validateVideoJitter kiểm tra video.jitter khi video và jitter riêng đều bật.
func (c GatewayConfig) validateVideoJitter() error {
	j := c.Video.Jitter
	if !c.Video.Enabled || !j.Enabled {
		return nil
	}
	for _, r := range []struct {
		name          string
		value, lo, hi int
	}{
		{"buffer_ms", j.BufferMs, 10, 1000},
		{"max_late_ms", j.MaxLateMs, 20, 5000},
		{"packet_time_ms", j.PacketTimeMs, 5, 100},
		{"packet_queue_size", j.PacketQueueSize, 16, 65536},
		{"out_queue_size", j.OutQueueSize, 16, 65536},
		{"max_packets", j.MaxPackets, 64, 65536},
	} {
		if r.value < r.lo || r.value > r.hi {
			return fmt.Errorf("video.jitter.%s must be %d..%d, got %d", r.name, r.lo, r.hi, r.value)
		}
	}
	if j.PacketTimeMs > j.BufferMs {
		return fmt.Errorf("video.jitter.packet_time_ms (%d) must be <= video.jitter.buffer_ms (%d)", j.PacketTimeMs, j.BufferMs)
	}
	return nil
}

// Load đọc YAML config tại path, merge lên Default().
// Field thiếu trong YAML giữ nguyên giá trị Default.
func Load(path string) (GatewayConfig, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return GatewayConfig{}, fmt.Errorf("config: open %s: %w", path, err)
	}
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&cfg); err != nil {
		return GatewayConfig{}, fmt.Errorf("config: decode %s: %w", path, err)
	}
	if len(cfg.Video.Services) != 0 {
		if err := rejectSharedVideoAIKeys(data); err != nil {
			return GatewayConfig{}, err
		}
	}
	if err := cfg.Callback.GestureKeywords.Validate(); err != nil {
		return GatewayConfig{}, fmt.Errorf("callback.gesture_keywords: %w", err)
	}
	if cfg.RTP.EvenPorts && cfg.RTP.PortStart > 0 && cfg.RTP.PortStart%2 != 0 {
		return GatewayConfig{}, fmt.Errorf("rtp.port_start must be even when rtp.even_ports=true (RTP even, RTCP = RTP+1), got %d", cfg.RTP.PortStart)
	}
	if v := cfg.Session.ReuseWaitMs; v < 1 || v > 60000 {
		return GatewayConfig{}, fmt.Errorf("session.reuse_wait_ms must be 1..60000, got %d", v)
	}
	if cfg.RTP.PortReuseDelayMs < 0 {
		return GatewayConfig{}, fmt.Errorf("rtp.port_reuse_delay_ms must be >= 0, got %d", cfg.RTP.PortReuseDelayMs)
	}
	if cfg.Video.AI.StreamTimeoutSec < 0 || cfg.Video.AI.MaxRetry < 0 {
		return GatewayConfig{}, fmt.Errorf("video.ai.stream_timeout_sec and video.ai.max_retry must be >= 0, got %d, %d",
			cfg.Video.AI.StreamTimeoutSec, cfg.Video.AI.MaxRetry)
	}
	if v := cfg.AI.ReconnectMaxBackoffMs; v != 0 && (v < 1000 || v > 120000) {
		return GatewayConfig{}, fmt.Errorf("ai.reconnect_max_backoff_ms must be 0 (gRPC default) or 1000..120000, got %d", v)
	}
	if err := cfg.normalizeVideoServices(); err != nil {
		return GatewayConfig{}, err
	}
	if err := cfg.normalizeVideoWorkers(); err != nil {
		return GatewayConfig{}, err
	}
	if cfg.Video.AI.ReadinessRequiresAI && (!cfg.Video.Enabled || cfg.Video.AI.EngineTarget != "" || len(cfg.ToVideoWorkers()) == 0) {
		return GatewayConfig{}, fmt.Errorf("video.ai.readiness_requires_ai requires video.enabled=true and AI targets from video.services, video.ai.workers or video.ai.grpc_target (not video.ai.engine_target)")
	}
	if err := cfg.validateAnswerDeadline(); err != nil {
		return GatewayConfig{}, err
	}
	if err := cfg.validateVideoJitter(); err != nil {
		return GatewayConfig{}, err
	}
	if !cfg.Audio.Enabled && strings.TrimSpace(cfg.AI.GRPCTarget) != "" {
		return GatewayConfig{}, fmt.Errorf("audio.enabled=false: ai.grpc_target (audio AI) must be empty, got %q", cfg.AI.GRPCTarget)
	}
	if cfg.Video.Gesture.Enabled {
		hasGestureWorker := cfg.Video.AI.EngineTarget != ""
		for _, worker := range cfg.ToVideoWorkers() {
			for _, task := range worker.Tasks {
				if task == "gesture_recognition" {
					hasGestureWorker = true
				}
			}
		}
		if !cfg.Video.Enabled || !hasGestureWorker {
			return GatewayConfig{}, fmt.Errorf("gesture requires video.enabled and a gesture worker in video.ai.workers or video.ai.grpc_target")
		}
	}
	// Config cũ / AI Engine: giới hạn video.gesture khi bật gesture. Có video.services thì
	// normalizeVideoServices kiểm tra thông số gesture của từng dịch vụ.
	if cfg.Video.Gesture.Enabled {
		if err := cfg.Video.Gesture.GestureProfile.validate("video.gesture"); err != nil {
			return GatewayConfig{}, err
		}
	}
	return cfg, nil
}

// ── Conversion helpers ────────────────────────────────────────────────────────

// ToSessionManagerConfig chuyển đổi sang session.ManagerConfig.
func (c GatewayConfig) ToSessionManagerConfig() session.ManagerConfig {
	return session.ManagerConfig{
		MaxSessions:          c.Session.MaxSessions,
		PacketQueueSize:      c.Session.PerSessionPacketQueue,
		AudioQueueSize:       c.Session.PerSessionAudioQueue,
		ResultQueueSize:      c.Session.PerSessionResultQueue,
		IdleTimeout:          time.Duration(c.Session.IdleTimeoutSec) * time.Second,
		GCInterval:           time.Duration(c.Session.GCIntervalSec) * time.Second,
		VideoPacketQueueSize: c.videoPacketQueueSize(),
		VideoFrameQueueSize:  c.Video.FrameQueueSize,
	}
}

// videoPacketQueueSize: hàng đợi listener → jitter của video; dùng chung với audio khi tắt video.jitter.
func (c GatewayConfig) videoPacketQueueSize() int {
	if c.Video.Jitter.Enabled {
		return c.Video.Jitter.PacketQueueSize
	}
	return c.Session.PerSessionPacketQueue
}

// videoJitter trả cấu hình jitter + cỡ hàng đợi ra của video; tắt video.jitter thì giống audio.
func (c GatewayConfig) videoJitter(audio jitter.Config) (jitter.Config, int) {
	j := c.Video.Jitter
	if !j.Enabled {
		return audio, c.Pipeline.JitterOutQueueSize
	}
	return jitter.Config{
		BufferMs:     j.BufferMs,
		MaxLateMs:    j.MaxLateMs,
		PacketTimeMs: j.PacketTimeMs,
		SkipGaps:     true,
		MaxPackets:   j.MaxPackets,
	}, j.OutQueueSize
}

// ToWorkerPoolConfig chuyển đổi sang pipeline.WorkerPoolConfig.
func (c GatewayConfig) ToWorkerPoolConfig() pipeline.WorkerPoolConfig {
	return pipeline.WorkerPoolConfig{
		Workers:   c.Pipeline.AudioWorkerCount,
		QueueSize: c.Pipeline.AudioJobQueueSize,
	}
}

// ToAIConfig chuyển đổi sang ai.Config.
func (c GatewayConfig) ToAIConfig() ai.Config {
	return ai.Config{
		MaxActiveStreams: c.AI.MaxActiveStreams,
		QueueSize:        c.AI.PerStreamQueueSize,
		SendTimeout:      time.Duration(c.AI.SendTimeoutMs) * time.Millisecond,
		StreamTimeout:    time.Duration(c.AI.StreamTimeoutSec) * time.Second,
		MaxRetries:       c.AI.MaxRetry,
		RetryBackoff:     time.Duration(c.AI.RetryBackoffMs) * time.Millisecond,
	}
}

// ToDispatcherConfig chuyển đổi sang result.Config.
func (c GatewayConfig) ToDispatcherConfig() result.Config {
	return result.Config{
		OrderedGestures:     c.Callback.GestureKeywords.Mode == "keyword",
		Workers:             c.Result.DispatcherWorkers,
		QueueSize:           c.Result.QueueSize,
		DropPartialWhenFull: c.Result.DropPartialWhenFull,
		SendTimeout:         time.Duration(c.Result.SendTimeoutMs) * time.Millisecond,
	}
}

// ToCoordinatorConfig chuyển đổi sang coordinator.Config.
func (c GatewayConfig) ToCoordinatorConfig() coordinator.Config {
	audioJitter := jitter.Config{
		BufferMs:     c.Pipeline.JitterBufferMs,
		MaxLateMs:    c.Pipeline.MaxPacketLateMs,
		PacketTimeMs: c.Pipeline.PacketTimeMs,
	}
	videoJitter, videoJitterOut := c.videoJitter(audioJitter)
	return coordinator.Config{
		JitterConfig:         audioJitter,
		JitterOutSize:        c.Pipeline.JitterOutQueueSize,
		OutSampleRate:        c.Audio.OutputSampleRate,
		OutChannels:          c.Audio.OutputChannels,
		ChunkMs:              c.Audio.ChunkMs,
		CallbackTimeout:      time.Duration(c.Callback.TimeoutMs) * time.Millisecond,
		CallbackRetry:        c.Callback.MaxRetry,
		CallbackRetryBackoff: time.Duration(c.Callback.RetryBackoffMs) * time.Millisecond,
		GestureKeywords:      c.Callback.GestureKeywords,
		PCMDumpDir:           c.Audio.PCMDumpDir,
		SessionReuseWait:     time.Duration(c.Session.ReuseWaitMs) * time.Millisecond,

		VideoJitterConfig: videoJitter, VideoJitterOutSize: videoJitterOut,
		VideoMaxWidth: c.Video.MaxWidth, VideoMaxHeight: c.Video.MaxHeight,
		VideoInferWidth: c.Video.InferWidth, VideoInferHeight: c.Video.InferHeight,
		VideoInferFPS:    c.Video.InferFPS,
		VideoJPEGQuality: c.Video.JPEGQuality,
		VideoEffectType:  c.Video.DefaultEffectType,
		GestureEnabled:   c.gestureInUse(), GestureFPS: c.Video.Gesture.InferFPS,
		GestureMaxDimension: c.Video.Gesture.MaxDimension, GestureMaxHands: c.Video.Gesture.MaxHands, GestureQueueSize: c.Video.Gesture.FrameQueueSize,
		VideoEngine: c.Video.AI.EngineTarget != "", VideoSourceMaxDimension: c.Video.AI.SourceMaxDimension,

		VideoServiceProfiles: c.videoServiceProfiles(),
	}
}

// ToVideoWorkerPoolConfig chuyển đổi sang pipeline.VideoWorkerPoolConfig.
func (c GatewayConfig) ToVideoWorkerPoolConfig() pipeline.VideoWorkerPoolConfig {
	return pipeline.VideoWorkerPoolConfig{
		Workers:   c.Video.WorkerCount,
		QueueSize: c.Video.JobQueueSize,
	}
}

// ToVideoAIConfig chuyển đổi sang ai.Config dùng cho ai.NewVideoManager.
// Tái dùng đúng struct ai.Config với audio (field ý nghĩa chung), chỉ khác
// giá trị — capacity của nhánh video nên đặt độc lập trong video.ai.* của YAML.
func (c GatewayConfig) ToVideoAIConfig() ai.Config {
	cfg := ai.Config{
		MaxActiveStreams: c.Video.AI.MaxActiveStreams,
		StreamTimeout:    time.Duration(c.Video.AI.StreamTimeoutSec) * time.Second,
		MaxRetries:       c.Video.AI.MaxRetry,
		RetryBackoff:     time.Duration(c.Video.AI.RetryBackoffMs) * time.Millisecond,
	}
	if c.Video.AI.EngineTarget != "" {
		cfg.StreamTimeout = 0
		cfg.SendTimeout = time.Duration(c.Video.AI.SendTimeoutMs) * time.Millisecond
		// A source may wait for RTP or pause while audio/call control stays
		// active. Worker progress is timed by the engine only while inference
		// is outstanding; an unconditional receive timer would kill idle video.
		cfg.RecvIdleTimeout = 0
	}
	return cfg
}

// ToGRPCKeepalive trả về keepalive durations cho SharedConnPool.
func (c GatewayConfig) ToGRPCKeepalive() (keepaliveTime, keepaliveTimeout time.Duration) {
	return time.Duration(c.AI.KeepaliveTimeSec) * time.Second,
		time.Duration(c.AI.KeepaliveTimeoutSec) * time.Second
}

// ToGRPCReconnectMaxBackoff trả về thời gian chờ tối đa giữa các lần gRPC tự nối lại;
// 0 = giữ mặc định gRPC (120s).
func (c GatewayConfig) ToGRPCReconnectMaxBackoff() time.Duration {
	return time.Duration(c.AI.ReconnectMaxBackoffMs) * time.Millisecond
}

// ToCallbackHTTPClient tạo shared CallbackHTTPClient từ config.
func (c GatewayConfig) ToCallbackHTTPClient() *result.CallbackHTTPClient {
	return result.NewCallbackHTTPClient(
		time.Duration(c.Callback.TimeoutMs)*time.Millisecond,
		time.Duration(c.Callback.ReadIdleTimeoutMs)*time.Millisecond,
		time.Duration(c.Callback.PingTimeoutMs)*time.Millisecond,
	)
}

// ToControlPlaneServerConfig chuyển đổi sang controlplane.ServerConfig.
func (c GatewayConfig) ToControlPlaneServerConfig() controlplane.ServerConfig {
	return controlplane.ServerConfig{
		Addr:                   c.Server.HTTPAddr,
		PublicURL:              c.Server.PublicURL,
		CertFile:               c.Server.CertFile,
		KeyFile:                c.Server.KeyFile,
		ReadTimeout:            10 * time.Second,
		WriteTimeout:           time.Duration(c.Server.WriteTimeoutSec) * time.Second,
		IdleTimeout:            60 * time.Second,
		GatewayID:              c.Gateway.ID,
		RTPPublicIP:            c.RTP.PublicIP,
		RTPBindIP:              c.RTP.BindIP,
		RTPPortStart:           c.RTP.PortStart,
		RTPPortEnd:             c.RTP.PortEnd,
		DCSFCallControlTimeout: time.Duration(c.DCSF.CallControlTimeoutMs) * time.Millisecond,
		ResultCallbackURL:      c.Callback.URL,
		WebRTCEnabled:          c.Gateway.WebRTCEnabled,
		WebRTC:                 c.ToWebRTCConfig(),
		MockResultPump:         c.Gateway.MockResultPump,
		EgressQueueFrames:      c.Session.PerSessionEgressQueue,
		VideoEnabled:           c.Video.Enabled,
		VideoServices:          c.ToVideoServices(),

		VideoReadinessRequiresAI: c.Video.AI.ReadinessRequiresAI,
		RTPEvenPorts:             c.RTP.EvenPorts,
		RTPPortReuseDelay:        time.Duration(c.RTP.PortReuseDelayMs) * time.Millisecond,
		AudioDisabled:            !c.Audio.Enabled,
	}
}

// ToRTPIngressConfig chuyển đổi sang rawrtp.IngressConfig.
func (c GatewayConfig) ToRTPIngressConfig() rawrtp.IngressConfig {
	return rawrtp.IngressConfig{
		Addr:            c.RTP.ListenAddr,
		MaxPacket:       1500,
		ReadBufferBytes: c.RTP.SocketReadBuffer,
	}
}

// ToDCSFAddrs trả về danh sách "host:port" từ cấu hình DCSF hosts.
// Dùng để tạo DCSFPool trong controlplane.
func (c GatewayConfig) ToDCSFAddrs() []string {
	addrs := make([]string, 0, len(c.DCSF.Hosts))
	for _, h := range c.DCSF.Hosts {
		addrs = append(addrs, fmt.Sprintf("%s:%d", h.Host, h.Port))
	}
	return addrs
}

// ToPCAPConfig chuyển đổi sang pcap.Config.
// PortMin/PortMax lấy từ RTP config để capture đúng port pool của gateway.
func (c GatewayConfig) ToPCAPConfig() pcap.Config {
	return pcap.Config{
		Interface: c.PCAP.Interface,
		OutputDir: c.PCAP.OutputDir,
		PortMin:   uint16(c.RTP.PortStart), //nolint:gosec
		PortMax:   uint16(c.RTP.PortEnd),   //nolint:gosec
		QueueSize: c.PCAP.QueueSize,
		SnapLen:   uint32(c.PCAP.SnapLen), //nolint:gosec
		Rotate: pcap.RotateConfig{
			MaxSizeMB: c.PCAP.Rotate.MaxSizeMB,
			MaxFiles:  c.PCAP.Rotate.MaxFiles,
		},
	}
}

// ToWebRTCConfig chuyển đổi sang webrtcingress.Config.
func (c GatewayConfig) ToWebRTCConfig() webrtcingress.Config {
	cfg := webrtcingress.Config{
		NAT1To1IPs:     c.WebRTC.NAT1To1IPs,
		ICEPortMin:     c.WebRTC.ICEPortMin,
		ICEPortMax:     c.WebRTC.ICEPortMax,
		DCProxyURL:     c.WebRTC.DCProxyURL,
		DCUDPProxyAddr: c.WebRTC.DCUDPProxyAddr,
	}
	for _, u := range c.WebRTC.STUNServers {
		cfg.ICEServers = append(cfg.ICEServers, webrtcingress.ICEServer{URLs: []string{u}})
	}
	for _, t := range c.WebRTC.TURNServers {
		cfg.ICEServers = append(cfg.ICEServers, webrtcingress.ICEServer{
			URLs:       t.URLs,
			Username:   t.Username,
			Credential: t.Credential,
		})
	}
	return cfg
}
