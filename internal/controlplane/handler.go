package controlplane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"media-ai-gateway/internal/ingress/rawrtp"
	"media-ai-gateway/internal/pipeline"
	"media-ai-gateway/internal/result"
	"media-ai-gateway/internal/session"
)

// notifyEvent handles POST /v1/vonras/call-sessions/:callId/notify-event.
// Dispatches on sessionEvent.event: BEGIN → 200 OK; supported ANSWER → 201 Created with RTP endpoints.
func (s *Server) notifyEvent(c *gin.Context) {
	if zap.L().Core().Enabled(zap.DebugLevel) {
		raw, _ := io.ReadAll(c.Request.Body)
		c.Request.Body = io.NopCloser(bytes.NewReader(raw))
		zap.L().Debug("dcsf→dcas: notify-event raw", zap.String("call_id", c.Param("callId")), zap.ByteString("body", raw))
	}

	var event SessionEvent
	if err := c.ShouldBindJSON(&event); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid JSON: " + err.Error()})
		return
	}
	callID := c.Param("callId")
	zap.L().Debug("dcsf→dcas: notify-event", zap.String("call_id", callID), zap.String("event", event.Event), zap.String("service", event.SelectedService), zap.String("callback_url", event.CallbackURL))
	switch strings.ToUpper(event.Event) {
	case "BEGIN":
		c.JSON(http.StatusOK, gin.H{})
	case "ANSWER":
		s.handleAnswer(c, callID, &event)
	default:
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "unknown event: " + event.Event})
	}
}

