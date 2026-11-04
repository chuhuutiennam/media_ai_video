"""Pure Python gesture filtering and per-stream event production."""
import json
import logging
from dataclasses import dataclass
from uuid import uuid4

from .config import GestureConfig, OverlayConfig, WaveConfig
from .hand_tracker import HandObservation, HandTracker
from .wave_detector import WaveDetector


logger = logging.getLogger(__name__)


@dataclass(frozen=True)
class GestureEvent:
    event_id: str
    session_id: str
    stream_id: str
    frame_id: int
    timestamp_ms: int
    track_id: int
    gesture: str
    kind: str
    score: float | None
    emoji_key: str
    duration_ms: int
    center_normalized: tuple[float, float] | None = None
    bbox_normalized: tuple[float, float, float, float] | None = None
    handedness: str = ""
    frame_width: int = 0
    frame_height: int = 0


@dataclass(frozen=True)
class TrackedHand:
    track_id: int
    handedness: str
    gesture: str
    score: float
    stable_gesture: str | None
    center_normalized: tuple[float, float]
    bbox_normalized: tuple[float, float, float, float]


@dataclass(frozen=True)
class HandFrame:
    session_id: str
    stream_id: str
    frame_id: int
    timestamp_ms: int
    frame_width: int
    frame_height: int
    hands: tuple[TrackedHand, ...]

    def to_dict(self):
        from dataclasses import asdict

        value = asdict(self)
        for hand in value["hands"]:
            hand["center_px"] = [round(hand["center_normalized"][0] * self.frame_width),
                                 round(hand["center_normalized"][1] * self.frame_height)]
            hand["bbox_px"] = [round(v * (self.frame_width if i % 2 == 0 else self.frame_height))
                               for i, v in enumerate(hand["bbox_normalized"])]
        return value


class StaticFilter:
    def __init__(self, cfg: GestureConfig):
        self.cfg = cfg
        self.candidate = None
        self.since = 0
        self.latched = None
        self.release_since = None
        self.last_event_ms = -10**18
        self.last_positive_ms = -10**18
        self.samples = 0

    def update(self, label: str | None, timestamp_ms: int) -> bool:
        if label != self.latched:
            if self.release_since is None:
                self.release_since = timestamp_ms
            if timestamp_ms - self.release_since >= self.cfg.static_release_ms:
                self.latched = None
        else:
            self.release_since = None
        if label is None:
            if timestamp_ms - self.last_positive_ms > self.cfg.static_gap_ms:
                self.candidate, self.samples = None, 0
            return False
        if label != self.candidate or timestamp_ms - self.last_positive_ms > self.cfg.static_gap_ms:
            self.candidate, self.since, self.samples = label, timestamp_ms, 0
        self.samples += 1
        self.last_positive_ms = timestamp_ms
        if label == self.latched:
            return False
        if self.samples < self.cfg.static_min_samples:
            return False
        if self.latched is not None or timestamp_ms - self.since < self.cfg.static_hold_ms:
            return False
        if timestamp_ms - self.last_event_ms < self.cfg.cooldown_ms:
            return False
        self.latched, self.last_event_ms = label, timestamp_ms
        self.release_since = None
        return True


class GestureEngine:
    def __init__(self, gesture: GestureConfig, wave: WaveConfig, overlay: OverlayConfig, session_id: str, stream_id: str):
        self.gesture, self.wave, self.overlay = gesture, wave, overlay
        self.session_id, self.stream_id = session_id, stream_id
        self.tracker = HandTracker(gesture.track_max_gap_ms, wave.max_track_distance, max(4, 2 * gesture.num_hands))
        self.static: dict[int, StaticFilter] = {}
        self.waves: dict[int, WaveDetector] = {}
        self.last_timestamp_ms = -1
        self.last_frame: HandFrame | None = None

    def process(self, hands: tuple[HandObservation, ...], timestamp_ms: int, frame_id: int, aspect=1.0,
                frame_width=0, frame_height=0):
        if timestamp_ms <= self.last_timestamp_ms:
            return []
        self.last_timestamp_ms = timestamp_ms
        if logger.isEnabledFor(logging.DEBUG):
            logger.debug("wave_frame %s", json.dumps({"session_id": self.session_id, "stream_id": self.stream_id,
                "timestamp_ms": timestamp_ms, "frame_id": frame_id, "aspect": aspect,
                "hands": [{"landmarks": h.landmarks, "gesture": h.gesture, "score": h.score,
                           "handedness": h.handedness} for h in hands]}))
        tracked = self.tracker.update(hands, timestamp_ms, aspect)
        live = set(self.tracker.tracks)
        self.static = {k: v for k, v in self.static.items() if k in live}
        self.waves = {k: v for k, v in self.waves.items() if k in live}
        seen, events, observations = set(), [], []
        for track, hand in tracked:
            key = track.track_id
            seen.add(key)
            static = self.static.setdefault(key, StaticFilter(self.gesture))
            wave = self.waves.setdefault(key, WaveDetector(self.wave))
            confident = hand.score >= self.gesture.static_score_threshold
            label = hand.gesture if confident and hand.gesture in self.gesture.emoji_mapping and hand.gesture not in ("None", "Open_Palm", "Wave") else None
            wave_open = hand.score >= self.wave.open_palm_score_threshold and hand.gesture == "Open_Palm"
            wave_hit = wave.update(timestamp_ms, track.center[0], track.size, wave_open,
                                   hand.landmarks[0][0] * aspect, aspect)
            if logger.isEnabledFor(logging.DEBUG):
                logger.debug("wave_decision session=%s stream=%s track=%s palm_x=%s wrist_x=%s diagnostics=%s",
                             self.session_id, self.stream_id, key, track.center[0], hand.landmarks[0][0] * aspect,
                             json.dumps(wave.diagnostics))
            static_hit = static.update(label, timestamp_ms)
            center = (max(0.0, min(1.0, track.center[0] / aspect)), max(0.0, min(1.0, track.center[1])))
            observation = TrackedHand(key, hand.handedness, hand.gesture, hand.score, static.latched, center, hand.bounds())
            observations.append(observation)
            if wave_hit and "Wave" in self.gesture.emoji_mapping:
                static.last_event_ms = timestamp_ms
                events.append(self._event("Wave", "dynamic", None, observation, timestamp_ms, frame_id, frame_width, frame_height))
            elif static_hit:
                events.append(self._event(label, "static", hand.score, observation, timestamp_ms, frame_id, frame_width, frame_height))
        for key, detector in self.waves.items():
            if key not in seen:
                detector.reset()
        for key, static in self.static.items():
            if key not in seen:
                static.update(None, timestamp_ms)
        self.last_frame = HandFrame(self.session_id, self.stream_id, frame_id, timestamp_ms,
                                    frame_width, frame_height, tuple(observations))
        # Priority is per hand; one waving hand must not suppress another hand's Thumb_Up.
        return events

    def _event(self, label, kind, score, hand, timestamp_ms, frame_id, width, height):
        return GestureEvent(uuid4().hex, self.session_id, self.stream_id, frame_id, timestamp_ms,
                            hand.track_id, label, kind, score, self.gesture.emoji_mapping[label], self.overlay.duration_ms,
                            hand.center_normalized, hand.bbox_normalized, hand.handedness, width, height)
