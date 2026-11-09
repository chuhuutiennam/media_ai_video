// Package coordinator wire toàn bộ per-session pipeline:
// PacketQueue → Jitter Buffer → Worker Pool → AI Stream → Result Dispatcher.
//
// Mỗi lần Start(sess) khởi động tất cả goroutine cho session đó.
// Goroutine tự thoát khi sess.Ctx bị cancel.
package coordinator

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"media-ai-gateway/internal/ai"
	"media-ai-gateway/internal/jitter"
	"media-ai-gateway/internal/pipeline"
	"media-ai-gateway/internal/result"
	"media-ai-gateway/internal/session"

	"go.uber.org/zap"
)

// Config cấu hình pipeline mỗi session.
type Config struct {
	JitterConfig    jitter.Config
	JitterOutSize   int                        // cap của channel jitterOut (jitter buffer → WorkerPool); default 16
	OutSampleRate   int                        // Hz sau resample (ví dụ: 16000)
	OutChannels     int                        // kênh đầu ra (thường 1 = mono)
	ChunkMs         int                        // độ dài AudioChunk gửi AI (ms)
	CallbackTimeout time.Duration              // timeout mỗi HTTP callback request (dùng khi CallbackClient = nil)
	CallbackRetry   int                        // số lần retry HTTP callback (không kể lần đầu)
	PCMDumpDir      string                     // nếu non-empty, ghi decoded PCM vào thư mục này (debug)
	CallbackClient  *result.CallbackHTTPClient // shared H/2 client; nil = per-sink transport (backward compat)

	// Jitter của track video (config video.jitter). VideoJitterConfig.PacketTimeMs == 0 → dùng
	// JitterConfig/JitterOutSize như audio.
	VideoJitterConfig  jitter.Config
	VideoJitterOutSize int

	// Video defaults — dùng khi session không tự chỉ định (Session.VideoInferWidth == 0, v.v.)
	VideoMaxWidth, VideoMaxHeight                                      int // ngưỡng an toàn cho decoder; 0 = default C++ (3840x2160)
	VideoInferWidth, VideoInferHeight                                  int
	VideoInferFPS                                                      int
	VideoJPEGQuality                                                   int
	VideoEffectType                                                    string // default effect nếu session không set
	GestureEnabled                                                     bool
	GestureFPS, GestureMaxDimension, GestureMaxHands, GestureQueueSize int
	VideoEngine                                                        bool
	VideoSourceMaxDimension                                            int
}

// DefaultConfig trả về cấu hình phù hợp cho production ASR.
func DefaultConfig() Config {
	return Config{
		JitterConfig:    jitter.DefaultConfig(),
		JitterOutSize:   16,
		OutSampleRate:   16000,
		OutChannels:     1,
		ChunkMs:         100,
		CallbackTimeout: 5 * time.Second,
		CallbackRetry:   3,

		VideoMaxWidth: 1920, VideoMaxHeight: 1080,
		VideoInferWidth: 256, VideoInferHeight: 256,
		VideoInferFPS:    5,
		VideoJPEGQuality: 80,
		VideoEffectType:  "bg_blur",
	}
}

// Coordinator wire các component lại với nhau cho mỗi session.
// Goroutine-safe: Start có thể gọi đồng thời cho nhiều session.
type Coordinator struct {
	cfg        Config
	pool       *pipeline.WorkerPool
	aiMgr      *ai.Manager
	dispatcher *result.Dispatcher

	// Video (tùy chọn) — nil nếu gateway build/deploy không bật video.
	videoPool *pipeline.VideoWorkerPool
	videoAI   *ai.VideoManager

	mu          sync.RWMutex
	videoJitter jitter.Counters
	jitterBufs  map[string]*jitter.Buffer // sessionID → active jitter buffer
}

// WithVideo gắn Video Worker Pool + Video AI Manager vào Coordinator.
func (c *Coordinator) WithVideo(videoPool *pipeline.VideoWorkerPool, videoAI *ai.VideoManager) *Coordinator {
	c.videoPool = videoPool
	c.videoAI = videoAI
	return c
}

// New tạo Coordinator với config và dependency cho trước.
func New(cfg Config, pool *pipeline.WorkerPool, aiMgr *ai.Manager, dispatcher *result.Dispatcher) *Coordinator {
	return &Coordinator{
		cfg:        cfg,
		pool:       pool,
		aiMgr:      aiMgr,
		dispatcher: dispatcher,
		jitterBufs: make(map[string]*jitter.Buffer),
	}
}