// handleAnswer creates tcore/taccess sessions and allocates only the media
// required by selectedService: audio services (serviceProfiles) get audio,
// video services (ServerConfig.VideoServices) get video with exactly one AI task.
func (s *Server) handleAnswer(c *gin.Context, callID string, event *SessionEvent) {
	service := strings.ToLower(event.SelectedService)
	profile, isAudio := lookupServiceProfile(service)
	if isAudio && s.cfg.AudioDisabled {
		// Gateway chỉ video: báo lỗi để DCSF biết đã gửi dịch vụ audio nhầm gateway (không 200 {}).
		zap.L().Warn("dcsf→dcas: audio service rejected, audio.enabled=false",
			zap.String("call_id", callID), zap.String("service", event.SelectedService))
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "service " + event.SelectedService + " is not supported by this gateway (audio.enabled=false)"})
		return
	}
	videoSvc, isVideo := s.cfg.VideoServices[service]
	isVideo = isVideo && videoSvc.Enabled
	if !isAudio && !isVideo {
		// Service not handled or disabled (e.g. fun_calling) — acknowledge without creating session.
		c.JSON(http.StatusOK, gin.H{})
		return
	}
	singleLeg := isVideo && videoSvc.SingleLeg

	if ok, retryMs := s.admission.Check(); !ok {
		c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: "service at capacity, try again later", RetryAfterMs: retryMs})
		return
	}

	// 1 dịch vụ ↔ 1 AI: dịch vụ audio không mở video kể cả khi bearerCapability=VIDEO.
	audioEnabled := isAudio
	videoEnabled := isVideo && s.cfg.VideoEnabled
	if !audioEnabled && !videoEnabled {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "service " + event.SelectedService + " requires video.enabled=true"})
		return
	}
	if videoEnabled && s.portAlloc == nil {
		c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: "video requires a per-session RTP port pool"})
		return
	}
	if videoEnabled && s.videoRegistry != nil && !s.videoRegistry.HasVideoCapacity(service, videoSvc.AITask) {
		c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: "no AI worker available for service " + service, RetryAfterMs: 3000})
		return
	}
	tcoreID := callID + "-tcore"
	taccessID := callID + "-taccess"
	baseCfg := session.SessionConfig{
		SourceType:  "raw_rtp",
		Channels:    1,
		Task:        event.SelectedService,
		CallbackURL: s.cfg.ResultCallbackURL,

		VideoEnabled: videoEnabled,
		VideoCodec:   "H264",
	}
	if videoEnabled {
		baseCfg.VideoAITask = videoSvc.AITask
		baseCfg.EffectType = videoSvc.EffectType
	}
	if audioEnabled {
		baseCfg.Codec = profile.Codec
		baseCfg.SampleRate = profile.SampleRate
	}

	baseCfg.ID = tcoreID
	sessTCore, err := s.sessionMgr.Create(baseCfg)
	if err != nil {
		switch {
		case errors.Is(err, session.ErrDuplicateID):
			c.JSON(http.StatusConflict, ErrorResponse{Error: err.Error()})
		case errors.Is(err, session.ErrMaxSessions):
			c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: err.Error(), RetryAfterMs: 5000})
		default:
			c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		}
		return
	}
	sessTCore.GatewayID = s.cfg.GatewayID

	// Only ports without a listener belong to this request. Once a listener
	// starts it owns the port and releases it AFTER closing its socket.
	pendingPorts := make(map[int]bool)
	rollback := func(status int, msg string) {
		for p := range pendingPorts {
			s.portAlloc.Release(p)
		}
		s.sessionMgr.Close(tcoreID)
		if !singleLeg {
			s.sessionMgr.Close(taccessID)
		}
		c.JSON(status, ErrorResponse{Error: msg})
	}

	var sessAccess *session.Session
	if !singleLeg {
		baseCfg.ID = taccessID
		sessAccess, err = s.sessionMgr.Create(baseCfg)
		if err != nil {
			s.sessionMgr.Close(tcoreID)
			switch {
			case errors.Is(err, session.ErrDuplicateID):
				c.JSON(http.StatusConflict, ErrorResponse{Error: err.Error()})
			case errors.Is(err, session.ErrMaxSessions):
				c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: err.Error(), RetryAfterMs: 5000})
			default:
				c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
			}
			return
		}
		sessAccess.GatewayID = s.cfg.GatewayID
	}

	var tcorePort, taccessPort int
	var tcoreVideoPort, taccessVideoPort int
	if s.portAlloc != nil {
		type legDef struct {
			sess                 *session.Session
			audioPort, videoPort *int
		}
		legs := []legDef{{sessTCore, &tcorePort, &tcoreVideoPort}}
		if !singleLeg {
			legs = append(legs, legDef{sessAccess, &taccessPort, &taccessVideoPort})
		}
		for _, leg := range legs {
			for _, media := range []struct {
				enabled, video bool
				port           *int
			}{
				{audioEnabled, false, leg.audioPort}, {videoEnabled, true, leg.videoPort},
			} {
				if !media.enabled {
					continue
				}
				port, err := s.portAlloc.Acquire()
				if err != nil {
					rollback(http.StatusServiceUnavailable, "no RTP ports available")
					return
				}
				pendingPorts[port] = true
				if media.video {
					err = rawrtp.StartVideoSessionListener(leg.sess, s.cfg.RTPBindIP, port, s.portAlloc, &s.videoIngress)
				} else {
					err = rawrtp.StartSessionListener(leg.sess, s.cfg.RTPBindIP, port, s.portAlloc, s.egressQueueFrames())
				}
				if err != nil {
					rollback(http.StatusInternalServerError, err.Error())
					return
				}
				delete(pendingPorts, port)
				*media.port = port
			}
		}
	}

	cbSink1, err := s.coord.Start(sessTCore)
	if err != nil {
		rollback(http.StatusBadGateway, "pipeline: "+err.Error())
		return
	}
	if cbSink1 != nil {
		s.RegisterCallbackSink(sessTCore, cbSink1)
	}
	if !singleLeg {
		cbSink2, err := s.coord.Start(sessAccess)
		if err != nil {
			rollback(http.StatusBadGateway, "pipeline: "+err.Error())
			return
		}
		if cbSink2 != nil {
			s.RegisterCallbackSink(sessAccess, cbSink2)
		}
	}

	if videoEnabled {
		if err := s.coord.StartVideo(sessTCore); err != nil {
			rollback(http.StatusBadGateway, "video pipeline: "+err.Error())
			return
		}
		if !singleLeg {
			if err := s.coord.StartVideo(sessAccess); err != nil {
				rollback(http.StatusBadGateway, "video pipeline: "+err.Error())
				return
			}
		}
	}

	resp := sessionToResponse(sessTCore)
	resp.SessionID = callID
	resp.GatewayID = s.cfg.GatewayID
	if tcorePort != 0 {
		resp.RTPIP = s.cfg.RTPPublicIP
		resp.TCoreRTPPort = tcorePort
		resp.TAccessRTPPort = taccessPort
		resp.TCoreLocalNonDcMedia = buildNonDcMedia(profile.Codec, profile.SampleRate, 1, tcorePort, 0)
		resp.TAccessLocalNonDcMedia = buildNonDcMedia(profile.Codec, profile.SampleRate, 1, taccessPort, 0)
	} else if videoEnabled {
		// Video-only: vẫn cần trả rtp_ip dù không có port audio, để DCSF biết gateway đang lắng nghe
		resp.RTPIP = s.cfg.RTPPublicIP
	}
	if videoEnabled && tcoreVideoPort != 0 {
		resp.TCoreVideoRTPPort = tcoreVideoPort
		resp.TCoreLocalNonDcVideoMedia = buildVideoNonDcMedia(tcoreVideoPort, 0)
		if !singleLeg {
			resp.TAccessVideoRTPPort = taccessVideoPort
			resp.TAccessLocalNonDcVideoMedia = buildVideoNonDcMedia(taccessVideoPort, 0)
		}
	}
	zap.L().Debug("dcas→dcsf: answer ack",
		zap.String("call_id", callID), zap.String("rtp_ip", resp.RTPIP),
		zap.Int("tcore_rtp_port", resp.TCoreRTPPort), zap.Int("taccess_rtp_port", resp.TAccessRTPPort),
		zap.Bool("video_enabled", videoEnabled),
		zap.Int("tcore_video_rtp_port", resp.TCoreVideoRTPPort), zap.Int("taccess_video_rtp_port", resp.TAccessVideoRTPPort),
	)
	c.JSON(http.StatusCreated, resp)
	c.Writer.Flush()

	if event.CallbackURL != "" {
		timeout := s.cfg.DCSFCallControlTimeout
		if timeout == 0 {
			timeout = 30 * time.Second
		}
		ctrlResultURL := s.cfg.PublicURL + "/v1/vonras/call-sessions/" + callID + "/ctrl-result"
		zap.L().Debug("dcas→dcsf: call-control", zap.String("call_id", callID), zap.String("target", event.CallbackURL), zap.String("service", event.SelectedService), zap.String("ctrl_result_url", ctrlResultURL))
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			if err := s.dcsfPool.SendCallControl(ctx, event.CallbackURL, callID, ctrlResultURL); err != nil {
				zap.L().Warn("call-control failed, releasing session", zap.String("call_id", callID), zap.Error(err))
				s.sessionMgr.Close(tcoreID)
				s.sessionMgr.Close(taccessID)
			}
		}()
	}
}

// ServiceProfile mô tả dịch vụ audio (codec gửi DCSF và task gửi AI).
type ServiceProfile struct {
	Codec      string
	SampleRate int
}

// serviceProfiles là bảng tra cứu selectedService audio → ServiceProfile. Khớp
// bảng selectedService trong docs/dcas-dcsf-signaling.md. Dịch vụ video nằm ở
// ServerConfig.VideoServices (cấu hình video.services).
var serviceProfiles = map[string]ServiceProfile{
	"speech_to_text":       {Codec: "PCMU", SampleRate: 8000},
	"realtime_translation": {Codec: "PCMU", SampleRate: 8000},
}

// lookupServiceProfile tra bảng serviceProfiles (case-insensitive). ok=false
// nếu không phải dịch vụ audio.
func lookupServiceProfile(service string) (ServiceProfile, bool) {
	p, ok := serviceProfiles[strings.ToLower(service)]
	return p, ok
}

// IsAudioService cho biết tên dịch vụ đã dùng cho audio (không được khai báo lại ở video.services).
func IsAudioService(service string) bool {
	_, ok := lookupServiceProfile(service)
	return ok
}

// AI task video — trùng tên task trong WorkerRegistry và key callbackUrls của ctrl-result.
const (
	VideoTaskSegment = "video_segment"
	VideoTaskGesture = "gesture_recognition"
)

