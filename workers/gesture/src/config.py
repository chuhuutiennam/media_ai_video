"""Standalone gesture configuration, independent of segmentation."""
import dataclasses
import math
from dataclasses import dataclass, field
from pathlib import Path
from typing import Optional

import yaml


@dataclass
class GestureConfig:
    enabled: bool = False
    model: Optional[Path] = None
    delegate: str = "cpu"
    num_hands: int = 4
    max_fps: float = 15.0
    max_concurrent_recognizers: int = 4
    result_timeout_secs: float = 2.0
    min_hand_detection_confidence: float = 0.5
    min_hand_presence_confidence: float = 0.5
    min_tracking_confidence: float = 0.5
    static_score_threshold: float = 0.6
    static_hold_ms: int = 200
    static_gap_ms: int = 150
    static_min_samples: int = 3
    track_max_gap_ms: int = 500
    static_release_ms: int = 300
    cooldown_ms: int = 1500
    max_result_age_ms: int = 500
    emoji_mapping: dict[str, str] = field(default_factory=lambda: {
        "Thumb_Up": "thumb_up", "Thumb_Down": "thumb_down", "Victory": "victory",
        "ILoveYou": "heart", "Closed_Fist": "fist", "Pointing_Up": "point_up", "Wave": "wave",
    })


@dataclass
class WaveConfig:
    enabled: bool = True
    signal: str = "palm"  # palm or wrist; keep the established default until evaluated
    counter: str = "hysteresis"  # hysteresis or steps
    open_palm_score_threshold: float = 0.5
    require_open_palm: bool = True
    min_step: float = 0.08  # palm-size units, for the notebook-style step counter
    min_motion_fraction: float = 0.005  # image-width noise floor
    window_ms: int = 1000
    min_duration_ms: int = 400
    min_direction_changes: int = 2
    max_observation_gap_ms: int = 300
    cooldown_ms: int = 2000
    min_amplitude: float = 0.65  # units of palm size per directional stroke
    min_open_ratio: float = 0.7
    smoothing_alpha: float = 0.65
    max_track_distance: float = 3.0  # units of palm size


@dataclass
class OverlayConfig:
    enabled: bool = True
    duration_ms: int = 1500
    position: str = "near_hand"
    size_ratio: float = 0.16
    hand_lost_ms: int = 300
    assets_dir: Optional[Path] = None  # None: packaged PNG assets