// Start kích hoạt per-session pipeline:
//  1. Đăng ký sink HTTP callback (nếu CallbackURL được cấu hình)
//  2. Đăng ký session với WorkerPool (decode → resample → chunk → AudioQueue)
//  3. Mở AI stream (audioIn=AudioQueue, resultOut=ResultQueue)
//  4. Jitter pump: PacketQueue → jitter buffer → WorkerPool.Submit
//  5. Result pump: ResultQueue → Dispatcher.Push
//  6. Cleanup goroutine: sau sess.Ctx.Done → UnregisterSession + UnregisterSinks
//
// Rollback: nếu bước 2 hoặc 3 thất bại, mọi đăng ký đã thực hiện đều được hoàn tác.
// Khi Start trả về nil, toàn bộ goroutine đã chạy và pipeline sẵn sàng nhận packet.
// HTTPCallbackSink được trả về (hoặc nil) để caller đăng ký vào metrics server.
func (c *Coordinator) Start(sess *session.Session) (*result.HTTPCallbackSink, error) {
	streamID := sess.ID
	if sess.TrackID != "" {
		streamID = sess.TrackID
	}

	// 1. HTTP callback sink.
	var cbSink *result.HTTPCallbackSink
	if sess.CallbackURL != "" {
		cbSink = c.newCallbackSink(sess.CallbackURL)
		c.dispatcher.RegisterSink(sess.ID, cbSink)
	}

	if sess.Codec != "" {
		// 2. Worker pool registration.
		poolCfg := pipeline.SessionConfig{
			Codec:         sess.Codec,
			SampleRate:    sess.SampleRate,
			Channels:      sess.Channels,
			OutSampleRate: c.cfg.OutSampleRate,
			OutChannels:   c.cfg.OutChannels,
			ChunkMs:       c.cfg.ChunkMs,
			SessionID:     sess.ID,
			StreamID:      streamID,
			PCMDumpDir:    c.cfg.PCMDumpDir,
		}
		if err := c.pool.RegisterSession(sess.Ctx, poolCfg, sess.AudioQueue); err != nil {
			c.dispatcher.UnregisterSinks(sess.ID)
			return nil, err
		}

		// 3. AI stream — rollback pool on failure.
		if _, err := c.aiMgr.Open(
			sess.Ctx, sess.ID, streamID, sess.Language, sess.Task,
			sess.AudioQueue, sess.ResultQueue,
		); err != nil {
			c.pool.UnregisterSession(sess.ID)
			c.dispatcher.UnregisterSinks(sess.ID)
			return nil, err
		}

		// 4. Jitter pump goroutines.
		c.startJitterPump(sess)
	}

	// 5. Result pump goroutine, luôn chạy dù audio hay video-only.
	go c.resultPump(sess)
	if sess.VideoEnabled {
		go c.pumpResults(sess, sess.VideoMaskResultQueue)
		go c.pumpResults(sess, sess.GestureResultQueue)
	}

	// 6. Cleanup sau khi session đóng.
	go func() {
		<-sess.Ctx.Done()
		if sess.Codec != "" {
			c.pool.UnregisterSession(sess.ID)
		}
		c.dispatcher.UnregisterSinks(sess.ID)
		c.mu.Lock()
		delete(c.jitterBufs, sess.ID)
		c.mu.Unlock()
	}()

	return cbSink, nil
}