// VideoService mô tả một dịch vụ video người dùng đăng ký: đúng một AI task
// (1 dịch vụ ↔ 1 AI). Xem docs/giai-phap-video-service-catalog.md.
type VideoService struct {
	Enabled    bool
	SingleLeg  bool   // true: chỉ tạo leg tcore; false: tcore + taccess
	EffectType string // chỉ dùng với VideoTaskSegment
	AITask     string // VideoTaskSegment | VideoTaskGesture
}

// DefaultVideoServices là danh mục khi không cấu hình video.services:
// change_background → segmentation (AI repo mediapipe); video_effects → gesture
// (AI repo gesture), chỉ có khi gesture được bật. Cả hai chỉ tạo leg tcore.
func DefaultVideoServices(gestureEnabled bool) map[string]VideoService {
	services := map[string]VideoService{
		"change_background": {Enabled: true, SingleLeg: true, EffectType: "bg_replace", AITask: VideoTaskSegment},
	}
	if gestureEnabled {
		services["video_effects"] = VideoService{Enabled: true, SingleLeg: true, AITask: VideoTaskGesture}
	}
	return services
}

// getCallSession handles GET /v1/vonras/call-sessions/:callId.
// Looks up {callId}-taccess first (subscriber side), falls back to {callId}-tcore.
func (s *Server) getCallSession(c *gin.Context) {
	callID := c.Param("callId")
	sess, ok := s.sessionMgr.Get(callID + "-taccess")
	if !ok {
		sess, ok = s.sessionMgr.Get(callID + "-tcore")
	}
	if !ok {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: "session not found"})
		return
	}
	resp := sessionToResponse(sess)
	resp.SessionID = callID
	c.JSON(http.StatusOK, resp)
}

// ctrlResult handles POST /v1/vonras/call-sessions/:callId/ctrl-result.
// Updates mediaResources and per-termination callbackUrl for both tcore and taccess sessions.
func (s *Server) ctrlResult(c *gin.Context) {
	if zap.L().Core().Enabled(zap.DebugLevel) {
		raw, _ := io.ReadAll(c.Request.Body)
		c.Request.Body = io.NopCloser(bytes.NewReader(raw))
		zap.L().Debug("dcsf→dcas: ctrl-result raw", zap.String("call_id", c.Param("callId")), zap.ByteString("body", raw))
	}

	var req CtrlResultRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid JSON: " + err.Error()})
		return
	}
	id := c.Param("callId")
	zap.L().Debug("dcsf→dcas: ctrl-result", zap.String("call_id", id), zap.Bool("has_media_resources", req.MediaResources != nil))

	var singleLeg bool
	if tcoreSess, ok := s.sessionMgr.Get(id + "-tcore"); ok {
		if v, ok := s.cfg.VideoServices[strings.ToLower(tcoreSess.Task)]; ok {
			singleLeg = v.SingleLeg
		}
	}

	if req.MediaResources != nil {
		for _, resource := range []MediaResourceInfo{req.MediaResources.TCore, req.MediaResources.TAccess} {
			for task := range resource.CallbackURLs {
				if task != "video_segment" && task != "gesture_recognition" {
					c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported callbackUrls task: " + task})
					return
				}
			}
		}
		// URL sai (scheme lạ, thiếu host…) được nhận thì mọi kết quả sau đó gửi lỗi và mất →
		// từ chối cả bản tin, không áp dụng phần nào. Rỗng giữ nghĩa cũ: callbackUrl rỗng = giữ URL
		// hiện tại, callbackUrls[task] rỗng = quay về URL mặc định của session.
		if field := invalidCtrlResultURL(req.MediaResources); field != "" {
			// Không log URL: có thể chứa user:password (một trong các lý do bị từ chối).
			zap.L().Warn("dcsf→dcas: ctrl-result rejected, invalid callback URL",
				zap.String("call_id", id), zap.String("field", field))
			c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid " + field + ": must be an absolute http(s) URL with host"})
			return
		}
		taskUpdater, supportsTasks := s.coord.(interface {
			UpdateTaskCallbackSink(string, string, string) *result.HTTPCallbackSink
		})
		if !supportsTasks && (len(req.MediaResources.TCore.CallbackURLs) > 0 || len(req.MediaResources.TAccess.CallbackURLs) > 0) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "task callbacks unsupported by coordinator"})
			return
		}
		zap.L().Debug("dcsf→dcas: ctrl-result media",
			zap.String("call_id", id),
			zap.String("tcore_ctx", req.MediaResources.TCore.ContextID),
			zap.String("tcore_term", req.MediaResources.TCore.Termination.TerminationID),
			zap.String("tcore_callback", req.MediaResources.TCore.CallbackURL),
			zap.String("taccess_ctx", req.MediaResources.TAccess.ContextID),
			zap.String("taccess_term", req.MediaResources.TAccess.Termination.TerminationID),
			zap.String("taccess_callback", req.MediaResources.TAccess.CallbackURL),
		)

		tcoreCallbackURL := req.MediaResources.TCore.CallbackURL
		if singleLeg {
			tcoreCallbackURL = req.MediaResources.TAccess.CallbackURL
		}
		tcoreMR := &pipeline.MediaResources{
			TCore: &pipeline.MediaResource{
				ContextID:     req.MediaResources.TCore.ContextID,
				TerminationID: req.MediaResources.TCore.Termination.TerminationID,
			},
		}
		if singleLeg {
			tcoreMR.TAccess = &pipeline.MediaResource{
				ContextID:     req.MediaResources.TAccess.ContextID,
				TerminationID: req.MediaResources.TAccess.Termination.TerminationID,
			}
		}
		tcorePatch := session.SessionPatch{
			CallbackURL:    tcoreCallbackURL,
			MediaResources: tcoreMR,
		}
		if sessTCore, err := s.sessionMgr.Update(id+"-tcore", tcorePatch); err == nil {
			for task, rawURL := range req.MediaResources.TCore.CallbackURLs {
				if cb := taskUpdater.UpdateTaskCallbackSink(id+"-tcore", task, rawURL); cb != nil {
					s.RegisterCallbackSink(sessTCore, cb)
				}
			}

			if cb := s.coord.UpdateCallbackSink(id+"-tcore", tcoreCallbackURL); cb != nil {
				s.RegisterCallbackSink(sessTCore, cb)
			}
			if s.cfg.MockResultPump {
				s.coord.StartMockResultPump(sessTCore, req.MediaResources.TCore.Termination.TerminationID)
			}
		}

		if !singleLeg {
			taccessPatch := session.SessionPatch{
				CallbackURL: req.MediaResources.TAccess.CallbackURL,
				MediaResources: &pipeline.MediaResources{
					TAccess: &pipeline.MediaResource{
						ContextID:     req.MediaResources.TAccess.ContextID,
						TerminationID: req.MediaResources.TAccess.Termination.TerminationID,
					},
				},
			}
			if sessAccess, err := s.sessionMgr.Update(id+"-taccess", taccessPatch); err == nil {
				for task, rawURL := range req.MediaResources.TAccess.CallbackURLs {
					if cb := taskUpdater.UpdateTaskCallbackSink(id+"-taccess", task, rawURL); cb != nil {
						s.RegisterCallbackSink(sessAccess, cb)
					}
				}
				if cb := s.coord.UpdateCallbackSink(id+"-taccess", req.MediaResources.TAccess.CallbackURL); cb != nil {
					s.RegisterCallbackSink(sessAccess, cb)
				}
				if s.cfg.MockResultPump {
					s.coord.StartMockResultPump(sessAccess, req.MediaResources.TAccess.Termination.TerminationID)
				}
			}
		}
	}

	sess, ok := s.sessionMgr.Get(id + "-taccess")
	if !ok {
		sess, ok = s.sessionMgr.Get(id + "-tcore")
	}
	if !ok {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: "session not found"})
		return
	}
	resp := sessionToResponse(sess)
	resp.SessionID = id
	c.JSON(http.StatusOK, resp)
}

