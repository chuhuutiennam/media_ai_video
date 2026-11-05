// Command media-ai-gateway là entry point của Media AI Gateway.
//
// Khởi động:
//
//	media-ai-gateway [--config path.yaml]
//	media-ai-gateway [flags]
//
// Khi --config được cung cấp, tất cả cấu hình đọc từ YAML file.
// Khi không có --config, dùng các flag riêng lẻ (backward compatible).
//
// Flags (mỗi flag có env var tương ứng GATEWAY_<FLAG_UPPER>):
//
//	--config           string   Path to YAML config file
//	--http-addr        string   HTTP/2 control plane (default ":8080")
//	--http-cert        string   TLS cert PEM — nếu rỗng dùng h2c cleartext
//	--http-key         string   TLS key PEM
//	--rtp-addr         string   Raw RTP UDP shared listen (default ":5004")
//	--gateway-id       string   Node identity returned in CREATE response
//	--rtp-public-ip    string   Public IP advertised to callers for per-session RTP
//	--rtp-bind-ip      string   Local IP to bind per-session UDP sockets (default "")
//	--rtp-port-start   int      First port in per-session port pool (0 = disabled)
//	--rtp-port-end     int      Last port in per-session port pool
//	--chunk-ms         int      Chunk duration gửi AI (ms, default 500)
//	--out-rate         int      Resample target Hz (default 16000)
//	--workers          int      Audio pipeline worker count (default 16)
//	--max-sess         int      Max concurrent sessions (default 10000)
//	--log-level        string   debug|info|warn|error (default info)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"media-ai-gateway/internal/ai"
	"media-ai-gateway/internal/config"
	"media-ai-gateway/internal/controlplane"
	"media-ai-gateway/internal/coordinator"
	"media-ai-gateway/internal/debug/pcap"
	"media-ai-gateway/internal/ingress/rawrtp"
	"media-ai-gateway/internal/monitor"
	"media-ai-gateway/internal/pipeline"
	"media-ai-gateway/internal/result"
	"media-ai-gateway/internal/session"
)