// StartVideo kích hoạt video pipeline cho session (chỉ gọi khi
// sess.VideoEnabled == true, sau khi Start() đã thành công). Đối xứng với
// Start():
//  1. Đăng ký session với VideoWorkerPool (RTP → decode chain)
//  2. Mở AI stream với queue kết quả riêng cho mask và gesture (kể cả source RPC).
//  3. Video jitter pump: VideoPacketQueue → jitter buffer riêng → VideoWorkerPool.Submit
//  4. Cleanup: khi sess.Ctx.Done() → UnregisterSession + videoAI.Close()
//
// Yêu cầu Coordinator đã được gắn WithVideo(); nếu chưa, trả về lỗi ngay.
func (c *Coordinator) StartVideo(sess *session.Session) error {
	if c.videoPool == nil || c.videoAI == nil {
		return fmt.Errorf("coordinator: video not configured (call WithVideo first)")
	}
	if !sess.VideoEnabled {
		return fmt.Errorf("coordinator: session %s does not have video enabled", sess.ID)
	}
	// 1 dịch vụ ↔ 1 AI: mỗi session video chỉ mở đúng một AI task.
	task := videoAITask(sess)
	if task != videoTaskSegment && task != videoTaskGesture {
		return fmt.Errorf("coordinator: unsupported video AI task %q", task)
	}
	gestureOnly := task == videoTaskGesture
	if gestureOnly && !c.cfg.GestureEnabled {
		return fmt.Errorf("coordinator: gesture_recognition requires video.gesture.enabled=true")
	}

	streamID := sess.ID
	if sess.TrackID != "" {
		streamID = sess.TrackID
	}

	maxW, maxH := sess.VideoMaxWidth, sess.VideoMaxHeight
	if maxW == 0 || maxH == 0 {
		maxW, maxH = c.cfg.VideoMaxWidth, c.cfg.VideoMaxHeight
	}
	inferW, inferH := sess.VideoInferWidth, sess.VideoInferHeight
	if inferW == 0 || inferH == 0 {
		inferW, inferH = c.cfg.VideoInferWidth, c.cfg.VideoInferHeight
	}
	inferFPS := sess.VideoInferFPS
	if inferFPS == 0 {
		inferFPS = c.cfg.VideoInferFPS
	}
	effect := sess.EffectType
	if gestureOnly {
		effect = ""
	} else if effect == "" {
		effect = c.cfg.VideoEffectType
	}

	poolCfg := pipeline.VideoSessionConfig{
		MaxWidth: maxW, MaxHeight: maxH,
		InferWidth: inferW, InferHeight: inferH,
		InferFPS:            float64(inferFPS),
		JPEGQuality:         c.cfg.VideoJPEGQuality,
		SessionID:           sess.ID,
		StreamID:            streamID,
		EffectType:          effect,
		DisableSegmentation: gestureOnly,
	}
	var gestureQueue chan pipeline.VideoFrame
	var gestureStream string
	if gestureOnly && !c.cfg.VideoEngine {
		if c.cfg.GestureFPS < 1 || c.cfg.GestureQueueSize < 1 {
			return fmt.Errorf("invalid gesture profile")
		}
		gestureQueue = make(chan pipeline.VideoFrame, c.cfg.GestureQueueSize)
		gestureStream = canonicalVideoID(sess.ID)
		poolCfg.Gesture = &pipeline.GestureOutputConfig{StreamID: gestureStream, FPS: c.cfg.GestureFPS, MaxDimension: c.cfg.GestureMaxDimension, Output: gestureQueue}
	}
	var source *ai.SourceStreamConfig
	if c.cfg.VideoEngine {
		streamID = canonicalVideoID(sess.ID)
		poolCfg.StreamID, poolCfg.SourceMaxDimension = streamID, c.cfg.VideoSourceMaxDimension
		poolCfg.InferFPS = 0
		source = &ai.SourceStreamConfig{InputMaxDimension: c.cfg.VideoSourceMaxDimension}
		if !gestureOnly {
			source.Segmentation = &ai.VideoStreamConfig{EffectType: effect, InferWidth: inferW, InferHeight: inferH, InferFPS: inferFPS}
			poolCfg.InferFPS = float64(inferFPS)
		}
		if gestureOnly {
			source.Gesture = &ai.VideoStreamConfig{InferFPS: c.cfg.GestureFPS, MaxHands: c.cfg.GestureMaxHands, InferWidth: c.cfg.GestureMaxDimension}
			poolCfg.InferFPS = max(poolCfg.InferFPS, float64(c.cfg.GestureFPS))
		}
		source.InputFPS = int(poolCfg.InferFPS)
	}
	if err := c.videoPool.RegisterSession(sess.Ctx, poolCfg, sess.VideoFrameQueue); err != nil {
		return fmt.Errorf("coordinator: register video session: %w", err)
	}

	streamCfg := ai.VideoStreamConfig{
		EffectType:      effect,
		InferWidth:      inferW,
		InferHeight:     inferH,
		InferFPS:        inferFPS,
		SelectedService: sess.Task, // chọn worker theo dịch vụ; không gửi lên wire segmentation
	}
	// Each task has its own result queue and pump, started by Start().
	if source != nil {
		cfg := ai.VideoStreamConfig{Source: source, SelectedService: sess.Task,
			MediaEpoch: fmt.Sprintf("%s-%d", streamID, sess.CreatedAt.UnixNano()), GestureResultOut: sess.GestureResultQueue}
		if _, err := c.videoAI.Open(sess.Ctx, sess.ID, streamID, cfg, sess.VideoFrameQueue, sess.VideoMaskResultQueue); err != nil {
			c.videoPool.UnregisterSession(sess.ID)
			return fmt.Errorf("coordinator: open source stream: %w", err)
		}
	} else if !gestureOnly {
		if _, err := c.videoAI.Open(sess.Ctx, sess.ID, streamID, streamCfg, sess.VideoFrameQueue, sess.VideoMaskResultQueue); err != nil {
			c.videoPool.UnregisterSession(sess.ID)
			return fmt.Errorf("coordinator: open video AI stream: %w", err)
		}
	}
	if gestureQueue != nil {
		cfg := ai.VideoStreamConfig{AIService: "gesture_recognition", SelectedService: sess.Task, MediaEpoch: fmt.Sprintf("%s-%d", gestureStream, sess.CreatedAt.UnixNano()), InferFPS: c.cfg.GestureFPS, MaxHands: c.cfg.GestureMaxHands}
		if _, err := c.videoAI.Open(sess.Ctx, sess.ID, gestureStream, cfg, gestureQueue, sess.GestureResultQueue); err != nil {
			c.videoAI.Close(sess.ID)
			c.videoPool.UnregisterSession(sess.ID)
			return fmt.Errorf("coordinator: open gesture task: %w", err)
		}
	}

	c.startVideoJitterPump(sess)

	go func() {
		<-sess.Ctx.Done()
		c.videoPool.UnregisterSession(sess.ID)
		c.videoAI.Close(sess.ID)
		c.mu.Lock()
		delete(c.jitterBufs, sess.ID)
		c.mu.Unlock()
	}()

	return nil
}

