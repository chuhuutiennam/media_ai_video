package config

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"media-ai-gateway/internal/ai"
	"media-ai-gateway/internal/controlplane"
)

// VideoServiceSection khai báo một dịch vụ video người dùng đăng ký (video.services.<tên>):
// đúng một AI service (1 dịch vụ ↔ 1 AI). Xem docs/giai-phap-video-service-catalog.md.
type VideoServiceSection struct {
	Enabled    *bool                 `yaml:"enabled"`     // nil = true; false → ANSWER trả 200 {} không tạo session
	Legs       string                `yaml:"legs"`        // tcore | both (mặc định tcore)
	EffectType string                `yaml:"effect_type"` // chỉ cho ai.task=video_segment; rỗng = video.default_effect_type
	AI         VideoServiceAISection `yaml:"ai"`
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