func main() {
	// ── flags ──────────────────────────────────────────────────────────────
	configFile := flag.String("config", envOr("GATEWAY_CONFIG", ""), "Path to YAML config file (overrides individual flags)")
	httpAddr := flag.String("http-addr", envOr("GATEWAY_HTTP_ADDR", ":8080"), "HTTP/2 control plane address")
	httpCert := flag.String("http-cert", envOr("GATEWAY_HTTP_CERT", ""), "TLS cert PEM (empty = h2c)")
	httpKey := flag.String("http-key", envOr("GATEWAY_HTTP_KEY", ""), "TLS key PEM")
	rtpAddr := flag.String("rtp-addr", envOr("GATEWAY_RTP_ADDR", ":5004"), "Raw RTP UDP shared listen address")
	gatewayID := flag.String("gateway-id", envOr("GATEWAY_ID", ""), "Node identity returned in CREATE response")
	rtpPublicIP := flag.String("rtp-public-ip", envOr("GATEWAY_RTP_PUBLIC_IP", ""), "Public IP advertised to callers for per-session RTP")
	rtpBindIP := flag.String("rtp-bind-ip", envOr("GATEWAY_RTP_BIND_IP", ""), "Local IP to bind per-session UDP sockets")
	rtpPortStart := flag.Int("rtp-port-start", 0, "First port in per-session pool (0 = disabled)")
	rtpPortEnd := flag.Int("rtp-port-end", 0, "Last port in per-session pool")
	chunkMs := flag.Int("chunk-ms", 500, "Audio chunk duration sent to AI (ms)")
	outRate := flag.Int("out-rate", 16000, "Resample output sample rate (Hz)")
	workers := flag.Int("workers", 16, "Audio pipeline worker pool size")
	maxSess := flag.Int("max-sess", 10000, "Maximum concurrent sessions")
	logLevel := flag.String("log-level", "info", "Log level: debug|info|warn|error")
	pcmDumpDir := flag.String("pcm-dump-dir", envOr("GATEWAY_PCM_DUMP_DIR", ""), "Directory for raw PCM debug dumps (empty = disabled)")
	mockResultPump := flag.Bool("mock-result-pump", false, "Enable mock ASR result pump every 1s (test/lab only)")
	flag.Parse()

	// ── config ─────────────────────────────────────────────────────────────
	var cfg config.GatewayConfig
	if *configFile != "" {
		var err error
		cfg, err = config.Load(*configFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to load config %q: %v\n", *configFile, err)
			os.Exit(1)
		}
	} else {
		cfg = config.Default()
		cfg.Server.HTTPAddr = *httpAddr
		cfg.Server.CertFile = *httpCert
		cfg.Server.KeyFile = *httpKey
		cfg.RTP.ListenAddr = *rtpAddr
		cfg.Gateway.ID = *gatewayID
		cfg.RTP.PublicIP = *rtpPublicIP
		cfg.RTP.BindIP = *rtpBindIP
		cfg.RTP.PortStart = *rtpPortStart
		cfg.RTP.PortEnd = *rtpPortEnd
		cfg.Audio.ChunkMs = *chunkMs
		cfg.Audio.OutputSampleRate = *outRate
		cfg.Pipeline.AudioWorkerCount = *workers
		cfg.Session.MaxSessions = *maxSess
		cfg.Log.Level = *logLevel
		if *pcmDumpDir != "" {
			cfg.Audio.PCMDumpDir = *pcmDumpDir
		}
		cfg.Gateway.MockResultPump = *mockResultPump
	}

	// ── logger ─────────────────────────────────────────────────────────────
	logger := buildZapLogger(cfg.Log.Level)
	zap.ReplaceGlobals(logger)
	defer logger.Sync() //nolint:errcheck

	zap.L().Info("media-ai-gateway starting",
		zap.String("http_addr", cfg.Server.HTTPAddr),
		zap.String("rtp_addr", cfg.RTP.ListenAddr),
		zap.String("gateway_id", cfg.Gateway.ID),
		zap.String("rtp_public_ip", cfg.RTP.PublicIP),
		zap.Ints("rtp_port_range", []int{cfg.RTP.PortStart, cfg.RTP.PortEnd}),
		zap.Int("chunk_ms", cfg.Audio.ChunkMs),
		zap.Int("out_rate", cfg.Audio.OutputSampleRate),
		zap.Int("workers", cfg.Pipeline.AudioWorkerCount),
		zap.Int("max_sess", cfg.Session.MaxSessions),
		zap.Bool("tls", cfg.Server.CertFile != ""),
		zap.Bool("h2c", cfg.Server.CertFile == ""),
	)

	// ── components ─────────────────────────────────────────────────────────
	sessMgr := session.NewManager(cfg.ToSessionManagerConfig())
	pool := pipeline.NewWorkerPool(cfg.ToWorkerPoolConfig())

	// WorkerRegistry: lưu AI worker nodes.
	// TTL=0 → static worker từ config không bao giờ expire.
	workerReg := ai.NewWorkerRegistry(0)

	// Shared gRPC connection pool: 1 conn per AI worker address, dùng chung cho tất cả session.
	kaTime, kaTimeout := cfg.ToGRPCKeepalive()
	grpcPool := ai.NewSharedConnPool(kaTime, kaTimeout, ai.WithReconnectMaxBackoff(cfg.ToGRPCReconnectMaxBackoff()))

	// Địa chỉ gRPC cần WatchAndReconnect (audio, engine, video workers) — không trùng lặp.
	var grpcWatchAddrs []string
	addGRPCWatch := func(addr string) {
		if addr != "" && !slices.Contains(grpcWatchAddrs, addr) {
			grpcWatchAddrs = append(grpcWatchAddrs, addr)
		}
	}

	var aiDialer ai.Dialer
	if cfg.AI.GRPCTarget != "" {
		addGRPCWatch(cfg.AI.GRPCTarget)
		// Seed registry với static worker từ config.
		// Khi AI worker tự đăng ký qua heartbeat, entry này bị override bởi thông tin live.
		workerReg.Register(ai.WorkerInfo{
			ID:         "config:" + cfg.AI.GRPCTarget,
			Addr:       cfg.AI.GRPCTarget,
			Tasks:      []string{"speech_to_text", "realtime_translation"},
			MaxStreams: cfg.AI.MaxActiveStreams,
		})
		aiDialer = ai.NewRoutingDialer(workerReg, grpcPool.DialFunc())
		// Pre-connect ngay khi khởi động — trigger async TCP dial, không block.
		grpcPool.Preconnect(cfg.AI.GRPCTarget)
		zap.L().Info("AI routing dialer active",
			zap.String("target", cfg.AI.GRPCTarget),
			zap.Duration("keepalive_time", kaTime),
			zap.Duration("keepalive_timeout", kaTimeout),
		)
	} else {
		// Dev/smoke-test mode: pipeline chạy đầy đủ nhưng không có transcript.
		aiDialer = ai.NullDialer{}
		zap.L().Warn("AI grpc_target not configured — using NullDialer, no transcripts will be generated")
	}
	aiMgr := ai.NewManager(cfg.ToAIConfig(), aiDialer)

	disp := result.NewDispatcher(cfg.ToDispatcherConfig())

	// Shared H/2 callback client — pre-connect khi khởi động nếu callback.url được cấu hình.
	callbackClient := cfg.ToCallbackHTTPClient()
	if cfg.Callback.URL != "" {
		preCtx, preCancel := context.WithTimeout(context.Background(), 10*time.Second)
		callbackClient.Preconnect(preCtx, cfg.Callback.URL)
		preCancel()
	}

	coordCfg := cfg.ToCoordinatorConfig()
	coordCfg.CallbackClient = callbackClient
	coord := coordinator.New(coordCfg, pool, aiMgr, disp)

	// videoPool cần Run() dù cfg.Video.Enabled=false lúc khởi động nhưng có thể KHÔNG cần thiết
	// nếu không session nào bật video — tạo pool luôn để tránh phải khởi động lại server khi
	// muốn bật video runtime qua config reload
	var videoPool *pipeline.VideoWorkerPool
	var videoAIMgr *ai.VideoManager
	if cfg.Video.Enabled {
		videoPool = pipeline.NewVideoWorkerPool(cfg.ToVideoWorkerPoolConfig())

		var videoDialer ai.VideoDialer
		videoWorkers := cfg.ToVideoWorkers()
		if cfg.Video.AI.EngineTarget != "" {
			workerReg.Register(ai.WorkerInfo{ID: "video-engine:" + cfg.Video.AI.EngineTarget, Addr: cfg.Video.AI.EngineTarget,
				Tasks: []string{"video_source"}, MediaTypes: []string{"video"}, InputFormats: []string{"jpeg"},
				Protocols: []string{"video_source"}, MaxStreams: cfg.Video.AI.MaxActiveStreams})
			grpcPool.Preconnect(cfg.Video.AI.EngineTarget)
			addGRPCWatch(cfg.Video.AI.EngineTarget)
			videoDialer = ai.NewSourceDialer(grpcPool, cfg.Video.AI.EngineTarget,
				time.Duration(cfg.Video.AI.OpenTimeoutMs)*time.Millisecond)
			zap.L().Info("video engine source routing active", zap.String("target", cfg.Video.AI.EngineTarget))
		} else if len(videoWorkers) != 0 {
			for _, worker := range videoWorkers {
				workerReg.Register(worker)
				grpcPool.Preconnect(worker.Addr)
				addGRPCWatch(worker.Addr)
				zap.L().Info("video AI worker registered", zap.String("worker_id", worker.ID),
					zap.String("target", worker.Addr), zap.Strings("tasks", worker.Tasks), zap.Int("max_streams", worker.MaxStreams))
			}
			videoDialer = ai.NewVideoRoutingDialer(workerReg, grpcPool.VideoDialFunc())
			zap.L().Info("video AI routing dialer active", zap.Int("worker_count", len(videoWorkers)))
		} else {
			videoDialer = ai.NullVideoDialer{}
			zap.L().Warn("no video AI workers configured — using NullVideoDialer")
		}
		videoAIMgr = ai.NewVideoManager(cfg.ToVideoAIConfig(), videoDialer)
		coord.WithVideo(videoPool, videoAIMgr)
		zap.L().Info("video pipeline enabled",
			zap.Int("workers", cfg.Video.WorkerCount),
			zap.Int("infer_fps", cfg.Video.InferFPS),
			zap.String("default_effect", cfg.Video.DefaultEffectType),
		)
	}

	rtpRouter := rawrtp.NewManagerRouter(sessMgr)
	rtpIngress := rawrtp.NewIngress(cfg.ToRTPIngressConfig(), rtpRouter)

	// DCSF outbound pool — pre-warm H/2 connection per configured host.
	dcsfPool := controlplane.NewDCSFPool(cfg.ToDCSFAddrs())
	if len(cfg.DCSF.Hosts) > 0 {
		preCtx, preCancel := context.WithTimeout(context.Background(), 10*time.Second)
		dcsfPool.Preconnect(preCtx)
		preCancel()
		zap.L().Info("DCSF pool initialized", zap.Int("hosts", len(cfg.DCSF.Hosts)))
	}

	apiSrv := controlplane.NewServer(cfg.ToControlPlaneServerConfig(), sessMgr, coord, pool, aiMgr, disp)
	apiSrv.SetRTPIngress(rtpIngress)
	apiSrv.SetVideoMetrics(videoPool, videoAIMgr)
	apiSrv.SetCallbackClient(callbackClient)
	apiSrv.SetDCSFPool(dcsfPool)
	// Wire WorkerRegistry và gRPC pool chỉ khi dùng RoutingDialer thực.
	// NullDialer (dev mode): bỏ qua để admission không block vì "no_ai_worker".
	if cfg.AI.GRPCTarget != "" {
		apiSrv.SetWorkerRegistry(workerReg)
		apiSrv.SetGRPCPool(grpcPool)
	}

	// ── PCAP capture (Linux only, debug) ──────────────────────────────────
	if cfg.PCAP.Enabled {
		rec, err := pcap.New(cfg.ToPCAPConfig())
		if err != nil {
			zap.L().Warn("pcap: disabled", zap.Error(err))
		} else {
			rec.Start()
			defer rec.Stop()
		}
	}

	// ── run ────────────────────────────────────────────────────────────────
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Periodic stats monitor (disabled when monitor_interval_sec = 0).
	mon := &monitor.Monitor{
		Interval:       time.Duration(cfg.Log.MonitorIntervalSec) * time.Second,
		SessionMgr:     sessMgr,
		Pool:           pool,
		AIMgr:          aiMgr,
		Dispatcher:     disp,
		GRPCPool:       grpcPool,
		CallbackClient: callbackClient,
		SharedIngress:  cfg.RTP.ListenAddr != "",
	}
	// Chỉ gán khi pool bật: con trỏ nil gán vào interface sẽ không còn == nil và monitor panic.
	if pa := apiSrv.PortAllocator(); pa != nil {
		mon.PortAlloc = pa
	}
	go mon.Run(ctx)

	if cfg.AI.WatchReconnect && len(grpcWatchAddrs) > 0 {
		for _, addr := range grpcWatchAddrs {
			go grpcPool.WatchAndReconnect(ctx, addr)
		}
		zap.L().Info("AI gRPC connection watch active",
			zap.Strings("addrs", grpcWatchAddrs),
			zap.Duration("reconnect_max_backoff", cfg.ToGRPCReconnectMaxBackoff()),
		)
	}

	go pool.Run(ctx)
	if videoPool != nil {
		go videoPool.Run(ctx)
	}
	go disp.Run(ctx)
	go sessMgr.Run(ctx)

	go func() {
		zap.L().Info("RTP ingress listening", zap.String("addr", cfg.RTP.ListenAddr))
		if err := rtpIngress.Run(ctx); err != nil {
			zap.L().Error("RTP ingress error", zap.Error(err))
			stop()
		}
	}()

	// Separate metrics server when MetricsAddr is configured.
	if cfg.Server.MetricsAddr != "" && cfg.Server.MetricsAddr != cfg.Server.HTTPAddr {
		go serveMetrics(ctx, cfg.Server.MetricsAddr, apiSrv.MetricsHandler())
	}

	httpDone := make(chan error, 1)
	go func() {
		mode := "h2c"
		if cfg.Server.CertFile != "" {
			mode = "tls+h2"
		}
		zap.L().Info("HTTP/2 control plane listening", zap.String("addr", cfg.Server.HTTPAddr), zap.String("mode", mode))
		httpDone <- apiSrv.ListenAndServe(ctx)
	}()

	// ── shutdown ───────────────────────────────────────────────────────────
	<-ctx.Done()
	zap.L().Info("signal received, shutting down")

	select {
	case err := <-httpDone:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			zap.L().Error("HTTP server exit error", zap.Error(err))
		}
	case <-time.After(15 * time.Second):
		zap.L().Warn("HTTP server shutdown timed out")
	}

	zap.L().Info("shutdown complete")
}

// serveMetrics runs a minimal HTTP server at addr serving only /metrics.
func serveMetrics(ctx context.Context, addr string, h http.Handler) {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", h)
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		zap.L().Error("metrics server listen failed", zap.String("addr", addr), zap.Error(err))
		return
	}
	zap.L().Info("metrics server listening", zap.String("addr", addr))
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		zap.L().Error("metrics server error", zap.Error(err))
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func buildZapLogger(level string) *zap.Logger {
	var l zapcore.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		fmt.Fprintf(os.Stderr, "unknown log level %q, using info\n", level)
		l = zapcore.InfoLevel
	}
	cfg := zap.NewProductionConfig()
	cfg.Level = zap.NewAtomicLevelAt(l)
	cfg.OutputPaths = []string{"stdout"}
	cfg.EncoderConfig.EncodeTime = zapcore.TimeEncoderOfLayout("2006-01-02T15:04:05.000000Z07:00")
	logger, err := cfg.Build()
	if err != nil {
		panic(fmt.Sprintf("failed to build logger: %v", err))
	}
	return logger
}
