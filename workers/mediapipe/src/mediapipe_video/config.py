"""Application configuration — dataclasses + YAML loader."""
import dataclasses
from dataclasses import dataclass, field
from pathlib import Path
from typing import Optional

import yaml


# ---------------------------------------------------------------------------
# Config sections
# ---------------------------------------------------------------------------

@dataclass
class ServerConfig:
    addr: str = "127.0.0.1:50052"
    max_streams: int = 8
    max_receive_message_bytes: int = 8 << 20   # 8 MiB
    shutdown_grace_secs: float = 2.0


@dataclass
class MediaPipeConfig:
    model: Optional[Path] = None
    delegate: str = "auto"
    foreground_index: Optional[int] = None
    result_timeout_secs: float = 10.0
    gpu_device_index: Optional[int] = None


@dataclass
class SegmentationConfig:
    default_threshold: float = 0.5
    max_inference_dim: int = 4096
    poll_interval_secs: float = 0.1
    allowed_effect_types: list = field(
        default_factory=lambda: ["bg_blur", "bg_replace", "bg_remove"]
    )
    # GPU EGL context limit: 0 = no limit; for GPU delegate set 4–6 to avoid EGL context exhaustion
    max_concurrent_segmenters: int = 0


@dataclass
class LogConfig:
    level: str = "INFO"
    file: Optional[Path] = Path("logs/mediapipe-video.log")
    backup_days: int = 30


@dataclass
class MetricsConfig:
    enabled: bool = True
    port: int = 9101
    log_interval_secs: int = 60   # 0 = tắt periodic log


@dataclass
class AppConfig:
    server: ServerConfig = field(default_factory=ServerConfig)
    mediapipe: MediaPipeConfig = field(default_factory=MediaPipeConfig)
    segmentation: SegmentationConfig = field(default_factory=SegmentationConfig)
    logging: LogConfig = field(default_factory=LogConfig)
    metrics: MetricsConfig = field(default_factory=MetricsConfig)


# ---------------------------------------------------------------------------
# Loader
# ---------------------------------------------------------------------------

def _fill(cls, data: dict):
    """Create a dataclass instance from a dict, ignoring unknown keys."""
    known = {f.name for f in dataclasses.fields(cls)}
    return cls(**{k: v for k, v in data.items() if k in known})


def load_config(path: Path) -> AppConfig:
    """Load AppConfig from a YAML file. Missing sections use defaults."""
    with open(path, encoding="utf-8") as fh:
        raw = yaml.safe_load(fh) or {}

    mp_data = dict(raw.get("mediapipe", {}))
    if "model" in mp_data and mp_data["model"] is not None:
        mp_data["model"] = Path(mp_data["model"])

    log_data = dict(raw.get("logging", {}))
    if "file" in log_data and log_data["file"] is not None:
        log_data["file"] = Path(log_data["file"])

    return AppConfig(
        server=_fill(ServerConfig, raw.get("server", {})),
        mediapipe=_fill(MediaPipeConfig, mp_data),
        segmentation=_fill(SegmentationConfig, raw.get("segmentation", {})),
        logging=_fill(LogConfig, log_data),
        metrics=_fill(MetricsConfig, raw.get("metrics", {})),
    )