const maxCallbackURLLen = 2048

// validCallbackURL chấp nhận URL http/https tuyệt đối có host, không kèm userinfo (theo media_ai).
func validCallbackURL(raw string) bool {
	if raw == "" || len(raw) > maxCallbackURLLen {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Hostname() == "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return true
	}
	return false
}

// invalidCtrlResultURL trả tên field của URL không hợp lệ đầu tiên trong ctrl-result ("" nếu mọi
// URL hợp lệ). Bỏ qua giá trị rỗng (có nghĩa riêng, xem ctrlResult).
func invalidCtrlResultURL(mr *MediaResources) string {
	for _, leg := range []struct {
		name string
		res  MediaResourceInfo
	}{{"tCore", mr.TCore}, {"tAccess", mr.TAccess}} {
		if u := leg.res.CallbackURL; u != "" && !validCallbackURL(u) {
			return leg.name + ".callbackUrl"
		}
		tasks := make([]string, 0, len(leg.res.CallbackURLs))
		for task := range leg.res.CallbackURLs {
			tasks = append(tasks, task)
		}
		slices.Sort(tasks)
		for _, task := range tasks {
			if u := leg.res.CallbackURLs[task]; u != "" && !validCallbackURL(u) {
				return leg.name + ".callbackUrls." + task
			}
		}
	}
	return ""
}

// deleteCallSession handles DELETE /v1/vonras/call-sessions/:callId.
// Closes both {callId}-tcore and {callId}-taccess sessions.
func (s *Server) deleteCallSession(c *gin.Context) {
	callID := c.Param("callId")
	zap.L().Debug("dcsf→dcas: delete call-session", zap.String("call_id", callID))
	closedCore := s.sessionMgr.Close(callID + "-tcore")
	closedAccess := s.sessionMgr.Close(callID + "-taccess")
	if !closedCore && !closedAccess {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: "session not found"})
		return
	}
	c.Status(http.StatusNoContent)
}

// heartbeat handles PUT /v1/gateways/:id/heartbeat.
func (s *Server) heartbeat(c *gin.Context) {
	var info GatewayInfo
	if err := c.ShouldBindJSON(&info); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid JSON: " + err.Error()})
		return
	}
	id := c.Param("id")
	if info.ID != "" && info.ID != id {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "id in body does not match URL path"})
		return
	}
	info.ID = id
	s.registry.Register(info)
	if s.cfg.GatewayID != "" {
		s.registry.Register(s.selfInfo())
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// healthLive handles GET /health/live — Kubernetes liveness probe.
func (s *Server) healthLive(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// healthReady handles GET /health/ready — Kubernetes readiness probe.
func (s *Server) healthReady(c *gin.Context) {
	ok, retryMs, reason := s.admission.CheckWithReason()
	if !ok {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"status":         "not_ready",
			"reason":         reason,
			"retry_after_ms": retryMs,
		})
		return
	}
	if s.cfg.VideoReadinessRequiresAI {
		if down := s.videoServicesWithoutAI(); len(down) > 0 {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"status":         "not_ready",
				"reason":         "video_service_unavailable",
				"services":       down,
				"retry_after_ms": 3000,
			})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"status": "ready"})
}

// videoServicesWithoutAI trả (đã sắp xếp) các dịch vụ video đang bật không còn target AI nào
// kết nối được. Chỉ xét khi video chọn AI qua registry và có pool gRPC.
func (s *Server) videoServicesWithoutAI() []string {
	if !s.cfg.VideoEnabled || s.videoRegistry == nil || s.grpcPool == nil {
		return nil
	}
	var down []string
	for name, svc := range s.cfg.VideoServices {
		if !svc.Enabled {
			continue
		}
		alive := false
		for _, w := range s.videoRegistry.VideoWorkers(name, svc.AITask) {
			if s.grpcPool.Reachable(w.Addr) {
				alive = true
				break
			}
		}
		if !alive {
			down = append(down, name)
		}
	}
	slices.Sort(down)
	return down
}

// metrics handles GET /metrics — Prometheus text exposition format (version 0.0.4).
func (s *Server) metrics(c *gin.Context) {
	w := c.Writer
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	s.metricsWrite(w)
}