// videoJitterConfig trả cấu hình jitter + cỡ jitterOut của track video; chưa cấu hình riêng
// thì dùng chung với audio.
func (c *Coordinator) videoJitterConfig() (jitter.Config, int) {
	cfg, outSize := c.cfg.VideoJitterConfig, c.cfg.VideoJitterOutSize
	if cfg.PacketTimeMs <= 0 {
		cfg, outSize = c.cfg.JitterConfig, c.cfg.JitterOutSize
	}
	if outSize <= 0 {
		outSize = 16
	}
	return cfg, outSize
}

// startVideoJitterPump khởi động jitter buffer + 2 goroutine bơm packet cho
// track video, đối xứng với startJitterPump (audio).
func (c *Coordinator) startVideoJitterPump(sess *session.Session) {
	jitterCfg, jitterOutSize := c.videoJitterConfig()
	jitterOut := make(chan pipeline.MediaPacket, jitterOutSize)
	buf := jitter.NewWithCounters(jitterCfg, &c.videoJitter)

	c.mu.Lock()
	c.jitterBufs[sess.ID] = buf
	c.mu.Unlock()

	go buf.Run(sess.Ctx, jitterOut)

	go func() {
		for {
			select {
			case <-sess.Ctx.Done():
				return
			case pkt := <-sess.VideoPacketQueue:
				buf.Push(pkt)
				sess.Touch()
			}
		}
	}()

	go func() {
		for {
			select {
			case <-sess.Ctx.Done():
				return
			case pkt := <-jitterOut:
				job := pipeline.VideoJob{SessionID: sess.ID, Packet: pkt}
				_ = c.videoPool.Submit(sess.Ctx, job)
			}
		}
	}()
}

// AI task video (trùng controlplane.VideoTaskSegment / VideoTaskGesture).
const (
	videoTaskSegment = "video_segment"
	videoTaskGesture = "gesture_recognition"
)

// videoAITask trả về AI task duy nhất của session video. Session không gán VideoAITask
// (tạo trực tiếp, không qua danh mục) thì suy ra từ Task: video_effects (dịch vụ gesture
// mặc định) hoặc gesture_recognition → gesture, còn lại → segmentation.
func videoAITask(sess *session.Session) string {
	if sess.VideoAITask != "" {
		return strings.ToLower(sess.VideoAITask)
	}
	if strings.EqualFold(sess.Task, "video_effects") || strings.EqualFold(sess.Task, videoTaskGesture) {
		return videoTaskGesture
	}
	return videoTaskSegment
}

