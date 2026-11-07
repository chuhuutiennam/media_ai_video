// Package controlplane là HTTP/2 control plane của Media AI Gateway.
//
// Hai chế độ hoạt động:
//   - h2c (HTTP/2 cleartext): CertFile bỏ trống, phù hợp cho backend sau proxy TLS.
//   - TLS + HTTP/2:           CertFile + KeyFile được cấu hình, ALPN tự negotiate h2.
//
// Routes:
//
//	POST   /v1/vonras/call-sessions/{callId}/notify-event — BEGIN: ACK; ANSWER: tạo session + cấp RTP port
//	POST   /v1/vonras/call-sessions/{callId}/ctrl-result  — cập nhật mediaResources + callbackUrl sau SDP negotiation
//	DELETE /v1/vonras/call-sessions/{callId}              — đóng session (RELEASE)
//	GET    /v1/vonras/call-sessions/{callId}              — lấy thông tin session (debug)
//	POST   /v1/webrtc/offer          — WebRTC SDP offer/answer (source_type=webrtc)
//	PUT    /v1/gateways/{id}/heartbeat — gateway node gửi trạng thái định kỳ
//	GET    /health/live              — liveness probe (luôn 200 nếu process chạy)
//	GET    /health/ready             — readiness probe (503 nếu quá tải)
//	GET    /health                   — tương đương /health/live (backward compat)
//	GET    /metrics                  — Prometheus-compatible metrics scrape
//	GET    /v1/stats                 — aggregate stats (JSON)
//	GET    /v1/connections           — trạng thái kết nối AI gRPC, callback H/2, RTP ports
package controlplane

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"media-ai-gateway/internal/ai"
	"media-ai-gateway/internal/ingress/rawrtp"
	wrtc "media-ai-gateway/internal/ingress/webrtc"
	"media-ai-gateway/internal/pipeline"
	"media-ai-gateway/internal/result"
	"media-ai-gateway/internal/session"
)

const defaultRegistryTTL = 30 * time.Second

// Starter là interface được implement bởi *coordinator.Coordinator.
type Starter interface {
	Start(sess *session.Session) (*result.HTTPCallbackSink, error)
	UpdateCallbackSink(sessID, newURL string) *result.HTTPCallbackSink
	StartMockResultPump(sess *session.Session, terminationID string)
	StartVideo(sess *session.Session) error
}

// ServerConfig cấu hình HTTP/2 control plane.
type ServerConfig struct {
	Addr         string // ví dụ: ":8080"
	CertFile     string // PEM cert; nếu rỗng → dùng h2c
	KeyFile      string // PEM key; nếu rỗng → dùng h2c
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration

	// Identity
	GatewayID string // returned in CREATE response so clients know which node

	// Per-session RTP port allocation (§5.4).
	// If RTPPortStart == 0, port allocation is disabled (shared ingress only).
	RTPPublicIP  string // IP advertised to the caller (may differ from bind IP)
	RTPBindIP    string // local IP to bind per-session UDP sockets; "" = all interfaces
	RTPPortStart int    // inclusive lower bound of the port pool
	RTPPortEnd   int    // inclusive upper bound of the port pool

	// RegistryTTL là thời gian tối đa một gateway node được coi là fresh sau lần heartbeat cuối.
	// 0 → sử dụng mặc định 30s.
	RegistryTTL time.Duration

	// DCSFCallControlTimeout là timeout cho POST CALL_CTRL đến DCSF sau khi xử lý ANSWER.
	// 0 → dùng mặc định 30s. Cần đủ lớn vì duplicate action khiến DCSF block chờ CALL_RESULT từ IMS-AS.
	DCSFCallControlTimeout time.Duration

	// PublicURL là địa chỉ public của DCAS (ví dụ: "http://10.0.0.1:8080").
	// Dùng để xây callbackUrl trong CALL_CTRL body để DCSF biết gửi ctrl-result về đâu.
	PublicURL string

	// ResultCallbackURL là URL global để gửi kết quả transcript (recognition result).
	// Dùng làm default callback sink cho tất cả DCSF session khi không có per-session URL.
	ResultCallbackURL string

	// WebRTCEnabled bật/tắt WebRTC ingress. false → POST /v1/webrtc/offer trả 404.
	WebRTCEnabled bool

	// VideoEnabled bật/tắt xử lý video. false → dịch vụ video trả 400.
	VideoEnabled bool

	// VideoServices là danh mục dịch vụ video (key chữ thường = selectedService),
	// mỗi dịch vụ đúng một AI task. Nguồn: video.services hoặc DefaultVideoServices.
	VideoServices map[string]VideoService

	// VideoReadinessRequiresAI: /health/ready trả 503 khi một dịch vụ video đang bật không còn
	// target AI nào kết nối được (cần SetVideoWorkerRegistry + SetGRPCPool). Mặc định tắt.
	VideoReadinessRequiresAI bool

	// WebRTC configures the WebRTC ingress (Pion PeerConnection, ICE servers, NAT).
	// Used to handle POST /v1/webrtc/offer.
	WebRTC wrtc.Config

	// MockResultPump bật giả lập ASR result 1s/lần sau khi nhận ctrl-result.
	// Chỉ dùng cho môi trường test/lab; tắt trong production.
	MockResultPump bool

	// EgressQueueFrames là số frame tối đa buffered trong egress pacer per session.
	// 0 → dùng mặc định 200 frame (4s tại 20ms/frame).
	EgressQueueFrames int
}

