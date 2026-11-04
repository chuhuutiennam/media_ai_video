"""Bounded, independent LIVE_STREAM gesture side branch.

Only the dispatcher thread owns the native task and gesture engine. submit() never
waits for model creation/inference or event consumers. A native close may block;
in that case its daemon thread retains its pool slot until close actually finishes.
"""
import json
import logging
import threading
import time
from collections import deque
from dataclasses import asdict, dataclass

from . import metrics
from .config import GestureConfig, OverlayConfig, WaveConfig, validate_gesture_config
from .gesture_events import GestureEngine
from .hand_tracker import HandObservation

logger = logging.getLogger(__name__)


def create_recognizer(config, model_bytes, callback=None):
    # Lazy import: disabling gesture does not create/load a gesture task.
    import mediapipe as mp

    return mp.tasks.vision.GestureRecognizer.create_from_options(
        mp.tasks.vision.GestureRecognizerOptions(
            base_options=mp.tasks.BaseOptions(
                model_asset_buffer=model_bytes,
                delegate=mp.tasks.BaseOptions.Delegate[config.delegate.upper()],
            ),
            running_mode=mp.tasks.vision.RunningMode.LIVE_STREAM if callback else mp.tasks.vision.RunningMode.VIDEO,
            num_hands=config.num_hands,
            min_hand_detection_confidence=config.min_hand_detection_confidence,
            min_hand_presence_confidence=config.min_hand_presence_confidence,
            min_tracking_confidence=config.min_tracking_confidence,
            result_callback=callback,
        )
    )


def snapshot_result(result):
    """Own all values before returning to MediaPipe; no borrowed native data."""
    hands = []
    for index, landmarks in enumerate(result.hand_landmarks):
        if len(landmarks) != 21:
            raise ValueError("expected 21 hand landmarks")
        categories = result.gestures[index] if index < len(result.gestures) else []
        category = max(categories, key=lambda c: c.score) if categories else None
        sides = result.handedness[index] if index < len(result.handedness) else []
        side = max(sides, key=lambda c: c.score).category_name if sides else ""
        hands.append(HandObservation(
            tuple((float(p.x), float(p.y), float(p.z)) for p in landmarks),
            category.category_name if category else "None", float(category.score) if category else 0.0, side,
        ))
    return tuple(hands)


@dataclass(frozen=True)
class _Frame:
    image: object
    timestamp_ms: int
    frame_id: int
    received: float