func canonicalVideoID(id string) string {
	for _, leg := range []string{"tcore", "taccess"} {
		suffix := "-" + leg
		if strings.HasSuffix(id, suffix) {
			return strings.TrimSuffix(id, suffix) + ":" + leg + ":video"
		}
	}
	return id + ":video"
}

// startJitterPump khởi động 3 goroutine per-session:
//   - jitter.Buffer.Run(ctx, jitterOut): flush loop mỗi PacketTimeMs
//   - packet pump: PacketQueue → buf.Push + sess.Touch()
//   - submit pump: jitterOut → pool.Submit (non-blocking, drop nếu pool queue đầy)
func (c *Coordinator) startJitterPump(sess *session.Session) {
	jitterOutSize := c.cfg.JitterOutSize
	if jitterOutSize <= 0 {
		jitterOutSize = 16
	}
	jitterOut := make(chan pipeline.MediaPacket, jitterOutSize)
	buf := jitter.New(c.cfg.JitterConfig)

	c.mu.Lock()
	c.jitterBufs[sess.ID] = buf
	c.mu.Unlock()

	go buf.Run(sess.Ctx, jitterOut)

	go func() {
		for {
			select {
			case <-sess.Ctx.Done():
				return
			case pkt := <-sess.PacketQueue:
				buf.Push(pkt)
				sess.Touch()
			}
		}
	}()

	go func() {
		for {
			select {
			case <-sess.Ctx.Done():
				return
			case pkt := <-jitterOut:
				job := pipeline.AudioJob{SessionID: sess.ID, Packet: pkt}
				_ = c.pool.Submit(sess.Ctx, job)
			}
		}
	}()
}

func (c *Coordinator) resultPump(sess *session.Session) {
	c.pumpResults(sess, sess.ResultQueue)
}

func (c *Coordinator) pumpResults(sess *session.Session, input <-chan pipeline.RecognitionResult) {
	streamID := sess.ID
	if sess.TrackID != "" {
		streamID = sess.TrackID
	}
	for {
		select {
		case <-sess.Ctx.Done():
			return
		case r := <-input:
			if r.VideoMask != nil && r.VideoMask.Status != "ok" {
				zap.L().Debug("coordinator: video mask error — dropped",
					zap.String("session_id", sess.ID),
					zap.String("status", r.VideoMask.Status),
					zap.String("error", r.VideoMask.ErrorMessage),
				)
				continue
			}
			if r.SessionID == "" {
				r.SessionID = sess.ID
			}
			if r.StreamID == "" {
				r.StreamID = streamID
			}
			if r.SourceType == "" {
				r.SourceType = sess.SourceType
			}
			if r.MediaResources == nil {
				r.MediaResources = sess.MediaResources() // nil khi chưa có ctrl-result
			}
			if r.IsVideoGesture() && r.GestureResult != nil {
				g := r.GestureResult
				zap.L().Debug("gesture: hand snapshot", zap.String("session_id", r.SessionID), zap.String("stream_id", r.StreamID), zap.Int64("frame_id", g.FrameID), zap.Int("hands", len(g.Hands)))
				for _, h := range g.Hands {
					if len(h.Center) != 2 || len(h.BBox) != 4 {
						continue
					}
					zap.L().Debug("gesture: hand", zap.String("session_id", r.SessionID), zap.String("stream_id", r.StreamID), zap.Int64("frame_id", g.FrameID),
						zap.Uint64("track_id", h.TrackID), zap.String("gesture", h.StableLabel), zap.Float64("score", h.RawScore), zap.Any("center", h.Center), zap.Any("bbox", h.BBox),
						zap.Any("center_px", []int{int(h.Center[0] * float64(g.OriginalWidth)), int(h.Center[1] * float64(g.OriginalHeight))}),
						zap.Any("bbox_px", []int{int(h.BBox[0] * float64(g.OriginalWidth)), int(h.BBox[1] * float64(g.OriginalHeight)), int(h.BBox[2] * float64(g.OriginalWidth)), int(h.BBox[3] * float64(g.OriginalHeight))}))
				}
			}
			zap.L().Debug("ai: result received",
				zap.String("session_id", r.SessionID),
				zap.String("stream_id", r.StreamID),
				zap.String("kind", r.Kind),
				zap.Bool("is_final", r.IsFinal),
				zap.String("text", r.Text),
				zap.Uint64("seq", r.Seq),
				zap.Float32("confidence", r.Confidence),
				zap.String("language", r.Language),
				zap.Int64("ts_start", r.TsStart),
				zap.Int64("ts_end", r.TsEnd),
				zap.Int("audio_bytes", len(r.AudioPayload)),
			)
			if len(r.AudioPayload) > 0 {
				sess.SendRTP(r.AudioPayload) //nolint:errcheck // drop warning logged inside egress closure
				// Strip audio trước khi đưa vào HTTP callback — MF đã nhận audio qua RTP egress.
				r.AudioPayload = nil
				r.AudioPT = 0
			}
			if err := c.dispatcher.Push(sess.Ctx, r); err != nil {
				zap.L().Debug("coordinator: result dropped",
					zap.String("session_id", r.SessionID),
					zap.String("kind", r.Kind),
					zap.Error(err),
				)
			}
		}
	}
}

