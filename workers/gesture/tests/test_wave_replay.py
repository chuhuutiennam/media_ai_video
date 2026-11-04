"""Common landmark fixtures evaluate every signal/counter on identical data."""
from dataclasses import replace
import json
import math
from pathlib import Path
import tempfile
import unittest

from test_gesture_core import hand
from workers.gesture.src.config import GestureConfig, OverlayConfig, WaveConfig, validate_gesture_config
from workers.gesture.src.gesture_events import GestureEngine
from workers.gesture.src.hand_tracker import HandObservation
from workers.gesture.src.wave_replay import compare, negative_controls, read_frames


def sequence(kind, fps=15):
    frames = []
    for i in range(fps * 3):
        t = i / fps
        if kind == 'wave':
            x = 0.5 + 0.14 * math.sin(2 * math.pi * 2 * t)
        elif kind == 'one_way' or kind == 'camera_pan':
            x = 0.2 + 0.15 * t
        elif kind == 'jitter':
            x = 0.5 + 0.003 * (-1) ** i
        else:
            x = 0.5
        h = hand(x, 'Open_Palm', 0.55)
        hands = [{'landmarks': h.landmarks, 'gesture': h.gesture, 'score': h.score, 'handedness': h.handedness}]
        if kind == 'switch':
            # Alternate distant hands, separated by explicit missing observations.
            if i % 4 == 0:
                hands = []
            else:
                h = hand(0.15 if (i // 4) % 2 else 0.85, 'Open_Palm', 0.55)
                hands[0]['landmarks'] = h.landmarks
        frames.append({'session_id': kind, 'stream_id': 'v', 'frame_id': i + 1,
                       'timestamp_ms': round(t * 1000), 'aspect': 4 / 3, 'hands': hands})
    return frames


class WaveReplayTests(unittest.TestCase):
    def test_shared_positive_and_negative_fixtures(self):
        for kind in ('wave', 'still', 'one_way', 'jitter', 'switch', 'camera_pan'):
            results = compare(sequence(kind))
            for name, result in results.items():
                with self.subTest(kind=kind, variant=name):
                    expected = kind == 'wave' and 'open-0.6' not in name
                    self.assertEqual(bool(result['events']), expected)

    def test_frame_rate_and_hand_scale(self):
        for fps in (10, 15, 30):
            for scale in (0.5, 1, 1.5):
                frames = sequence('wave', fps)
                for row in frames:
                    row['hands'][0]['landmarks'] = [[0.5 + (x - 0.5) * scale,
                        0.5 + (y - 0.5) * scale, z] for x, y, z in row['hands'][0]['landmarks']]
                result = compare(frames)
                self.assertTrue(result['palm/hysteresis/open-0.5']['events'], (fps, scale))

    def test_signal_selection_and_static_confidence_independence(self):
        engine = GestureEngine(GestureConfig(static_score_threshold=0.9), WaveConfig(), OverlayConfig(), 's', 'v')
        frames = sequence('wave')
        events = []
        for row in frames:
            h = row['hands'][0]
            events.extend(engine.process((HandObservation(tuple(h['landmarks']), h['gesture'], h['score']),),
                                         row['timestamp_ms'], row['frame_id']))
        self.assertTrue(any(e.gesture == 'Wave' for e in events))
        # Move only the wrist; palm signal is deliberately cancelled at the MCPs.
        for row in frames:
            points = [list(p) for p in row['hands'][0]['landmarks']]
            delta = points[0][0] - 0.5
            for index in (5, 9, 13, 17):
                points[index][0] -= delta * 1.25
            row['hands'][0]['landmarks'] = points
        results = compare(frames)
        self.assertFalse(results['palm/hysteresis/ungated']['events'])
        self.assertTrue(results['wrist/hysteresis/open-0.5']['events'])

    def test_missing_hand_resets_history_but_preserves_cooldown(self):
        engine = GestureEngine(GestureConfig(), WaveConfig(smoothing_alpha=1), OverlayConfig(), 's', 'v')
        for i, x in enumerate((0.4, 0.6, 0.4, 0.6, 0.4, 0.6)):
            engine.process((hand(x, 'Open_Palm'),), i * 100, i)
        detector = next(iter(engine.waves.values()))
        last_event = detector.last_event_ms
        self.assertGreaterEqual(last_event, 0)
        engine.process((), 600, 6)
        self.assertFalse(detector.samples)
        self.assertEqual(detector.last_event_ms, last_event)
        self.assertFalse(engine.process((hand(0.4, 'Open_Palm'),), 700, 7))

    def test_derived_controls_report_camera_ambiguity(self):
        controls = negative_controls(sequence('wave'))
        for name, rows in controls.items():
            result = compare(rows)['palm/hysteresis/open-0.5']
            self.assertEqual(bool(result['events']), name == 'camera_oscillation', name)

    def test_streams_do_not_share_history(self):
        frames = sequence('wave')
        combined = []
        for row in frames:
            combined.extend([row, dict(row, stream_id='other')])
        report = compare(combined)
        for name, item in compare(frames).items():
            self.assertEqual(len(report[name]['events']), 2 * len(item['events']))

    def test_config_and_log_roundtrip(self):
        for changes in ({'signal': 'bad'}, {'counter': 'bad'}, {'require_open_palm': 1},
                        {'open_palm_score_threshold': float('nan')}, {'min_step': 0}, {'min_motion_fraction': -1}):
            with self.assertRaises(ValueError):
                validate_gesture_config(GestureConfig(), replace(WaveConfig(), **changes), OverlayConfig())
        row = sequence('wave')[0]
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'trace.jsonl'
            path.write_text(json.dumps({'message': 'wave_frame ' + json.dumps(row)}) + '\n')
            self.assertEqual(list(read_frames(path))[0]['frame_id'], 1)

    def test_oscillating_camera_is_not_distinguishable_without_reference(self):
        # Translating the entire scene can produce exactly the same landmarks
        # as a waving hand. Keep this limitation visible rather than claim safety.
        self.assertTrue(compare(sequence('wave'))['palm/hysteresis/open-0.5']['events'])


if __name__ == '__main__':
    unittest.main()
