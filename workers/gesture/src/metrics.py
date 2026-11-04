"""Gesture-only Prometheus registry entries, independent of video segmentation."""
import json
import logging
import math
import threading

from prometheus_client import Counter, Gauge, Histogram

logger = logging.getLogger(__name__)
ACTIVE_STREAMS = Gauge("mediapipe_gesture_active_streams", "Active gesture RPCs")
STREAMS_TOTAL = Counter("mediapipe_gesture_streams_total", "Gesture RPCs completed")

ACTIVE_RECOGNIZERS = Gauge("mediapipe_gesture_active_recognizers", "Active gesture recognizers")
GESTURE_FRAMES = Counter("mediapipe_gesture_frames_total", "Gesture frame outcomes", ["status"])
GESTURE_ERRORS = Counter("mediapipe_gesture_errors_total", "Gesture failures", ["reason"])
GESTURE_EVENTS = Counter("mediapipe_gesture_events_total", "Confirmed gesture events", ["gesture", "kind"])
GESTURE_LATENCY = Histogram("mediapipe_gesture_latency_seconds", "Gesture queue and inference latency",
                            buckets=[0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5])


def snapshot():
    """Read public collector samples; never depend on private histogram fields."""
    def samples(metric):
        return [sample for family in metric.collect() for sample in family.samples]

    def totals(metric, label):
        return {s.labels[label]: s.value for s in samples(metric) if s.name.endswith("_total")}

    latency = {s.name.rsplit("_", 1)[-1]: s.value for s in samples(GESTURE_LATENCY)
               if s.name.endswith(("_count", "_sum"))}
    return dict(active_streams=next(iter(samples(ACTIVE_STREAMS))).value,
                active_recognizers=next(iter(samples(ACTIVE_RECOGNIZERS))).value,
                streams_total=sum(s.value for s in samples(STREAMS_TOTAL) if s.name.endswith("_total")),
                frames=totals(GESTURE_FRAMES, "status"), errors=totals(GESTURE_ERRORS, "reason"),
                events_total=sum(s.value for s in samples(GESTURE_EVENTS) if s.name.endswith("_total")),
                latency_avg_ms=latency["sum"] / latency["count"] * 1000 if latency["count"] else None)


def start_periodic_logger(interval_secs):
    if isinstance(interval_secs, bool) or not isinstance(interval_secs, (int, float)) or not math.isfinite(interval_secs) or interval_secs < 0:
        raise ValueError("metrics.log_interval_secs must be finite and nonnegative")
    stopped = threading.Event()
    if interval_secs == 0:
        return stopped

    def loop():
        while not stopped.wait(interval_secs):
            try:
                logger.info("metrics_snapshot %s", json.dumps(snapshot(), ensure_ascii=False))
            except Exception:
                logger.exception("metrics snapshot failed")

    threading.Thread(target=loop, name="gesture-metrics-logger", daemon=True).start()
    logger.info("metrics periodic logger started interval=%ss", interval_secs)
    return stopped
