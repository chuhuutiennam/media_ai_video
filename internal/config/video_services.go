package config

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"media-ai-gateway/internal/ai"
	"media-ai-gateway/internal/controlplane"
	"media-ai-gateway/internal/coordinator"
)

// VideoServiceSection khai báo một dịch vụ video người dùng đăng ký (video.services.<tên>):
// đúng một AI service (1 dịch vụ ↔ 1 AI). Xem docs/giai-phap-video-service-catalog.md.
type VideoServiceSection struct {
	Enabled    *bool  `yaml:"enabled"`     // nil = true; false → ANSWER trả 200 {} không tạo session
	Legs       string `yaml:"legs"`        // tcore | both (mặc định tcore)
	EffectType string `yaml:"effect_type"` // chỉ cho ai.task=video_segment; rỗng = mặc định code (bg_blur)

	// Thông số AI của dịch vụ (§4.7 tài liệu giải pháp). Bỏ trống hoặc 0 = mặc định code.
	Segmentation *VideoSegmentationProfile `yaml:"segmentation"` // chỉ cho ai.task=video_segment
	Gesture      *GestureProfile           `yaml:"gesture"`      // chỉ cho ai.task=gesture_recognition

	AI VideoServiceAISection `yaml:"ai"`
}

// VideoSegmentationProfile: thông số AI segmentation của một dịch vụ (ai.task video_segment).
type VideoSegmentationProfile struct {
	InferFPS    int `yaml:"infer_fps"`    // 1..60; tần suất lấy mẫu frame gửi AI
	InferWidth  int `yaml:"infer_width"`  // 16..1920; kích thước JPEG gửi AI và mask trả về
	InferHeight int `yaml:"infer_height"` // 16..1920
}

// GestureProfile: thông số AI gesture (ai.task gesture_recognition). Dùng ở
// video.services.<tên>.gesture và, kèm enabled, ở video.gesture của config cũ / AI Engine.
type GestureProfile struct {
	InferFPS       int `yaml:"infer_fps"`        // 1..60; tần suất lấy mẫu frame gửi AI
	MaxDimension   int `yaml:"max_dimension"`    // 32..1920; cạnh dài nhất JPEG gửi AI
	MaxHands       int `yaml:"max_hands"`        // 1..8; gửi worker, gateway từ chối kết quả nhiều tay hơn
	FrameQueueSize int `yaml:"frame_queue_size"` // 1..32; hàng đợi frame gesture mỗi session
}

// withDefaults: trường 0 lấy từ d (mặc định code); số âm giữ nguyên để validate báo lỗi.
func (p VideoSegmentationProfile) withDefaults(d VideoSegmentationProfile) VideoSegmentationProfile {
	if p.InferFPS == 0 {
		p.InferFPS = d.InferFPS
	}
	if p.InferWidth == 0 {
		p.InferWidth = d.InferWidth
	}
	if p.InferHeight == 0 {
		p.InferHeight = d.InferHeight
	}
	return p
}

func (p VideoSegmentationProfile) validate(prefix string) error {
	return checkLimits(prefix,
		intLimit{"infer_fps", p.InferFPS, 1, 60},
		intLimit{"infer_width", p.InferWidth, 16, 1920},
		intLimit{"infer_height", p.InferHeight, 16, 1920})
}

// withDefaults: trường 0 lấy từ d (mặc định code); số âm giữ nguyên để validate báo lỗi.
func (p GestureProfile) withDefaults(d GestureProfile) GestureProfile {
	if p.InferFPS == 0 {
		p.InferFPS = d.InferFPS
	}
	if p.MaxDimension == 0 {
		p.MaxDimension = d.MaxDimension
	}
	if p.MaxHands == 0 {
		p.MaxHands = d.MaxHands
	}
	if p.FrameQueueSize == 0 {
		p.FrameQueueSize = d.FrameQueueSize
	}
	return p
}

func (p GestureProfile) validate(prefix string) error {
	return checkLimits(prefix,
		intLimit{"infer_fps", p.InferFPS, 1, 60},
		intLimit{"max_dimension", p.MaxDimension, 32, 1920},
		intLimit{"max_hands", p.MaxHands, 1, 8},
		intLimit{"frame_queue_size", p.FrameQueueSize, 1, 32})
}

type intLimit struct {
	name          string
	value, lo, hi int
}

// checkLimits trả lỗi đầu tiên dạng "<prefix>.<tên> must be lo..hi, got v".
func checkLimits(prefix string, limits ...intLimit) error {
	for _, l := range limits {
		if l.value < l.lo || l.value > l.hi {
			return fmt.Errorf("%s.%s must be %d..%d, got %d", prefix, l.name, l.lo, l.hi, l.value)
		}
	}
	return nil
}

// sharedVideoAIKeys: khoá thông số AI chung (config cũ / AI Engine) và chỗ của chúng khi có video.services.
var sharedVideoAIKeys = []struct{ key, moveTo string }{
	{"gesture", "video.services.<name>.gesture"},
	{"infer_fps", "video.services.<name>.segmentation.infer_fps"},
	{"infer_width", "video.services.<name>.segmentation.infer_width"},
	{"infer_height", "video.services.<name>.segmentation.infer_height"},
	{"default_effect_type", "video.services.<name>.effect_type"},
}

