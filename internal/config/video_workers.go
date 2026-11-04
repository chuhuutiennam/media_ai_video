package config

import (
	"fmt"
	"strings"

	"media-ai-gateway/internal/ai"
)

// VideoWorkerSection declares only the tasks this endpoint actually implements.
// All current video workers use the shared JPEG/VideoStream.Segment contract.
type VideoWorkerSection struct {
	ID         string   `yaml:"id"`
	GRPCTarget string   `yaml:"grpc_target"`
	Tasks      []string `yaml:"tasks"`       // video_segment, gesture_recognition
	MaxStreams int      `yaml:"max_streams"` // 0 inherits video.ai.max_active_streams.
}

func (c *GatewayConfig) normalizeVideoWorkers() error {
	c.Video.AI.EngineTarget = strings.TrimSpace(c.Video.AI.EngineTarget)
	if c.Video.AI.EngineTarget != "" {
		if len(c.Video.AI.Workers) != 0 || strings.TrimSpace(c.Video.AI.GRPCTarget) != "" {
			return fmt.Errorf("video.ai.engine_target cannot be combined with workers or grpc_target")
		}
		if c.Video.AI.SourceMaxDimension < 32 || c.Video.AI.SourceMaxDimension > 1920 ||
			c.Video.InferFPS < 1 || c.Video.InferFPS > 60 || c.Video.AI.OpenTimeoutMs < 1 || c.Video.AI.SendTimeoutMs < 1 ||
			c.Video.InferWidth < 1 || c.Video.InferHeight < 1 || max(c.Video.InferWidth, c.Video.InferHeight) > c.Video.AI.SourceMaxDimension ||
			(c.Video.Gesture.Enabled && c.Video.Gesture.MaxDimension > c.Video.AI.SourceMaxDimension) {
			return fmt.Errorf("invalid engine source dimensions/FPS/timeouts; source must cover task profiles")
		}
	}
	c.Video.AI.GRPCTarget = strings.TrimSpace(c.Video.AI.GRPCTarget)
	ids := make(map[string]bool)
	for i := range c.Video.AI.Workers {
		w := &c.Video.AI.Workers[i]
		w.ID, w.GRPCTarget = strings.TrimSpace(w.ID), strings.TrimSpace(w.GRPCTarget)
		if w.ID == "" || w.GRPCTarget == "" || len(w.Tasks) == 0 || w.MaxStreams < 0 {
			return fmt.Errorf("video.ai.workers[%d] requires id, grpc_target, tasks and nonnegative max_streams", i)
		}
		if ids[w.ID] {
			return fmt.Errorf("video.ai.workers: duplicate id %q", w.ID)
		}
		ids[w.ID] = true
		tasks := make(map[string]bool)
		for j, task := range w.Tasks {
			task = strings.ToLower(strings.TrimSpace(task))
			if task != "video_segment" && task != "gesture_recognition" {
				return fmt.Errorf("video.ai.workers[%d]: unsupported task %q", i, task)
			}
			if tasks[task] {
				return fmt.Errorf("video.ai.workers[%d]: duplicate task %q", i, task)
			}
			tasks[task], w.Tasks[j] = true, task
		}
	}
	return nil
}

// ToVideoWorkers resolves explicit workers or the backward-compatible shared
// endpoint. Both forms are registered in the same registry, never dialed directly.
func (c GatewayConfig) ToVideoWorkers() []ai.WorkerInfo {
	configured := c.Video.AI.Workers
	if len(configured) == 0 {
		target := strings.TrimSpace(c.Video.AI.GRPCTarget)
		if target == "" {
			return nil
		}
		tasks := []string{"video_segment"}
		if c.Video.Gesture.Enabled {
			tasks = append(tasks, "gesture_recognition")
		}
		configured = []VideoWorkerSection{{ID: target, GRPCTarget: target, Tasks: tasks}}
	}
	workers := make([]ai.WorkerInfo, 0, len(configured))
	for _, w := range configured {
		limit := w.MaxStreams
		if limit == 0 {
			limit = c.Video.AI.MaxActiveStreams
		}
		workers = append(workers, ai.WorkerInfo{
			ID: "config-video:" + w.ID, Addr: w.GRPCTarget,
			Tasks: append([]string(nil), w.Tasks...), MaxStreams: limit,
			MediaTypes: []string{"video"}, InputFormats: []string{"jpeg"}, Protocols: []string{"video_segment"},
		})
	}
	return workers
}