// DefaultServerConfig trả về cấu hình mặc định (h2c, port 8080).
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		Addr:          ":8080",
		ReadTimeout:   10 * time.Second,
		WriteTimeout:  10 * time.Second,
		IdleTimeout:   60 * time.Second,
		VideoServices: DefaultVideoServices(true),
	}
}

// Server là HTTP/2 control plane cho Media AI Gateway.
type Server struct {
	cfg        ServerConfig
	sessionMgr *session.Manager
	coord      Starter
	pool       *pipeline.WorkerPool
	aiMgr      *ai.Manager
	videoPool  *pipeline.VideoWorkerPool
	videoAI    *ai.VideoManager
	dispatcher *result.Dispatcher
	portAlloc  *PortAllocator
	admission  *AdmissionController
	registry   *GatewayRegistry
	selector   *GatewaySelector
	webrtc     *wrtc.Handler
	inner      *http.Server

	// Optional metric sources — set after construction via setter methods.
	ingress *rawrtp.Ingress
	// httpCallbacks giữ sink của session còn mở; khi session đóng, sink bị gỡ và
	// số retry của nó cộng vào retiredCallbackRetries (metric vẫn là tổng tích lũy).
	cbMu                   sync.Mutex
	httpCallbacks          map[*session.Session][]*result.HTTPCallbackSink
	retiredCallbackRetries uint64

	// Optional connection sources — set after construction via setter methods.
	grpcPool       *ai.SharedConnPool
	callbackClient *result.CallbackHTTPClient
	dcsfPool       *DCSFPool

	// videoRegistry: registry chọn worker video theo (dịch vụ, task); nil = không kiểm tra
	// AI theo dịch vụ khi ANSWER (Engine hoặc không có worker video).
	videoRegistry *ai.WorkerRegistry

	videoIngress rawrtp.VideoIngressCounters
}

// egressQueueFrames trả về số frame buffer cho egress pacer, mặc định 200 nếu không cấu hình.
func (s *Server) egressQueueFrames() int {
	if s.cfg.EgressQueueFrames > 0 {
		return s.cfg.EgressQueueFrames
	}
	return 200
}

// SetRTPIngress wires shared Raw RTP ingress cho metric collection tại /metrics.
func (s *Server) SetRTPIngress(ingress *rawrtp.Ingress) { s.ingress = ingress }

// SetVideoMetrics attaches the video sources before the HTTP server starts.
func (s *Server) SetVideoMetrics(pool *pipeline.VideoWorkerPool, manager *ai.VideoManager) {
	s.videoPool, s.videoAI = pool, manager
}

