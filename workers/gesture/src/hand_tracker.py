"""Small per-stream hand tracker; contains no MediaPipe/native state."""
import math
from dataclasses import dataclass


@dataclass(frozen=True)
class HandObservation:
    landmarks: tuple[tuple[float, float, float], ...]
    gesture: str = "None"
    score: float = 0.0
    handedness: str = ""

    def bounds(self):
        xs, ys = zip(*[(p[0], p[1]) for p in self.landmarks])
        return tuple(max(0.0, min(1.0, v)) for v in (min(xs), min(ys), max(xs), max(ys)))

    def geometry(self, aspect: float):
        points = [(self.landmarks[i][0] * aspect, self.landmarks[i][1]) for i in (0, 5, 9, 13, 17)]
        center = tuple(sum(p[j] for p in points) / len(points) for j in (0, 1))
        size = max(math.dist(points[0], points[2]), math.dist(points[1], points[4]), 1e-4)
        return center, size


@dataclass
class HandTrack:
    track_id: int
    center: tuple[float, float]
    size: float
    handedness: str
    timestamp_ms: int


class HandTracker:
    def __init__(self, max_gap_ms=500, max_distance=3.0, max_tracks=16):
        self.max_gap_ms = max_gap_ms
        self.max_distance = max_distance
        self.max_tracks = max_tracks
        self.tracks: dict[int, HandTrack] = {}
        self._next_id = 1

    def update(self, hands: tuple[HandObservation, ...], timestamp_ms: int, aspect: float):
        self.tracks = {k: t for k, t in self.tracks.items() if 0 <= timestamp_ms - t.timestamp_ms <= self.max_gap_ms}
        geometry = [h.geometry(aspect) for h in hands]
        candidates = []
        for i, (hand, (center, size)) in enumerate(zip(hands, geometry)):
            for key, track in self.tracks.items():
                ratio = size / track.size
                distance = math.dist(center, track.center) / max(size, track.size)
                if 0.5 <= ratio <= 2 and distance <= self.max_distance:
                    penalty = 0.35 if hand.handedness != track.handedness else 0
                    candidates.append((distance + penalty, i, key))
        # Near-equal alternatives indicate crossing/ambiguous hands. Start fresh.
        ambiguous_hands, ambiguous_tracks = set(), set()
        for index in range(len(hands)):
            options = sorted(c for c in candidates if c[1] == index)
            if len(options) > 1 and options[1][0] - options[0][0] < 0.25:
                ambiguous_hands.add(index)
        for key in self.tracks:
            options = sorted(c for c in candidates if c[2] == key)
            if len(options) > 1 and options[1][0] - options[0][0] < 0.25:
                ambiguous_tracks.add(key)
        assigned, used = {}, set()
        for _, index, key in sorted(candidates):
            if index not in assigned and key not in used and index not in ambiguous_hands and key not in ambiguous_tracks:
                assigned[index] = key
                used.add(key)
        result = []
        for index, (hand, (center, size)) in enumerate(zip(hands, geometry)):
            key = assigned.get(index)
            if key is None:
                key, self._next_id = self._next_id, self._next_id + 1
            track = HandTrack(key, center, size, hand.handedness, timestamp_ms)
            self.tracks[key] = track
            result.append((track, hand))
        # Ambiguous old tracks must not be matched again on the next frame.
        keep = {track.track_id for track, _ in result}
        for key in ambiguous_tracks | {c[2] for c in candidates if c[1] in ambiguous_hands}:
            if key not in keep:
                self.tracks.pop(key, None)
        if len(self.tracks) > self.max_tracks:
            ordered = sorted(self.tracks.values(), key=lambda t: (t.track_id in keep, t.timestamp_ms), reverse=True)
            self.tracks = {t.track_id: t for t in ordered[:self.max_tracks]}
        return result