// metricsWrite writes Prometheus text output to w.
func (s *Server) metricsWrite(w io.Writer) {
	s.videoMetricsWrite(w)
	ps := s.pool.Stats()
	ds := s.dispatcher.Stats()
	now := time.Now().UnixMilli()

	fmt.Fprintf(w, "# HELP media_ai_sessions_active Active session count\n")
	fmt.Fprintf(w, "# TYPE media_ai_sessions_active gauge\n")
	fmt.Fprintf(w, "media_ai_sessions_active %d\n", s.sessionMgr.Count())

	fmt.Fprintf(w, "# HELP media_ai_sessions_max Maximum allowed session count\n")
	fmt.Fprintf(w, "# TYPE media_ai_sessions_max gauge\n")
	fmt.Fprintf(w, "media_ai_sessions_max %d\n", s.sessionMgr.MaxSessions())

	fmt.Fprintf(w, "# HELP media_ai_sessions_created_total Total sessions created since startup\n")
	fmt.Fprintf(w, "# TYPE media_ai_sessions_created_total counter\n")
	fmt.Fprintf(w, "media_ai_sessions_created_total %d\n", s.sessionMgr.CreatedTotal())

	fmt.Fprintf(w, "# HELP media_ai_sessions_closed_total Total sessions closed since startup\n")
	fmt.Fprintf(w, "# TYPE media_ai_sessions_closed_total counter\n")
	fmt.Fprintf(w, "media_ai_sessions_closed_total %d\n", s.sessionMgr.ClosedTotal())

	fmt.Fprintf(w, "# HELP media_ai_ai_streams_active Active AI gRPC stream count\n")
	fmt.Fprintf(w, "# TYPE media_ai_ai_streams_active gauge\n")
	fmt.Fprintf(w, "media_ai_ai_streams_active %d\n", s.aiMgr.Count())

	fmt.Fprintf(w, "# HELP media_ai_ai_streams_max Maximum allowed AI stream count\n")
	fmt.Fprintf(w, "# TYPE media_ai_ai_streams_max gauge\n")
	fmt.Fprintf(w, "media_ai_ai_streams_max %d\n", s.aiMgr.MaxStreams())

	fmt.Fprintf(w, "# HELP media_ai_pool_sessions_active Per-session pipelines registered in worker pool\n")
	fmt.Fprintf(w, "# TYPE media_ai_pool_sessions_active gauge\n")
	fmt.Fprintf(w, "media_ai_pool_sessions_active %d\n", s.pool.SessionCount())

	fmt.Fprintf(w, "# HELP media_ai_pool_queue_len Worker pool pending job count\n")
	fmt.Fprintf(w, "# TYPE media_ai_pool_queue_len gauge\n")
	fmt.Fprintf(w, "media_ai_pool_queue_len %d\n", s.pool.QueueLen())

	fmt.Fprintf(w, "# HELP media_ai_pool_queue_cap Worker pool job queue capacity\n")
	fmt.Fprintf(w, "# TYPE media_ai_pool_queue_cap gauge\n")
	fmt.Fprintf(w, "media_ai_pool_queue_cap %d\n", s.pool.QueueCap())

	fmt.Fprintf(w, "# HELP media_ai_pool_submitted_total Total jobs submitted to worker pool\n")
	fmt.Fprintf(w, "# TYPE media_ai_pool_submitted_total counter\n")
	fmt.Fprintf(w, "media_ai_pool_submitted_total %d\n", ps.Submitted)

	fmt.Fprintf(w, "# HELP media_ai_pool_dropped_total Total jobs dropped from worker pool\n")
	fmt.Fprintf(w, "# TYPE media_ai_pool_dropped_total counter\n")
	fmt.Fprintf(w, "media_ai_pool_dropped_total %d\n", ps.Dropped)

	fmt.Fprintf(w, "# HELP media_ai_pool_processed_total Total jobs processed by worker pool\n")
	fmt.Fprintf(w, "# TYPE media_ai_pool_processed_total counter\n")
	fmt.Fprintf(w, "media_ai_pool_processed_total %d\n", ps.Processed)

	fmt.Fprintf(w, "# HELP media_ai_pool_decode_errors_total Total decode errors in worker pool\n")
	fmt.Fprintf(w, "# TYPE media_ai_pool_decode_errors_total counter\n")
	fmt.Fprintf(w, "media_ai_pool_decode_errors_total %d\n", ps.DecodeErrors)

	fmt.Fprintf(w, "# HELP media_ai_dispatcher_queue_len Result dispatcher pending queue depth\n")
	fmt.Fprintf(w, "# TYPE media_ai_dispatcher_queue_len gauge\n")
	fmt.Fprintf(w, "media_ai_dispatcher_queue_len %d\n", s.dispatcher.QueueLen())
	fmt.Fprintln(w, "# HELP media_ai_dispatcher_task_queue_len Pending callback results by independent task lane")
	fmt.Fprintln(w, "# TYPE media_ai_dispatcher_task_queue_len gauge")
	for _, task := range []string{"default", "video_segment", "gesture_recognition"} {
		fmt.Fprintf(w, "media_ai_dispatcher_task_queue_len{task=%q} %d\n", task, s.dispatcher.TaskQueueLen(task))
	}

	fmt.Fprintf(w, "# HELP media_ai_dispatcher_pushed_total Total results pushed into dispatcher queue\n")
	fmt.Fprintf(w, "# TYPE media_ai_dispatcher_pushed_total counter\n")
	fmt.Fprintf(w, "media_ai_dispatcher_pushed_total %d\n", ds.Pushed)

	fmt.Fprintf(w, "# HELP media_ai_dispatcher_sent_total Total results sent via callback\n")
	fmt.Fprintf(w, "# TYPE media_ai_dispatcher_sent_total counter\n")
	fmt.Fprintf(w, "media_ai_dispatcher_sent_total %d\n", ds.Sent)

	fmt.Fprintf(w, "# HELP media_ai_dispatcher_send_errors_total Total callback send errors\n")
	fmt.Fprintf(w, "# TYPE media_ai_dispatcher_send_errors_total counter\n")
	fmt.Fprintf(w, "media_ai_dispatcher_send_errors_total %d\n", ds.SendErrors)

	if s.portAlloc != nil {
		fmt.Fprintf(w, "# HELP media_ai_rtp_ports_available Free RTP ports in pool\n")
		fmt.Fprintf(w, "# TYPE media_ai_rtp_ports_available gauge\n")
		fmt.Fprintf(w, "media_ai_rtp_ports_available %d\n", s.portAlloc.Available())

		fmt.Fprintf(w, "# HELP media_ai_rtp_ports_total Total RTP ports in pool\n")
		fmt.Fprintf(w, "# TYPE media_ai_rtp_ports_total gauge\n")
		fmt.Fprintf(w, "media_ai_rtp_ports_total %d\n", s.portAlloc.Total())
	}

	// --- AI stream errors ---
	aiSt := s.aiMgr.Stats()
	fmt.Fprintf(w, "# HELP media_ai_ai_send_errors_total Total AI gRPC send errors\n")
	fmt.Fprintf(w, "# TYPE media_ai_ai_send_errors_total counter\n")
	fmt.Fprintf(w, "media_ai_ai_send_errors_total %d\n", aiSt.TotalSendErrors)

	fmt.Fprintf(w, "# HELP media_ai_ai_recv_errors_total Total AI gRPC recv errors\n")
	fmt.Fprintf(w, "# TYPE media_ai_ai_recv_errors_total counter\n")
	fmt.Fprintf(w, "media_ai_ai_recv_errors_total %d\n", aiSt.TotalRecvErrors)

	fmt.Fprintf(w, "# HELP media_ai_ai_reconnects_total Total AI gRPC stream reconnect attempts\n")
	fmt.Fprintf(w, "# TYPE media_ai_ai_reconnects_total counter\n")
	fmt.Fprintf(w, "media_ai_ai_reconnects_total %d\n", aiSt.TotalRetries)

	fmt.Fprintf(w, "# HELP media_ai_ai_audio_dropped_total AudioChunks dropped due to AI send queue full\n")
	fmt.Fprintf(w, "# TYPE media_ai_ai_audio_dropped_total counter\n")
	fmt.Fprintf(w, "media_ai_ai_audio_dropped_total %d\n", aiSt.TotalAudioDropped)

	fmt.Fprintf(w, "# HELP media_ai_ai_partial_dropped_total Partial ASR results dropped due to result queue full\n")
	fmt.Fprintf(w, "# TYPE media_ai_ai_partial_dropped_total counter\n")
	fmt.Fprintf(w, "media_ai_ai_partial_dropped_total %d\n", aiSt.TotalPartialDropped)

	fmt.Fprintf(w, "# HELP media_ai_ai_first_result_ms_total Cumulative sum of first-result latencies per stream (ms)\n")
	fmt.Fprintf(w, "# TYPE media_ai_ai_first_result_ms_total counter\n")
	fmt.Fprintf(w, "media_ai_ai_first_result_ms_total %d\n", aiSt.LatencyFirstSum)

	fmt.Fprintf(w, "# HELP media_ai_ai_first_result_count_total Number of streams that produced at least one result\n")
	fmt.Fprintf(w, "# TYPE media_ai_ai_first_result_count_total counter\n")
	fmt.Fprintf(w, "media_ai_ai_first_result_count_total %d\n", aiSt.LatencyFirstCount)

	// --- Result metrics ---
	fmt.Fprintf(w, "# HELP media_ai_result_partial_total Total partial recognition results delivered\n")
	fmt.Fprintf(w, "# TYPE media_ai_result_partial_total counter\n")
	fmt.Fprintf(w, "media_ai_result_partial_total %d\n", ds.SentPartial)

	fmt.Fprintf(w, "# HELP media_ai_result_final_total Total final recognition results delivered\n")
	fmt.Fprintf(w, "# TYPE media_ai_result_final_total counter\n")
	fmt.Fprintf(w, "media_ai_result_final_total %d\n", ds.SentFinal)

	fmt.Fprintf(w, "# HELP media_ai_result_queue_dropped_total Total results dropped from dispatcher queue\n")
	fmt.Fprintf(w, "# TYPE media_ai_result_queue_dropped_total counter\n")
	fmt.Fprintf(w, "media_ai_result_queue_dropped_total %d\n", ds.Dropped)

	// --- Callback retries ---
	fmt.Fprintf(w, "# HELP media_ai_callback_retry_total Total HTTP callback retry attempts\n")
	fmt.Fprintf(w, "# TYPE media_ai_callback_retry_total counter\n")
	fmt.Fprintf(w, "media_ai_callback_retry_total %d\n", s.callbackRetries())

	// --- RTP ingress ---
	if s.ingress != nil {
		ist := s.ingress.Stats()
		fmt.Fprintf(w, "# HELP media_ai_rtp_packets_total Total RTP packets received from UDP\n")
		fmt.Fprintf(w, "# TYPE media_ai_rtp_packets_total counter\n")
		fmt.Fprintf(w, "media_ai_rtp_packets_total %d\n", ist.Received)

		fmt.Fprintf(w, "# HELP media_ai_rtp_packets_routed_total RTP packets successfully routed to session\n")
		fmt.Fprintf(w, "# TYPE media_ai_rtp_packets_routed_total counter\n")
		fmt.Fprintf(w, "media_ai_rtp_packets_routed_total %d\n", ist.Routed)

		fmt.Fprintf(w, "# HELP media_ai_rtp_queue_dropped_total RTP packets dropped: session queue full (shared UDP ingress)\n")
		fmt.Fprintf(w, "# TYPE media_ai_rtp_queue_dropped_total counter\n")
		fmt.Fprintf(w, "media_ai_rtp_queue_dropped_total %d\n", ist.DroppedQueueFull)
	}

	fmt.Fprintf(w, "# HELP media_ai_rtp_session_queue_dropped_total RTP packets dropped: session queue full (per-session UDP listener)\n")
	fmt.Fprintf(w, "# TYPE media_ai_rtp_session_queue_dropped_total counter\n")
	fmt.Fprintf(w, "media_ai_rtp_session_queue_dropped_total %d\n", s.sessionMgr.TotalPacketQueueDropped())

	fmt.Fprintf(w, "# HELP media_ai_rtp_video_session_queue_dropped_total RTP video packets dropped: session video queue full (per-session UDP listener)\n")
	fmt.Fprintf(w, "# TYPE media_ai_rtp_video_session_queue_dropped_total counter\n")
	fmt.Fprintf(w, "media_ai_rtp_video_session_queue_dropped_total %d\n", s.sessionMgr.TotalVideoPacketQueueDropped())

	if s.ingress != nil {
		ist := s.ingress.Stats()

		fmt.Fprintf(w, "# HELP media_ai_rtp_unknown_ssrc_total RTP packets dropped: no matching session SSRC\n")
		fmt.Fprintf(w, "# TYPE media_ai_rtp_unknown_ssrc_total counter\n")
		fmt.Fprintf(w, "media_ai_rtp_unknown_ssrc_total %d\n", ist.DroppedUnknownSSRC)

		fmt.Fprintf(w, "# HELP media_ai_rtp_parse_errors_total RTP packets dropped: parse error\n")
		fmt.Fprintf(w, "# TYPE media_ai_rtp_parse_errors_total counter\n")
		fmt.Fprintf(w, "media_ai_rtp_parse_errors_total %d\n", ist.DroppedParseError)
	}

	// --- System metrics ---
	fmt.Fprintf(w, "# HELP media_ai_goroutines_current Current number of goroutines\n")
	fmt.Fprintf(w, "# TYPE media_ai_goroutines_current gauge\n")
	fmt.Fprintf(w, "media_ai_goroutines_current %d\n", runtime.NumGoroutine())

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Fprintf(w, "# HELP media_ai_memory_usage_bytes Current heap allocation in bytes\n")
	fmt.Fprintf(w, "# TYPE media_ai_memory_usage_bytes gauge\n")
	fmt.Fprintf(w, "media_ai_memory_usage_bytes %d\n", ms.Alloc)

	fmt.Fprintf(w, "# HELP media_ai_gateway_nodes_registered Registered gateway nodes in registry\n")
	fmt.Fprintf(w, "# TYPE media_ai_gateway_nodes_registered gauge\n")
	fmt.Fprintf(w, "media_ai_gateway_nodes_registered %d\n", s.registry.Len())

	// --- Jitter buffer aggregate (type assert — graceful nếu coord không implement) ---
	type jitterStatsGetter interface {
		AggregateJitterStats() (received, released, dropped, lost uint64)
	}
	if js, ok := s.coord.(jitterStatsGetter); ok {
		recv, rel, drop, lost := js.AggregateJitterStats()
		fmt.Fprintf(w, "# HELP media_ai_jitter_received_total Total RTP packets received by jitter buffers\n")
		fmt.Fprintf(w, "# TYPE media_ai_jitter_received_total counter\n")
		fmt.Fprintf(w, "media_ai_jitter_received_total %d\n", recv)

		fmt.Fprintf(w, "# HELP media_ai_jitter_released_total Total packets released from jitter buffers in order\n")
		fmt.Fprintf(w, "# TYPE media_ai_jitter_released_total counter\n")
		fmt.Fprintf(w, "media_ai_jitter_released_total %d\n", rel)

		fmt.Fprintf(w, "# HELP media_ai_jitter_dropped_total Total packets dropped (late arrival, duplicate, output full)\n")
		fmt.Fprintf(w, "# TYPE media_ai_jitter_dropped_total counter\n")
		fmt.Fprintf(w, "media_ai_jitter_dropped_total %d\n", drop)

		fmt.Fprintf(w, "# HELP media_ai_jitter_lost_total Total detected sequence gaps (packets never arrived)\n")
		fmt.Fprintf(w, "# TYPE media_ai_jitter_lost_total counter\n")
		fmt.Fprintf(w, "media_ai_jitter_lost_total %d\n", lost)
	}

	fmt.Fprintf(w, "# HELP media_ai_scrape_timestamp_ms Unix millisecond timestamp of this scrape\n")
	fmt.Fprintf(w, "# TYPE media_ai_scrape_timestamp_ms gauge\n")
	fmt.Fprintf(w, "media_ai_scrape_timestamp_ms %d\n", now)
}