// RegisterCallbackSink đăng ký HTTPCallbackSink của sess để expose callback_retry_total
// tại /metrics. Khi sess đóng, sink được gỡ để danh sách không tăng theo số cuộc gọi.
// Goroutine-safe: được gọi từ HTTP handler goroutines đồng thời.
func (s *Server) RegisterCallbackSink(sess *session.Session, sink *result.HTTPCallbackSink) {
	s.cbMu.Lock()
	defer s.cbMu.Unlock()
	if s.httpCallbacks == nil {
		s.httpCallbacks = make(map[*session.Session][]*result.HTTPCallbackSink)
	}
	_, watched := s.httpCallbacks[sess]
	s.httpCallbacks[sess] = append(s.httpCallbacks[sess], sink)
	if !watched {
		go func() {
			<-sess.Ctx.Done()
			s.retireCallbackSinks(sess)
		}()
	}
}

// retireCallbackSinks gỡ sink của session đã đóng, giữ lại số retry của chúng.
func (s *Server) retireCallbackSinks(sess *session.Session) {
	s.cbMu.Lock()
	defer s.cbMu.Unlock()
	for _, sink := range s.httpCallbacks[sess] {
		s.retiredCallbackRetries += sink.Retries()
	}
	delete(s.httpCallbacks, sess)
}

// callbackRetries trả về tổng retry của mọi sink từng đăng ký (cả session đã đóng).
func (s *Server) callbackRetries() uint64 {
	s.cbMu.Lock()
	defer s.cbMu.Unlock()
	total := s.retiredCallbackRetries
	for _, sinks := range s.httpCallbacks {
		for _, sink := range sinks {
			total += sink.Retries()
		}
	}
	return total
}

// SetVideoWorkerRegistry bật kiểm tra AI theo dịch vụ khi ANSWER: dịch vụ video không còn
// worker phục vụ (dịch vụ, task) với slot trống → 503. Chỉ gọi khi video dial qua registry.
func (s *Server) SetVideoWorkerRegistry(r *ai.WorkerRegistry) { s.videoRegistry = r }

// SetGRPCPool wires SharedConnPool để expose trạng thái kết nối AI tại GET /v1/connections.
func (s *Server) SetGRPCPool(pool *ai.SharedConnPool) { s.grpcPool = pool }

// SetCallbackClient wires CallbackHTTPClient để expose trạng thái H/2 tại GET /v1/connections.
func (s *Server) SetCallbackClient(c *result.CallbackHTTPClient) { s.callbackClient = c }

// SetDCSFPool wires DCSFPool để gửi CALL_CTRL đến DCSF sau khi xử lý ANSWER.
// Nếu không gọi, handler dùng dcsfFallbackClient (không pre-warmed).
func (s *Server) SetDCSFPool(pool *DCSFPool) { s.dcsfPool = pool }

// SetWorkerRegistry wires AI WorkerRegistry vào AdmissionController để kiểm tra reachability.
// Readiness sẽ fail (reason: "no_ai_worker") khi không có fresh worker nào trong registry.
func (s *Server) SetWorkerRegistry(r *ai.WorkerRegistry) { s.admission.SetWorkerRegistry(r) }

// PortAllocator trả về per-session UDP port pool; nil nếu không cấu hình.
// Dùng để expose port stats cho monitor.
func (s *Server) PortAllocator() *PortAllocator { return s.portAlloc }

// SetMemThreshold cấu hình ngưỡng heap allocation (bytes) cho readiness check.
// Readiness sẽ fail (reason: "memory_pressure") khi heap vượt ngưỡng. 0 = disabled.
func (s *Server) SetMemThreshold(bytes uint64) { s.admission.SetMemThreshold(bytes) }

// MetricsHandler trả về http.Handler phục vụ chỉ endpoint /metrics.
// Dùng khi MetricsAddr khác HTTPAddr để chạy server metrics riêng.
func (s *Server) MetricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		s.metricsWrite(w)
	})
}