// rejectSharedVideoAIKeys: có video.services thì thông số AI nằm trong từng dịch vụ. Khoá chung cũ
// bị từ chối vì loader YAML không báo khoá lạ — nếu bỏ qua, dịch vụ chạy mặc định mà không ai biết.
func rejectSharedVideoAIKeys(data []byte) error {
	var raw struct {
		Video map[string]any `yaml:"video"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("config: read video keys: %w", err)
	}
	var moves []string
	for _, k := range sharedVideoAIKeys {
		if _, ok := raw.Video[k.key]; ok {
			moves = append(moves, "video."+k.key+" to "+k.moveTo)
		}
	}
	if len(moves) != 0 {
		return fmt.Errorf("video.services: AI parameters are set per service, move %s", strings.Join(moves, ", "))
	}
	return nil
}

// VideoServiceAISection là AI service duy nhất của dịch vụ. Nhiều target = nhiều instance
// của cùng AI service, chọn theo tải.
type VideoServiceAISection struct {
	Task       string   `yaml:"task"`        // video_segment | gesture_recognition
	Targets    []string `yaml:"targets"`     // host:port, >= 1
	MaxStreams int      `yaml:"max_streams"` // mỗi target; 0 = video.ai.max_active_streams
}

const (
	videoLegsTCore = "tcore"
	videoLegsBoth  = "both"
)

var videoServiceNamePattern = regexp.MustCompile(`^[a-z0-9_]+$`)

func (s VideoServiceSection) enabled() bool { return s.Enabled == nil || *s.Enabled }

// normalizeVideoServices validate và chuẩn hoá video.services (tên, legs, task về chữ thường).
func (c *GatewayConfig) normalizeVideoServices() error {
	if len(c.Video.Services) == 0 {
		return nil
	}
	if !c.Video.Enabled {
		return fmt.Errorf("video.services requires video.enabled=true")
	}
	if len(c.Video.AI.Workers) != 0 || strings.TrimSpace(c.Video.AI.GRPCTarget) != "" ||
		strings.TrimSpace(c.Video.AI.EngineTarget) != "" || c.Video.Gesture.Enabled {
		return fmt.Errorf("video.services cannot be combined with video.ai.workers, video.ai.grpc_target, video.ai.engine_target or video.gesture.enabled")
	}
	defaults := Default().Video // mặc định code cho thông số AI bỏ trống
	normalized := make(map[string]VideoServiceSection, len(c.Video.Services))
	for rawName, s := range c.Video.Services {
		name := strings.ToLower(strings.TrimSpace(rawName))
		prefix := "video.services." + name
		switch {
		case !videoServiceNamePattern.MatchString(name):
			return fmt.Errorf("video.services: invalid service name %q (use [a-z0-9_])", rawName)
		case controlplane.IsAudioService(name):
			return fmt.Errorf("%s: %q is an audio service and cannot be a video service", prefix, name)
		}
		if _, dup := normalized[name]; dup {
			return fmt.Errorf("video.services: duplicate service %q", name)
		}
		s.Legs = strings.ToLower(strings.TrimSpace(s.Legs))
		if s.Legs == "" {
			s.Legs = videoLegsTCore
		}
		if s.Legs != videoLegsTCore && s.Legs != videoLegsBoth {
			return fmt.Errorf("%s.legs must be tcore or both, got %q", prefix, s.Legs)
		}
		s.AI.Task = strings.ToLower(strings.TrimSpace(s.AI.Task))
		if s.AI.Task != controlplane.VideoTaskSegment && s.AI.Task != controlplane.VideoTaskGesture {
			return fmt.Errorf("%s.ai.task must be video_segment or gesture_recognition, got %q", prefix, s.AI.Task)
		}
		s.EffectType = strings.TrimSpace(s.EffectType)
		if s.EffectType != "" && s.AI.Task != controlplane.VideoTaskSegment {
			return fmt.Errorf("%s.effect_type is only valid with ai.task video_segment", prefix)
		}
		if err := s.resolveAIProfile(prefix, defaults); err != nil {
			return err
		}
		if len(s.AI.Targets) == 0 {
			return fmt.Errorf("%s.ai.targets requires at least one host:port", prefix)
		}
		targets := make([]string, 0, len(s.AI.Targets))
		for _, t := range s.AI.Targets {
			t = strings.TrimSpace(t)
			if t == "" || slices.Contains(targets, t) {
				return fmt.Errorf("%s.ai.targets: empty or duplicate target %q", prefix, t)
			}
			targets = append(targets, t)
		}
		s.AI.Targets = targets
		if s.AI.MaxStreams < 0 {
			return fmt.Errorf("%s.ai.max_streams must be >= 0, got %d", prefix, s.AI.MaxStreams)
		}
		normalized[name] = s
	}
	c.Video.Services = normalized
	return nil
}

// resolveAIProfile: chỉ nhóm thông số đúng AI task của dịch vụ được khai báo; điền mặc định code
// (d) cho trường bỏ trống rồi kiểm tra giới hạn. Sau bước này nhóm của task luôn khác nil.
func (s *VideoServiceSection) resolveAIProfile(prefix string, d VideoSection) error {
	switch s.AI.Task {
	case controlplane.VideoTaskSegment:
		if s.Gesture != nil {
			return fmt.Errorf("%s.gesture is only valid with ai.task gesture_recognition", prefix)
		}
		var p VideoSegmentationProfile
		if s.Segmentation != nil {
			p = *s.Segmentation
		}
		p = p.withDefaults(VideoSegmentationProfile{InferFPS: d.InferFPS, InferWidth: d.InferWidth, InferHeight: d.InferHeight})
		if err := p.validate(prefix + ".segmentation"); err != nil {
			return err
		}
		s.Segmentation = &p
	case controlplane.VideoTaskGesture:
		if s.Segmentation != nil {
			return fmt.Errorf("%s.segmentation is only valid with ai.task video_segment", prefix)
		}
		var g GestureProfile
		if s.Gesture != nil {
			g = *s.Gesture
		}
		g = g.withDefaults(d.Gesture.GestureProfile)
		if err := g.validate(prefix + ".gesture"); err != nil {
			return err
		}
		s.Gesture = &g
	}
	return nil
}

// videoServiceProfiles: thông số AI đã điền đủ của từng dịch vụ trong video.services, cho coordinator.
// Nhóm không thuộc task của dịch vụ giữ giá trị chung (mặc định code), không được dùng tới.
// Không có video.services (config cũ, AI Engine) → nil: coordinator dùng khoá chung video.*.
func (c GatewayConfig) videoServiceProfiles() map[string]coordinator.VideoServiceProfile {
	if len(c.Video.Services) == 0 {
		return nil
	}
	out := make(map[string]coordinator.VideoServiceProfile, len(c.Video.Services))
	for name, s := range c.Video.Services {
		g := c.Video.Gesture
		p := coordinator.VideoServiceProfile{
			InferWidth: c.Video.InferWidth, InferHeight: c.Video.InferHeight, InferFPS: c.Video.InferFPS,
			GestureFPS: g.InferFPS, GestureMaxDimension: g.MaxDimension, GestureMaxHands: g.MaxHands, GestureQueueSize: g.FrameQueueSize,
		}
		if seg := s.Segmentation; seg != nil {
			p.InferWidth, p.InferHeight, p.InferFPS = seg.InferWidth, seg.InferHeight, seg.InferFPS
		}
		if sg := s.Gesture; sg != nil {
			p.GestureFPS, p.GestureMaxDimension, p.GestureMaxHands, p.GestureQueueSize = sg.InferFPS, sg.MaxDimension, sg.MaxHands, sg.FrameQueueSize
		}
		out[name] = p
	}
	return out
}

// gestureInUse: gesture được dùng bởi config cũ (video.gesture.enabled) hoặc một dịch vụ trong danh mục.
func (c GatewayConfig) gestureInUse() bool {
	if c.Video.Gesture.Enabled {
		return true
	}
	for _, s := range c.Video.Services {
		if s.AI.Task == controlplane.VideoTaskGesture {
			return true
		}
	}
	return false
}

// ToVideoServices trả danh mục dịch vụ video cho control plane. Không có video.services
// thì dùng danh mục mặc định tương đương config cũ (controlplane.DefaultVideoServices).
func (c GatewayConfig) ToVideoServices() map[string]controlplane.VideoService {
	if len(c.Video.Services) == 0 {
		return controlplane.DefaultVideoServices(c.Video.Gesture.Enabled)
	}
	out := make(map[string]controlplane.VideoService, len(c.Video.Services))
	for name, s := range c.Video.Services {
		effect := s.EffectType
		if s.AI.Task == controlplane.VideoTaskSegment && effect == "" {
			effect = c.Video.DefaultEffectType
		}
		out[name] = controlplane.VideoService{
			Enabled:    s.enabled(),
			SingleLeg:  s.Legs == videoLegsTCore,
			EffectType: effect,
			AITask:     s.AI.Task,
		}
	}
	return out
}

// videoServiceWorkers: mỗi target của dịch vụ đang bật thành một worker gắn đúng dịch vụ đó.
// Thứ tự ổn định theo tên dịch vụ rồi thứ tự target.
func (c GatewayConfig) videoServiceWorkers() []ai.WorkerInfo {
	names := make([]string, 0, len(c.Video.Services))
	for name := range c.Video.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	var workers []ai.WorkerInfo
	for _, name := range names {
		s := c.Video.Services[name]
		if !s.enabled() {
			continue
		}
		limit := s.AI.MaxStreams
		if limit == 0 {
			limit = c.Video.AI.MaxActiveStreams
		}
		for _, target := range s.AI.Targets {
			workers = append(workers, ai.WorkerInfo{
				ID: "video-service:" + name + ":" + target, Addr: target,
				Tasks: []string{s.AI.Task}, Services: []string{name}, MaxStreams: limit,
				MediaTypes: []string{"video"}, InputFormats: []string{"jpeg"}, Protocols: []string{"video_segment"},
			})
		}
	}
	return workers
}
