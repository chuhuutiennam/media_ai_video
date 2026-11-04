import tempfile
import unittest
from dataclasses import replace
from pathlib import Path
from types import SimpleNamespace

import numpy as np
from test_gesture_core import hand
from test_gesture_runtime import result_for

from workers.gesture.src.config import GestureConfig, OverlayConfig, WaveConfig
from workers.gesture.src.gesture_events import GestureEngine, StaticFilter
from workers.gesture.src.overlay import EmojiOverlay
from workers.gesture.src.video import VideoGestureProcessor


class MultiHandTests(unittest.TestCase):
    def engine(self):
        return GestureEngine(GestureConfig(), WaveConfig(), OverlayConfig(), "s", "v")

    def test_thumb_up_sample_confidence_is_accepted_after_stability(self):
        engine = self.engine()
        detected = ()
        for ts in range(0, 301, 50):
            detected = engine.process((hand(label="Thumb_Up", score=0.65),), ts, ts, 16 / 9, 1280, 720)
            if detected:
                break
        self.assertEqual(detected[0].gesture, "Thumb_Up")
        self.assertEqual(detected[0].score, 0.65)
        self.assertEqual(detected[0].frame_width, 1280)
        payload = engine.last_frame.to_dict()
        self.assertNotIn("landmarks", str(payload))
        self.assertEqual(len(payload["hands"][0]["center_px"]), 2)
        self.assertEqual(len(payload["hands"][0]["bbox_px"]), 4)

    def test_short_uncertainty_does_not_reset_hold(self):
        filt = StaticFilter(GestureConfig(static_hold_ms=250))
        hits = [filt.update(label, ts) for ts, label in ((0, "Thumb_Up"), (100, "Thumb_Up"),
                                                       (150, None), (200, "Thumb_Up"), (300, "Thumb_Up"))]
        self.assertEqual(hits, [False, False, False, False, True])
        self.assertFalse(filt.update("Thumb_Up", 400))

    def test_four_hands_reordering_and_independent_events(self):
        engine = self.engine()
        hands = tuple(hand(x, "Thumb_Up", side="Left" if i % 2 else "Right")
                      for i, x in enumerate((0.1, 0.35, 0.65, 0.9)))
        engine.process(hands, 0, 1)
        ids = [h.track_id for h in engine.last_frame.hands]
        engine.process(tuple(reversed(hands)), 100, 2)
        self.assertEqual([h.track_id for h in engine.last_frame.hands], list(reversed(ids)))
        events = engine.process(hands, 200, 3)
        self.assertEqual({e.track_id for e in events}, set(ids))
        overlay = EmojiOverlay(OverlayConfig())
        overlay.update_hands(engine.last_frame)
        for event in events:
            self.assertTrue(overlay.accept(event, 200))
        overlay.render(np.zeros((480, 1000, 3), np.uint8), source_timestamp_ms=200)
        self.assertEqual(len(overlay.last_placements), 4)

    def test_wave_on_one_hand_does_not_suppress_other_thumb(self):
        engine = self.engine()
        hands = (hand(0.2, "Open_Palm"), hand(0.8, "Thumb_Up"))
        engine.process(hands, 0, 1)
        engine.process(hands, 100, 2)
        wave_track = engine.last_frame.hands[0].track_id
        engine.waves[wave_track].update = lambda *args: True
        events = engine.process(hands, 200, 3)
        self.assertEqual({e.gesture for e in events}, {"Wave", "Thumb_Up"})

    def test_overlay_follows_hand_mirror_and_loss(self):
        engine = self.engine()
        for ts in (0, 100):
            engine.process((hand(0.3),), ts, ts)
        events = engine.process((hand(0.3),), 200, 3)
        overlay = EmojiOverlay(OverlayConfig())
        overlay.update_hands(engine.last_frame)
        overlay.accept(events[0], 200)
        frame = np.zeros((480, 1000, 3), np.uint8)
        overlay.render(frame, source_timestamp_ms=200)
        first_x = overlay.last_placements[events[0].track_id][0]
        engine.process((hand(0.4),), 300, 4)
        overlay.update_hands(engine.last_frame)
        overlay.render(frame, source_timestamp_ms=300)
        second_x = overlay.last_placements[events[0].track_id][0]
        self.assertGreater(second_x, first_x)
        overlay.render(frame, source_timestamp_ms=300, mirror=True)
        self.assertGreater(overlay.last_placements[events[0].track_id][0], second_x)
        # No fresh callback/hand state: don't leave a sticker floating on stale hands.
        overlay.render(frame, source_timestamp_ms=1000)
        self.assertEqual(overlay.last_placements, {})

    def test_video_timing_and_held_pose_outlast_wall_time_duration(self):
        engine = self.engine()
        overlay = EmojiOverlay(OverlayConfig(duration_ms=300))
        frame = np.zeros((200, 300, 3), np.uint8)
        for ts in range(0, 1501, 100):
            events = engine.process((hand(),), ts, ts)
            overlay.update_hands(engine.last_frame)
            for event in events:
                overlay.accept(event, ts)
            overlay.render(frame, source_timestamp_ms=ts)
        self.assertTrue(overlay.last_placements)
        engine.process((), 1900, 1900)
        overlay.update_hands(engine.last_frame)
        overlay.render(frame, source_timestamp_ms=1900)
        self.assertFalse(overlay.last_placements)

    def test_video_processor_processes_every_frame_and_reports_coordinates(self):
        class Task:
            def __init__(self):
                self.calls = []
                self.closed = False

            def recognize_for_video(self, image, timestamp_ms):
                self.calls.append(timestamp_ms)
                return result_for(hand(score=0.65))

            def close(self):
                self.closed = True

        with tempfile.TemporaryDirectory() as directory:
            model = Path(directory) / "fake.task"
            model.write_bytes(b"fake")
            task = Task()
            cfg = replace(GestureConfig(), model=model)
            with VideoGestureProcessor(cfg, WaveConfig(), OverlayConfig(), factory=lambda *args: task) as processor:
                events = []
                for ts in range(0, 501, 50):
                    result, observation = processor.process(SimpleNamespace(width=1280, height=720), ts, ts)
                    events.extend(result)
                    self.assertEqual(observation.timestamp_ms, ts)
                    self.assertEqual(len(observation.hands), 1)
                self.assertEqual(len(task.calls), 11)
                self.assertEqual(len(events), 1)
                with self.assertRaises(ValueError):
                    processor.process(SimpleNamespace(width=1, height=1), 500, 12)
            self.assertTrue(task.closed)