func (s *Server) stats(c *gin.Context) {
	ps := s.pool.Stats()
	ds := s.dispatcher.Stats()
	aiSt := s.aiMgr.Stats()
	c.JSON(http.StatusOK, StatsResponse{
		Sessions:  s.sessionMgr.Count(),
		AIStreams: s.aiMgr.Count(),
		Pool: PoolStats{
			Submitted:      ps.Submitted,
			Dropped:        ps.Dropped,
			Processed:      ps.Processed,
			DecodeErrors:   ps.DecodeErrors,
			ActiveSessions: s.pool.SessionCount(),
			QueueLen:       s.pool.QueueLen(),
		},
		Dispatcher: DispatcherStats{
			Pushed:      ds.Pushed,
			Dropped:     ds.Dropped,
			Sent:        ds.Sent,
			SentPartial: ds.SentPartial,
			SentFinal:   ds.SentFinal,
			SendErrors:  ds.SendErrors,
			QueueLen:    s.dispatcher.QueueLen(),
		},
		AI: AIStats{
			TotalSendErrors: aiSt.TotalSendErrors,
			TotalRecvErrors: aiSt.TotalRecvErrors,
			TotalRetries:    aiSt.TotalRetries,
		},
	})
}

// describeVideoWorker bổ sung dịch vụ, task và số stream đang giữ cho địa chỉ là AI video
// trong registry; địa chỉ không phải AI video giữ nguyên số stream audio.
func (s *Server) describeVideoWorker(conn *AIWorkerConn) {
	if s.videoRegistry == nil {
		return
	}
	video, active := false, 0
	for _, w := range s.videoRegistry.List() {
		if w.Addr != conn.Addr || !slices.Contains(w.MediaTypes, "video") {
			continue
		}
		video = true
		active += w.ActiveStreams
		conn.Services = append(conn.Services, w.Services...)
		conn.Tasks = append(conn.Tasks, w.Tasks...)
	}
	if !video {
		return
	}
	conn.ActiveStream = active
	slices.Sort(conn.Services)
	conn.Services = slices.Compact(conn.Services)
	slices.Sort(conn.Tasks)
	conn.Tasks = slices.Compact(conn.Tasks)
}