// NewServer tạo Server và đăng ký tất cả routes.
// Nếu cfg.RTPPortStart > 0, một PortAllocator được tạo tự động; panic nếu range không hợp lệ.
func NewServer(
	cfg ServerConfig,
	sessionMgr *session.Manager,
	coord Starter,
	pool *pipeline.WorkerPool,
	aiMgr *ai.Manager,
	dispatcher *result.Dispatcher,
) *Server {
	var portAlloc *PortAllocator
	if cfg.RTPPortStart > 0 {
		var err error
		portAlloc, err = NewPortAllocator(cfg.RTPPortStart, cfg.RTPPortEnd)
		if err != nil {
			panic("controlplane: invalid RTP port range: " + err.Error())
		}
	}

	ttl := cfg.RegistryTTL
	if ttl == 0 {
		ttl = defaultRegistryTTL
	}
	registry := NewGatewayRegistry(ttl)
	selector := NewGatewaySelector(registry)

	s := &Server{
		cfg:        cfg,
		sessionMgr: sessionMgr,
		coord:      coord,
		pool:       pool,
		aiMgr:      aiMgr,
		dispatcher: dispatcher,
		portAlloc:  portAlloc,
		admission:  NewAdmissionController(sessionMgr, pool, aiMgr, portAlloc),
		registry:   registry,
		selector:   selector,
	}

	if cfg.WebRTCEnabled {
		webrtcAPI, err := wrtc.NewAPI(cfg.WebRTC)
		if err != nil {
			panic("controlplane: WebRTC API init failed: " + err.Error())
		}
		webrtcHandler := wrtc.NewHandler(webrtcAPI, cfg.WebRTC, sessionMgr)
		webrtcHandler.SetDispatcher(dispatcher)
		s.webrtc = webrtcHandler
	}

	// Auto-register self so this node appears in the registry immediately.
	if cfg.GatewayID != "" {
		registry.Register(s.selfInfo())
	}

	var handler http.Handler = s.routes()

	if cfg.CertFile == "" {
		handler = h2c.NewHandler(handler, &http2.Server{})
	}

	s.inner = &http.Server{
		Addr:         cfg.Addr,
		Handler:      handler,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
	}

	if cfg.CertFile != "" {
		_ = http2.ConfigureServer(s.inner, nil)
	}

	return s
}

// routes đăng ký tất cả REST endpoints lên gin.Engine.
func (s *Server) routes() http.Handler {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery(), ginLogger())

	v1 := r.Group("/v1")
	{
		vonras := v1.Group("/vonras/call-sessions")
		vonras.POST("/:callId/notify-event", s.notifyEvent)
		vonras.POST("/:callId/ctrl-result", s.ctrlResult)
		vonras.DELETE("/:callId", s.deleteCallSession)
		vonras.GET("/:callId", s.getCallSession)

		if s.webrtc != nil {
			v1.POST("/webrtc/offer", gin.WrapF(s.webrtc.ServeOffer))
		}
		v1.PUT("/gateways/:id/heartbeat", s.heartbeat)
		v1.GET("/stats", s.stats)
		v1.GET("/connections", s.connections)
	}

	r.GET("/health/live", s.healthLive)
	r.GET("/health/ready", s.healthReady)
	r.GET("/health", s.healthLive) // backward compat
	r.GET("/metrics", s.metrics)

	return r
}

// selfInfo snapshots this node's current resource state for the GatewayRegistry.
func (s *Server) selfInfo() GatewayInfo {
	var usedPorts int
	if s.portAlloc != nil {
		usedPorts = s.portAlloc.Total() - s.portAlloc.Available()
	}
	var audioQueueUsage float64
	if cap := s.pool.QueueCap(); cap > 0 {
		audioQueueUsage = float64(s.pool.QueueLen()) / float64(cap)
	}
	return GatewayInfo{
		ID:              s.cfg.GatewayID,
		Addr:            s.cfg.Addr,
		RTPPublicIP:     s.cfg.RTPPublicIP,
		Sessions:        s.sessionMgr.Count(),
		MaxSessions:     s.sessionMgr.MaxSessions(),
		AIStreams:       s.aiMgr.Count(),
		MaxAIStreams:    s.aiMgr.MaxStreams(),
		AudioQueueUsage: audioQueueUsage,
		RTPPortStart:    s.cfg.RTPPortStart,
		RTPPortEnd:      s.cfg.RTPPortEnd,
		UsedPorts:       usedPorts,
		LastHeartbeatMs: time.Now().UnixMilli(),
	}
}

// ListenAndServe khởi động server, block cho đến khi ctx bị cancel hoặc lỗi fatal.
func (s *Server) ListenAndServe(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.inner.Shutdown(shutCtx)
	}()

	if s.cfg.CertFile != "" {
		return s.inner.ListenAndServeTLS(s.cfg.CertFile, s.cfg.KeyFile)
	}
	return s.inner.ListenAndServe()
}
