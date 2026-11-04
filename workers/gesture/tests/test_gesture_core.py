import dataclasses
import tempfile
import unittest
from pathlib import Path

from workers.gesture.src.config import GestureConfig, OverlayConfig, WaveConfig, load_config, validate_gesture_config
from workers.gesture.src.gesture_events import GestureEngine, StaticFilter
from workers.gesture.src.hand_tracker import HandObservation, HandTracker
from workers.gesture.src.wave_detector import WaveDetector


def hand(x=0.5, label="Thumb_Up", score=0.95, side="Left"):
    points = [(x, 0.5, 0.0)] * 21
    for index, dx, dy in ((0, 0, 0.1), (5, -0.05, 0), (9, 0, -0.05), (13, 0.025, 0), (17, 0.05, 0)):
        points[index] = (x + dx, 0.5 + dy, 0.0)
    return HandObservation(tuple(points), label, score, side)


class CoreTests(unittest.TestCase):
    def test_static_hold_release_and_cooldown(self):
        cfg = GestureConfig(static_hold_ms=200, static_release_ms=200, cooldown_ms=500, static_gap_ms=250)
        filt = StaticFilter(cfg)
        self.assertFalse(filt.update("Thumb_Up", 0))
        self.assertFalse(filt.update("Thumb_Up", 100))
        self.assertTrue(filt.update("Thumb_Up", 200))
        self.assertFalse(filt.update("Thumb_Up", 800))
        self.assertFalse(filt.update(None, 900))
        self.assertFalse(filt.update("Thumb_Up", 1000))  # short uncertainty doesn't release
        self.assertFalse(filt.update(None, 1100))
        self.assertFalse(filt.update(None, 1300))
        self.assertFalse(filt.update("Thumb_Up", 1400))
        self.assertFalse(filt.update("Thumb_Up", 1500))
        self.assertTrue(filt.update("Thumb_Up", 1600))

    def test_low_confidence_and_stream_isolation(self):
        cfg, wave, overlay = GestureConfig(static_hold_ms=300, static_gap_ms=250), WaveConfig(), OverlayConfig()
        first = GestureEngine(cfg, wave, overlay, "one", "v")
        second = GestureEngine(cfg, wave, overlay, "two", "v")
        self.assertEqual(first.process((hand(score=0.3),), 0, 1), [])
        self.assertEqual(first.process((hand(),), 100, 2), [])
        self.assertEqual(first.process((hand(),), 300, 3), [])
        event = first.process((hand(),), 400, 4)[0]
        self.assertEqual((event.gesture, event.frame_id, event.session_id), ("Thumb_Up", 4, "one"))
        self.assertEqual(second.process((hand(),), 400, 4), [])
        self.assertEqual(first.process((hand(),), 400, 5), [])

    def test_tracking_reordering_gap_jump_and_crossing(self):
        tracker = HandTracker(max_gap_ms=300)
        a, b = hand(0.2), hand(0.8, side="Right")
        initial = tracker.update((a, b), 0, 1)
        reordered = tracker.update((b, a), 100, 1)
        self.assertEqual(initial[0][0].track_id, reordered[1][0].track_id)
        self.assertEqual(initial[1][0].track_id, reordered[0][0].track_id)
        expired = tracker.update((a,), 500, 1)[0][0]
        self.assertNotEqual(expired.track_id, initial[0][0].track_id)
        jumped = tracker.update((b,), 600, 1)[0][0]
        self.assertNotEqual(jumped.track_id, expired.track_id)
        tracker = HandTracker()
        old = tracker.update((hand(0.45), hand(0.55)), 0, 1)
        new = tracker.update((hand(0.5), hand(0.5)), 100, 1)
        self.assertTrue({x.track_id for x, _ in old}.isdisjoint({x.track_id for x, _ in new}))

    def test_wave_motion_in_both_directions_and_cooldown(self):
        for sign in (-1, 1):
            detector = WaveDetector(WaveConfig(smoothing_alpha=1))
            xs = [0, 0.1, 0.2, 0.1, 0, 0.1, 0.2]
            hits = [detector.update(i * 100, sign * x, 0.15, True) for i, x in enumerate(xs)]
            self.assertTrue(any(hits))
            self.assertFalse(detector.update(700, 0, 0.15, True))

    def test_wave_negative_cases_and_gap(self):
        for xs, opened in (([0] * 10, True), ([i * 0.05 for i in range(10)], True),
                           ([0, 0.01] * 5, True), ([0, 0.2] * 5, False)):
            detector = WaveDetector(WaveConfig(smoothing_alpha=1))
            self.assertFalse(any(detector.update(i * 100, x, 0.15, opened) for i, x in enumerate(xs)))
        detector = WaveDetector(WaveConfig(smoothing_alpha=1))
        for i, x in enumerate((0, 0.2, 0)):
            self.assertFalse(detector.update(i * 100, x, 0.15, True))
        self.assertFalse(detector.update(1000, 0.2, 0.15, True))

    def test_wave_engine_metadata_and_no_open_palm_event(self):
        engine = GestureEngine(GestureConfig(), WaveConfig(smoothing_alpha=1), OverlayConfig(), "s", "v")
        events = []
        for i, x in enumerate((0.4, 0.5, 0.6, 0.5, 0.4, 0.5, 0.6)):
            events.extend(engine.process((hand(x, "Open_Palm"),), i * 100, i, 16 / 9))
        self.assertEqual(len(events), 1)
        self.assertEqual((events[0].gesture, events[0].kind, events[0].score), ("Wave", "dynamic", None))

    def test_config_paths_and_validation(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "config.yaml"
            path.write_text('gesture:\n  model: "models/gesture.task"\noverlay:\n  assets_dir: "icons"\n')
            cfg = load_config(path)
            self.assertEqual(cfg.gesture.model, Path(directory) / "models/gesture.task")
            self.assertEqual(cfg.overlay.assets_dir, Path(directory) / "icons")
            self.assertFalse(cfg.gesture.enabled)
        for field, value in (("max_fps", 0), ("max_fps", float("nan")), ("num_hands", 9),
                             ("max_concurrent_recognizers", 1.2), ("static_score_threshold", 2),
                             ("emoji_mapping", {"Wave": "../bad"})):
            with self.subTest(field=field), self.assertRaises(ValueError):
                validate_gesture_config(dataclasses.replace(GestureConfig(), **{field: value}), WaveConfig(), OverlayConfig())


if __name__ == "__main__":
    unittest.main()
