"""Deterministic file-video processing: every frame is matched to its own result."""
import json
import logging
import time

from . import metrics
from .config import validate_gesture_config
from .gesture import create_recognizer, snapshot_result
from .gesture_events import GestureEngine

logger = logging.getLogger(__name__)


class VideoGestureProcessor:
    def __init__(self, config, wave, overlay, session_id="video", stream_id="video", factory=create_recognizer):
        validate_gesture_config(config, wave, overlay)
        if config.model is None:
            raise ValueError("gesture.model is required for video processing")
        self.engine = GestureEngine(config, wave, overlay, session_id, stream_id)
        self.task = factory(config, config.model.read_bytes())
        self.closed = False
        self.last_timestamp_ms = -1
        metrics.ACTIVE_RECOGNIZERS.inc()

    def process(self, image, timestamp_ms, frame_id):
        if self.closed:
            raise RuntimeError("video processor is closed")
        if timestamp_ms <= self.last_timestamp_ms:
            raise ValueError("video timestamps must increase")
        self.last_timestamp_ms = timestamp_ms
        started = time.monotonic()
        result = self.task.recognize_for_video(image, timestamp_ms)
        events = self.engine.process(snapshot_result(result), timestamp_ms, frame_id, image.width / image.height,
                                      image.width, image.height)
        metrics.GESTURE_LATENCY.observe(time.monotonic() - started)
        metrics.GESTURE_FRAMES.labels(status="processed").inc()
        if self.engine.last_frame.hands and logger.isEnabledFor(logging.DEBUG):
            logger.debug("hand_observation %s", json.dumps(self.engine.last_frame.to_dict()))
        for event in events:
            metrics.GESTURE_EVENTS.labels(gesture=event.gesture, kind=event.kind).inc()
        return events, self.engine.last_frame

    def close(self):
        if not self.closed:
            self.closed = True
            try:
                self.task.close()
            finally:
                metrics.ACTIVE_RECOGNIZERS.dec()

    def __enter__(self):
        return self

    def __exit__(self, *args):
        self.close()
