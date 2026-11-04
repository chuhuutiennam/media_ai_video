"""Wave heuristic with palm-scaled hysteresis and a bounded time window."""
from collections import deque
from statistics import median

from .config import WaveConfig


class WaveDetector:
    def __init__(self, config: WaveConfig):
        self.config = config
        self.samples = deque(maxlen=256)
        self.last_event_ms = -10**18
        self.smoothed_x = None
        self.diagnostics = {}

    def reset(self):
        """Discard motion history while retaining event cooldown."""
        self.samples.clear()
        self.smoothed_x = None

    def update(self, timestamp_ms: int, x: float, size: float, is_open: bool,
               wrist_x: float | None = None, aspect: float = 1.0) -> bool:
        cfg = self.config
        self.diagnostics = {"reason": "disabled", "timestamp_ms": timestamp_ms}
        if not cfg.enabled:
            return False
        if cfg.signal == "wrist" and wrist_x is not None:
            x = wrist_x
        if self.samples and (timestamp_ms <= self.samples[-1][0] or timestamp_ms - self.samples[-1][0] > cfg.max_observation_gap_ms):
            self.reset()
        # Preserve alpha at 15 FPS; use media-time deltas at other frame rates.
        dt = timestamp_ms - self.samples[-1][0] if self.samples else 1000 / 15
        alpha = 1 - (1 - cfg.smoothing_alpha) ** (dt / (1000 / 15))
        self.smoothed_x = x if self.smoothed_x is None else alpha * x + (1 - alpha) * self.smoothed_x
        self.samples.append((timestamp_ms, self.smoothed_x, size, is_open))
        while self.samples and timestamp_ms - self.samples[0][0] > cfg.window_ms:
            self.samples.popleft()
        self.diagnostics = {"timestamp_ms": timestamp_ms, "reason": "cooldown", "samples": len(self.samples)}
        if timestamp_ms - self.last_event_ms < cfg.cooldown_ms:
            return False
        self.diagnostics["reason"] = "samples"
        if len(self.samples) < 4:
            return False
        self.diagnostics["reason"] = "duration"
        if timestamp_ms - self.samples[0][0] < cfg.min_duration_ms:
            return False
        ratio = sum(s[3] for s in self.samples) / len(self.samples)
        self.diagnostics.update(reason="open_palm", open_ratio=ratio)
        if cfg.require_open_palm and (not is_open or ratio < cfg.min_open_ratio):
            return False
        palm_size = median(s[2] for s in self.samples)
        amplitude = max(cfg.min_amplitude * palm_size, cfg.min_motion_fraction * aspect)
        step = max(cfg.min_step * palm_size, cfg.min_motion_fraction * aspect)
        low = high = self.samples[0][1]
        direction, changes = 0, 0
        for _, value, _, _ in self.samples:
            if direction == 0:
                low, high = min(low, value), max(high, value)
                if value - low >= amplitude:
                    direction, high = 1, value
                elif high - value >= amplitude:
                    direction, low = -1, value
            elif direction > 0:
                high = max(high, value)
                if high - value >= amplitude:
                    changes += 1
                    direction, low = -1, value
            else:
                low = min(low, value)
                if value - low >= amplitude:
                    changes += 1
                    direction, high = 1, value
        span = max(s[1] for s in self.samples) - min(s[1] for s in self.samples)
        if cfg.counter == "steps":
            changes, direction = 0, 0
            for previous, current in zip(self.samples, list(self.samples)[1:]):
                diff = current[1] - previous[1]
                if abs(diff) <= step:
                    continue
                next_direction = 1 if diff > 0 else -1
                if direction and next_direction != direction:
                    changes += 1
                direction = next_direction
        self.diagnostics.update(reason="motion", changes=changes, span=span,
                                amplitude=amplitude, step=step, palm_size=palm_size)
        if span < amplitude or changes < cfg.min_direction_changes:
            return False
        self.diagnostics["reason"] = "hit"
        self.last_event_ms = timestamp_ms
        self.samples.clear()  # next event requires a new motion sequence
        self.smoothed_x = None
        return True