class GestureStream:
    def __init__(self, config, wave, overlay, session_id, stream_id, model_bytes,
                 release, on_event=None, factory=create_recognizer):
        self.config = config
        self.engine = GestureEngine(config, wave, overlay, session_id, stream_id)
        self.session_id, self.stream_id = session_id, stream_id
        self._model_bytes, self._factory = model_bytes, factory
        self._release, self._on_event = release, on_event
        self._condition = threading.Condition()
        self._pending = None
        self._inflight = None
        self._result = None
        self._stopped = False
        self._latest_ts = -1
        self._last_accepted_ts = None
        self._events = deque(maxlen=32)
        self._hand_frame = None
        self.ready = threading.Event()
        self.closed = threading.Event()
        self.error = None
        self._thread = threading.Thread(target=self._run, daemon=True, name=f"mp-gesture-{session_id[:8]}")
        self._thread.start()

    def submit(self, image, timestamp_ms: int, frame_id: int) -> bool:
        """Nonblocking latest-frame mailbox; valid decoded RGB mp.Image only."""
        with self._condition:
            if self._stopped:
                return False
            if timestamp_ms <= self._latest_ts:
                metrics.GESTURE_FRAMES.labels(status="invalid_timestamp").inc()
                return False
            self._latest_ts = timestamp_ms
            if self._last_accepted_ts is not None and timestamp_ms - self._last_accepted_ts < 1000 / self.config.max_fps:
                metrics.GESTURE_FRAMES.labels(status="sampled_out").inc()
                return False
            self._last_accepted_ts = timestamp_ms
            if self._pending is not None:
                metrics.GESTURE_FRAMES.labels(status="replaced").inc()
            self._pending = _Frame(image, timestamp_ms, frame_id, time.monotonic())
            self._condition.notify_all()
            return True

    def drain_events(self):
        """Bounded local event sink for demo/tests; no gRPC contract changes."""
        with self._condition:
            result = list(self._events)
            self._events.clear()
            return result

    def latest_hand_frame(self):
        with self._condition:
            return self._hand_frame

    def close(self):
        with self._condition:
            self._stopped = True
            self._pending = self._result = None
            self._condition.notify_all()
        if threading.current_thread() is not self._thread:
            self._thread.join(timeout=self.config.result_timeout_secs + 0.5)
            if self._thread.is_alive():
                logger.warning("gesture close pending session=%s stream=%s; pool slot retained", self.session_id, self.stream_id)

    def _callback(self, result, image, timestamp_ms):
        with self._condition:
            if self._stopped or self._inflight is None or timestamp_ms != self._inflight.timestamp_ms:
                return
        try:
            payload = (snapshot_result(result), None)
        except Exception as exc:
            payload = (None, exc)
        with self._condition:
            if not self._stopped and self._inflight is not None and timestamp_ms == self._inflight.timestamp_ms:
                self._result = payload
                self._condition.notify_all()

    def _run(self):
        task = None
        active = False
        try:
            task = self._factory(self.config, self._model_bytes, self._callback)
            metrics.ACTIVE_RECOGNIZERS.inc()
            active = True
            self.ready.set()
            while True:
                with self._condition:
                    self._condition.wait_for(lambda: self._stopped or self._pending is not None)
                    if self._stopped:
                        break
                    frame, self._pending = self._pending, None
                    if (time.monotonic() - frame.received) * 1000 > self.config.max_result_age_ms:
                        metrics.GESTURE_FRAMES.labels(status="stale").inc()
                        continue
                    self._inflight, self._result = frame, None
                task.recognize_async(frame.image, frame.timestamp_ms)
                with self._condition:
                    arrived = self._condition.wait_for(
                        lambda: self._stopped or self._result is not None,
                        timeout=self.config.result_timeout_secs,
                    )
                    if self._stopped:
                        break
                    if not arrived:
                        raise TimeoutError("gesture callback timed out")
                    hands, error = self._result
                    self._result = self._inflight = None
                    latest_ts = self._latest_ts
                if error is not None:
                    raise error
                age = time.monotonic() - frame.received
                metrics.GESTURE_LATENCY.observe(age)
                if age * 1000 > self.config.max_result_age_ms or latest_ts - frame.timestamp_ms > self.config.max_result_age_ms:
                    metrics.GESTURE_FRAMES.labels(status="stale").inc()
                    continue
                metrics.GESTURE_FRAMES.labels(status="processed").inc()
                events = self.engine.process(hands, frame.timestamp_ms, frame.frame_id, frame.image.width / frame.image.height,
                                              frame.image.width, frame.image.height)
                with self._condition:
                    self._hand_frame = self.engine.last_frame
                if self.engine.last_frame.hands and logger.isEnabledFor(logging.DEBUG):
                    logger.debug("hand_observation %s", json.dumps(self.engine.last_frame.to_dict()))
                for event in events:
                    with self._condition:
                        if self._stopped:
                            break
                        self._events.append(event)
                    metrics.GESTURE_EVENTS.labels(gesture=event.gesture, kind=event.kind).inc()
                    logger.info("gesture_event %s", json.dumps(asdict(event), ensure_ascii=False))
                    if self._on_event is not None:
                        try:
                            self._on_event(event)
                        except Exception:
                            metrics.GESTURE_ERRORS.labels(reason="consumer").inc()
                            logger.exception("gesture event consumer failed")
        except Exception as exc:
            self.error = exc
            metrics.GESTURE_ERRORS.labels(reason="timeout" if isinstance(exc, TimeoutError) else "task").inc()
            logger.exception("gesture disabled session=%s stream=%s", self.session_id, self.stream_id)
        finally:
            with self._condition:
                self._stopped = True
                self._pending = self._result = self._inflight = None
            self.ready.set()
            try:
                if task is not None:
                    task.close()
            except Exception:
                metrics.GESTURE_ERRORS.labels(reason="close").inc()
                logger.exception("gesture task close failed")
            finally:
                if active:
                    metrics.ACTIVE_RECOGNIZERS.dec()
                self._release()
                self.closed.set()


class GesturePool:
    """Worker-wide model bytes and admission limit; stream state remains private."""
    def __init__(self, config: GestureConfig, wave: WaveConfig, overlay: OverlayConfig, factory=create_recognizer):
        validate_gesture_config(config, wave, overlay)
        self.config, self.wave, self.overlay = config, wave, overlay
        self._factory = factory
        self._slots = threading.BoundedSemaphore(config.max_concurrent_recognizers)
        self._model_bytes = None
        if config.enabled:
            try:
                if config.model is None:
                    raise ValueError("gesture.model is required when gesture is enabled")
                self._model_bytes = config.model.read_bytes()
            except (ValueError, OSError):
                metrics.GESTURE_ERRORS.labels(reason="model").inc()
                logger.exception("gesture model unavailable")

    def open(self, session_id: str, stream_id: str, on_event=None):
        if self._model_bytes is None:
            return None
        if not self._slots.acquire(blocking=False):
            metrics.GESTURE_ERRORS.labels(reason="capacity").inc()
            logger.warning("gesture capacity exhausted session=%s stream=%s", session_id, stream_id)
            return None
        try:
            return GestureStream(self.config, self.wave, self.overlay, session_id, stream_id,
                                 self._model_bytes, self._slots.release, on_event, self._factory)
        except Exception:
            self._slots.release()
            metrics.GESTURE_ERRORS.labels(reason="startup").inc()
            logger.exception("cannot start gesture dispatcher")
            return None