func (s *Server) connections(c *gin.Context) {
	resp := ConnectionsResponse{}

	// AI gRPC workers.
	if s.grpcPool != nil {
		aiSt := s.aiMgr.Stats()
		addrs := s.grpcPool.Addrs()
		slices.Sort(addrs)
		for _, addr := range addrs {
			conn := AIWorkerConn{
				Addr:         addr,
				State:        s.grpcPool.State(addr).String(),
				ActiveStream: s.aiMgr.Count(),
				Latency: AILatencyStats{
					AvgFirstResultMs: aiSt.AvgFirstResultMs(),
				},
			}
			s.describeVideoWorker(&conn)
			resp.AIWorkers = append(resp.AIWorkers, conn)
		}
	}

	// Callback HTTP/2.
	if s.callbackClient != nil {
		st := s.callbackClient.PreconnectState()
		resp.Callback = CallbackConn{
			URL:       st.URL,
			Connected: st.Connected,
			Error:     st.Error,
		}
		if !st.PreconnectAt.IsZero() {
			resp.Callback.PreconnectAt = st.PreconnectAt.UTC().Format(time.RFC3339)
		}
	}

	// RTP connections.
	if s.portAlloc != nil {
		total := s.portAlloc.Total()
		avail := s.portAlloc.Available()
		resp.RTP = RTPConnSummary{
			PerSessionOpen:     total - avail,
			PerSessionCapacity: total,
			SharedIngress:      s.ingress != nil,
		}
	} else {
		resp.RTP = RTPConnSummary{
			SharedIngress: s.ingress != nil,
		}
	}

	c.JSON(http.StatusOK, resp)
}