// UpdateCallbackSink thay thế HTTP callback sink của session.
// Nếu newURL rỗng, sink hiện tại được giữ nguyên (không override bằng nil).
// Trả về sink mới (hoặc nil nếu newURL rỗng) để caller đăng ký vào metrics.
func (c *Coordinator) UpdateCallbackSink(sessID, newURL string) *result.HTTPCallbackSink {
	if newURL == "" {
		return nil
	}
	cbSink := c.newCallbackSink(newURL)
	c.dispatcher.ReplaceHTTPSink(sessID, cbSink)
	return cbSink
}

// UpdateTaskCallbackSink configures a video-task HTTP endpoint independently of
// the session fallback. Empty URL restores the fallback.
func (c *Coordinator) UpdateTaskCallbackSink(sessID, task, rawURL string) *result.HTTPCallbackSink {
	var sink *result.HTTPCallbackSink
	if rawURL != "" {
		sink = c.newCallbackSink(rawURL)
	}
	c.dispatcher.ReplaceTaskHTTPSink(sessID, task, sink)
	return sink
}

// StartMockResultPump giả lập kết quả ASR mỗi 1 giây cho môi trường test.
// Goroutine tự thoát khi sess.Ctx bị cancel.
func (c *Coordinator) StartMockResultPump(sess *session.Session, terminationID string) {
	streamID := sess.ID
	if sess.TrackID != "" {
		streamID = sess.TrackID
	}
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		var seq uint64
		for {
			select {
			case <-sess.Ctx.Done():
				return
			case <-ticker.C:
				seq++
				r := pipeline.RecognitionResult{
					SessionID:  sess.ID,
					StreamID:   streamID,
					SourceType: sess.SourceType,
					Text:       "đây là test MF term2text với " + terminationID + " lúc " + time.Now().Format("02/01/2006 15:04:05"),
					IsFinal:    true,
					Seq:        seq,
					Confidence: 1.0,
				}
				select {
				case sess.ResultQueue <- r:
				default:
				}
			}
		}
	}()
}

// newCallbackSink tạo HTTPCallbackSink dùng shared client nếu có, hoặc per-sink transport.
func (c *Coordinator) newCallbackSink(rawURL string) *result.HTTPCallbackSink {
	if c.cfg.CallbackClient != nil {
		return c.cfg.CallbackClient.NewHTTPCallbackSink(rawURL, c.cfg.CallbackRetry)
	}
	return result.NewHTTPCallbackSink(rawURL, c.cfg.CallbackTimeout, c.cfg.CallbackRetry)
}

// AggregateJitterStats trả về tổng hợp jitter stats từ tất cả session đang active.
func (c *Coordinator) AggregateJitterStats() (received, released, dropped, lost uint64) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, buf := range c.jitterBufs {
		st := buf.Stats()
		received += st.Received
		released += st.Released
		dropped += st.Dropped
		lost += st.Lost
	}
	return
}

// VideoJitterStats includes closed sessions; counters do not retain buffer references.
func (c *Coordinator) VideoJitterStats() jitter.Stats { return c.videoJitter.Stats() }
