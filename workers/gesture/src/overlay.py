"""Cartoon PNG stickers anchored to individual tracked hands."""
from pathlib import Path

import cv2
import numpy as np

from .config import OverlayConfig


class EmojiOverlay:
    def __init__(self, config: OverlayConfig, max_event_age_ms=500):
        self.config = config
        self.max_event_age_ms = max_event_age_ms
        packaged = Path(__file__).parent / "assets" / "emoji"
        self.assets_dir = config.assets_dir or (packaged if packaged.is_dir() else Path(__file__).parent.parent / "assets/emoji")
        self._cache = {}
        self._active = {}
        self._hands = {}
        self._seen = []
        self._source_timestamp = 0
        self.last_placements = {}  # debug/tests: track_id -> (x, y, width, height)

    def update_hands(self, frame):
        if frame is None or frame.timestamp_ms < self._source_timestamp:
            return
        self._source_timestamp = frame.timestamp_ms
        for hand in frame.hands:
            self._hands[hand.track_id] = (hand, frame.timestamp_ms)
            active = self._active.get(hand.track_id)
            if active:
                event, expiry = active
                if event.kind == "static" and hand.stable_gesture == event.gesture and hand.gesture == event.gesture:
                    self._active[hand.track_id] = (event, frame.timestamp_ms + event.duration_ms)
        self._hands = {k: v for k, v in self._hands.items()
                       if frame.timestamp_ms - v[1] <= self.config.hand_lost_ms}

    def accept(self, event, source_timestamp_ms: int, now=None):
        # Explicit now is retained for old callers/tests, otherwise all timing uses
        # source media time so slow/offline video exports have deterministic durations.
        timestamp = source_timestamp_ms if now is None else now * 1000
        if not self.config.enabled or event.event_id in self._seen:
            return False
        if not 0 <= source_timestamp_ms - event.timestamp_ms <= self.max_event_age_ms:
            return False
        active = self._active.get(event.track_id)
        if active and timestamp < active[1]:
            previous = active[0]
            if event.timestamp_ms < previous.timestamp_ms:
                return False
            if previous.kind == "dynamic" and event.kind == "static":
                return False
        self._seen = (self._seen + [event.event_id])[-128:]
        self._active[event.track_id] = (event, timestamp + event.duration_ms)
        self._source_timestamp = source_timestamp_ms
        return True

    def _icon(self, key):
        if key not in self._cache:
            if Path(key).name != key or key in (".", ".."):
                raise ValueError("emoji key must be an asset basename")
            icon = cv2.imread(str(self.assets_dir / f"{key}.png"), cv2.IMREAD_UNCHANGED)
            if icon is None or icon.ndim != 3 or icon.shape[2] != 4:
                raise ValueError(f"missing RGBA emoji asset: {key}")
            self._cache[key] = icon
        return self._cache[key]

    @staticmethod
    def _overlap(rect, others):
        x, y, w, h = rect
        return sum(max(0, min(x + w, ox + ow) - max(x, ox)) *
                   max(0, min(y + h, oy + oh) - max(y, oy))
                   for ox, oy, ow, oh in others)

    def render(self, bgr, now=None, source_timestamp_ms=None, mirror=False):
        timestamp = (self._source_timestamp if source_timestamp_ms is None else source_timestamp_ms) if now is None else now * 1000
        self.last_placements = {}
        if not self.config.enabled:
            return bgr
        height, width = bgr.shape[:2]
        side = max(1, min(height, width, round(min(height, width) * self.config.size_ratio)))
        margin = min(8, round(min(height, width) * 0.03), max(0, (min(height, width) - side) // 2))
        output = bgr.copy()
        self._active = {k: v for k, v in self._active.items() if timestamp < v[1]}
        for track_id, (event, _) in sorted(self._active.items()):
            hand_entry = self._hands.get(track_id)
            if hand_entry and timestamp - hand_entry[1] > self.config.hand_lost_ms:
                hand_entry = None
            if event.center_normalized is not None and hand_entry is None and timestamp - event.timestamp_ms > self.config.hand_lost_ms:
                continue
            center = hand_entry[0].center_normalized if hand_entry else event.center_normalized
            bounds = hand_entry[0].bbox_normalized if hand_entry else event.bbox_normalized
            if center is not None and bounds is not None and self.config.position == "near_hand":
                cx, cy = center
                left, top, right, bottom = bounds
                if mirror:
                    cx, left, right = 1 - cx, 1 - right, 1 - left
                candidates = [(cx * width - side / 2, top * height - side - margin),
                              (right * width + margin, cy * height - side / 2),
                              (left * width - side - margin, cy * height - side / 2),
                              (cx * width - side / 2, bottom * height + margin)]
                candidates = [(round(max(0, min(width - side, x))),
                               round(max(0, min(height - side, y))), side, side)
                              for x, y in candidates]
                x, y, _, _ = min(candidates, key=lambda r: self._overlap(r, self.last_placements.values()))
            else:
                x = width - margin - side if self.config.position.endswith("right") or self.config.position == "near_hand" else margin
                y = height - margin - side if self.config.position.startswith("bottom") else margin
            icon = cv2.resize(self._icon(event.emoji_key), (side, side), interpolation=cv2.INTER_AREA)
            alpha = icon[:, :, 3:4].astype(np.float32) / 255
            region = output[y:y + side, x:x + side]
            region[:] = np.round(icon[:, :, :3] * alpha + region * (1 - alpha)).astype(np.uint8)
            self.last_placements[track_id] = (x, y, side, side)
        return output