func sessionToResponse(sess *session.Session) SessionResponse {
	return SessionResponse{
		SessionID:  sess.ID,
		Status:     string(sess.Status()),
		SourceType: sess.SourceType,
		Codec:      sess.Codec,
		SampleRate: sess.SampleRate,
		Channels:   sess.Channels,
		Language:   sess.Language,
		Task:       sess.Task,
		CreatedAt:  sess.CreatedAt,
	}
}

// buildNonDcMedia xây dựng SDP mô tả RTP endpoint của gateway (dùng cho 3GPP MRM API).
// DCAS dùng kết quả này làm remoteNonDcMedia khi gọi MF MRM POST /contexts.
func buildNonDcMedia(codec string, sampleRate, channels, rtpPort int, payloadType uint8) *NonDcMedia {
	upper := strings.ToUpper(codec)

	ch := channels
	if ch <= 0 {
		ch = 1
	}

	pt := int(payloadType) // 0 = dùng default của codec

	var maxptime int
	var rtpmap, fmtp string

	switch upper {
	case "PCMU":
		// PT 0 là static assignment cho PCMU — không thay đổi dù payloadType=0
		maxptime = 20
		if ch > 1 {
			rtpmap = fmt.Sprintf("rtpmap:%d PCMU/%d/%d", pt, sampleRate, ch)
		} else {
			rtpmap = fmt.Sprintf("rtpmap:%d PCMU/%d", pt, sampleRate)
		}

	case "PCMA":
		if pt == 0 {
			pt = 8
		}
		maxptime = 20
		if ch > 1 {
			rtpmap = fmt.Sprintf("rtpmap:%d PCMA/%d/%d", pt, sampleRate, ch)
		} else {
			rtpmap = fmt.Sprintf("rtpmap:%d PCMA/%d", pt, sampleRate)
		}

	case "OPUS":
		if pt == 0 {
			pt = 111
		}
		if ch < 2 {
			ch = 2 // Opus default 2 channels trong SDP
		}
		maxptime = 60
		rtpmap = fmt.Sprintf("rtpmap:%d opus/%d/%d", pt, sampleRate, ch)
		fmtp = fmt.Sprintf("fmtp:%d minptime=10;useinbandfec=1", pt)

	case "AMR", "AMR-NB":
		if pt == 0 {
			pt = 96
		}
		maxptime = 80
		rtpmap = fmt.Sprintf("rtpmap:%d AMR/8000", pt)
		fmtp = fmt.Sprintf("fmtp:%d octet-align=1", pt)

	case "AMR-WB", "AMRWB":
		if pt == 0 {
			pt = 97
		}
		maxptime = 80
		rtpmap = fmt.Sprintf("rtpmap:%d AMR-WB/16000", pt)
		fmtp = fmt.Sprintf("fmtp:%d octet-align=1", pt)

	case "EVS":
		if pt == 0 {
			pt = 100
		}
		if sampleRate == 0 {
			sampleRate = 16000
		}
		maxptime = 100
		rtpmap = fmt.Sprintf("rtpmap:%d EVS/%d", pt, sampleRate)
		bw := "wb"
		if sampleRate <= 8000 {
			bw = "nb"
		}
		fmtp = fmt.Sprintf("fmtp:%d br=13.2;bw=%s", pt, bw)

	default:
		if pt == 0 {
			pt = 96
		}
		rtpmap = fmt.Sprintf("rtpmap:%d %s/%d", pt, codec, sampleRate)
	}

	if maxptime == 0 {
		maxptime = 20
	}

	aLines := []string{rtpmap}
	if fmtp != "" {
		aLines = append(aLines, fmtp)
	}
	aLines = append(aLines, "ptime:20", fmt.Sprintf("maxptime:%d", maxptime), "sendrecv")

	return &NonDcMedia{
		SDPMLine:  fmt.Sprintf("audio %d RTP/AVP %d", rtpPort, pt),
		SDPALines: aLines,
	}
}

// buildVideoNonDcMedia dựng khối SDP m=video cho H264. Đối xứng với
// buildNonDcMedia (audio) nhưng m-line là "video" và không có ptime/maxptime
// (không áp dụng cho video). packetizationMode=1 (FU-A/STAP-A, non-interleaved)
// là mode phổ biến nhất, tương thích rộng nhất với SBC/MGW/browser.
func buildVideoNonDcMedia(rtpPort int, payloadType uint8) *NonDcMedia {
	pt := int(payloadType)
	if pt == 0 {
		pt = 96 // dynamic payload type mặc định cho H264 (không có static PT theo RFC 3551)
	}
	aLines := []string{
		fmt.Sprintf("rtpmap:%d H264/90000", pt),
		fmt.Sprintf("fmtp:%d packetization-mode=1;profile-level-id=42e01f", pt),
		"sendrecv",
	}
	return &NonDcMedia{
		SDPMLine:  fmt.Sprintf("video %d RTP/AVP %d", rtpPort, pt),
		SDPALines: aLines,
	}
}