def validate_gesture_config(gesture: GestureConfig, wave: WaveConfig, overlay: OverlayConfig) -> None:
    """Validate gesture worker settings."""
    def positive(name, value):
        if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value) or value <= 0:
            raise ValueError(f"{name} must be finite and positive")

    def integer(name, value):
        positive(name, value)
        if not isinstance(value, int):
            raise ValueError(f"{name} must be an integer")

    if gesture.delegate not in ("cpu", "gpu"):
        raise ValueError("gesture.delegate must be cpu or gpu")
    if type(gesture.num_hands) is not int or not 1 <= gesture.num_hands <= 8:
        raise ValueError("gesture.num_hands must be in 1..8")
    for name in ("max_fps", "result_timeout_secs"):
        positive(f"gesture.{name}", getattr(gesture, name))
    for name in ("max_concurrent_recognizers", "static_hold_ms", "static_release_ms", "cooldown_ms", "max_result_age_ms",
                 "static_gap_ms", "static_min_samples", "track_max_gap_ms"):
        integer(f"gesture.{name}", getattr(gesture, name))
    for name in ("window_ms", "min_duration_ms", "min_direction_changes", "max_observation_gap_ms", "cooldown_ms"):
        integer(f"wave.{name}", getattr(wave, name))
    if wave.signal not in ("palm", "wrist") or wave.counter not in ("hysteresis", "steps"):
        raise ValueError("unsupported wave.signal/counter")
    for name in ("min_amplitude", "max_track_distance", "min_step"):
        positive(f"wave.{name}", getattr(wave, name))
    for prefix, cfg, names in (
        ("gesture", gesture, ("min_hand_detection_confidence", "min_hand_presence_confidence", "min_tracking_confidence", "static_score_threshold")),
        ("wave", wave, ("min_open_ratio", "smoothing_alpha", "open_palm_score_threshold", "min_motion_fraction")),
        ("overlay", overlay, ("size_ratio",)),
    ):
        for name in names:
            value = getattr(cfg, name)
            if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value) or not 0 < value <= 1:
                raise ValueError(f"{prefix}.{name} must be in (0,1]")
    if wave.min_duration_ms > wave.window_ms:
        raise ValueError("wave.min_duration_ms must not exceed window_ms")
    integer("overlay.duration_ms", overlay.duration_ms)
    integer("overlay.hand_lost_ms", overlay.hand_lost_ms)
    if overlay.position not in ("near_hand", "top_right", "top_left", "bottom_right", "bottom_left"):
        raise ValueError("unsupported overlay.position")
    if not isinstance(gesture.emoji_mapping, dict) or any(
        not isinstance(k, str) or not isinstance(v, str) or not v or Path(v).name != v or v in (".", "..")
        for k, v in gesture.emoji_mapping.items()
    ):
        raise ValueError("gesture.emoji_mapping must map gesture names to asset basenames")
    for name, value in (("gesture.enabled", gesture.enabled), ("wave.enabled", wave.enabled),
                        ("wave.require_open_palm", wave.require_open_palm), ("overlay.enabled", overlay.enabled)):
        if not isinstance(value, bool):
            raise ValueError(f"{name} must be a boolean")


@dataclass
class ServerConfig:
    addr: str = "127.0.0.1:50053"
    max_streams: int = 8
    max_receive_message_bytes: int = 8 << 20
    max_inference_dim: int = 1920
    shutdown_grace_secs: float = 2.0


@dataclass
class MetricsConfig:
    enabled: bool = True
    port: int = 9102
    log_interval_secs: float = 60


@dataclass
class LogConfig:
    level: str = "INFO"
    file: Optional[Path] = Path("logs/mediapipe-gesture.log")
    backup_days: int = 30


@dataclass
class AppConfig:
    gesture: GestureConfig = field(default_factory=GestureConfig)
    wave: WaveConfig = field(default_factory=WaveConfig)
    overlay: OverlayConfig = field(default_factory=OverlayConfig)
    server: ServerConfig = field(default_factory=ServerConfig)
    metrics: MetricsConfig = field(default_factory=MetricsConfig)
    logging: LogConfig = field(default_factory=LogConfig)


def _fill(cls, data):
    known = {f.name for f in dataclasses.fields(cls)}
    return cls(**{k: v for k, v in data.items() if k in known})


def load_config(path: Path) -> AppConfig:
    with path.open(encoding="utf-8") as file:
        raw = yaml.safe_load(file) or {}
    gesture_data = dict(raw.get("gesture", {}))
    overlay_data = dict(raw.get("overlay", {}))
    for data, key in ((gesture_data, "model"), (overlay_data, "assets_dir")):
        if data.get(key) is not None:
            value = Path(data[key]).expanduser()
            data[key] = (path.resolve().parent / value).resolve() if not value.is_absolute() else value
    cfg = AppConfig(_fill(GestureConfig, gesture_data), _fill(WaveConfig, raw.get("wave", {})),
                    _fill(OverlayConfig, overlay_data))
    cfg.server = _fill(ServerConfig, raw.get("server", {}))
    cfg.metrics = _fill(MetricsConfig, raw.get("metrics", {}))
    cfg.logging = _fill(LogConfig, raw.get("logging", {}))
    if cfg.logging.file is not None:
        cfg.logging.file = Path(cfg.logging.file).expanduser()
    validate_gesture_config(cfg.gesture, cfg.wave, cfg.overlay)
    return cfg
