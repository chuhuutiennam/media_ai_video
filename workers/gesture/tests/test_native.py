import importlib.util
import os
import unittest
from pathlib import Path

import cv2
import mediapipe as mp
import numpy as np
from test_gesture_runtime import eventually

from workers.gesture.src.config import GestureConfig, OverlayConfig, WaveConfig
from workers.gesture.src.gesture import GesturePool
from workers.gesture.src.video import VideoGestureProcessor


@unittest.skipUnless(os.environ.get("MEDIAPIPE_NATIVE_SMOKE") == "1", "set MEDIAPIPE_NATIVE_SMOKE=1 for checkpoint tests")
class NativeSmokeTests(unittest.TestCase):
    def test_sample_thumb_up_at_four_seconds(self):
        root = Path(__file__).resolve().parents[2]
        video = root.parent / "data/gesture_sample/static_gesture.mp4"
        if not video.is_file():
            self.skipTest("static gesture sample video is unavailable")
        cfg = GestureConfig(enabled=True, model=root / "checkpoint/gesture_recognizer.task")
        capture = cv2.VideoCapture(str(video))
        self.addCleanup(capture.release)
        self.assertTrue(capture.isOpened())
        fps = capture.get(cv2.CAP_PROP_FPS)
        capture.set(cv2.CAP_PROP_POS_MSEC, 3500)
        events = []
        with VideoGestureProcessor(cfg, WaveConfig(), OverlayConfig()) as processor:
            for index in range(round(2 * fps)):
                ok, bgr = capture.read()
                self.assertTrue(ok)
                image = mp.Image(image_format=mp.ImageFormat.SRGB, data=cv2.cvtColor(bgr, cv2.COLOR_BGR2RGB))
                detected, observation = processor.process(image, round(3500 + index * 1000 / fps), index)
                events.extend(detected)
                self.assertTrue(all(0 <= h.center_normalized[0] <= 1 for h in observation.hands))
        self.assertTrue(any(e.gesture == "Thumb_Up" and 4000 <= e.timestamp_ms <= 5500 for e in events), events)

    def test_real_gesture_livestream_callback(self):
        root = Path(__file__).resolve().parents[2]
        cfg = GestureConfig(enabled=True, model=root / "checkpoint/gesture_recognizer.task", result_timeout_secs=10)
        pool = GesturePool(cfg, WaveConfig(), OverlayConfig())
        stream = pool.open("native", "gesture")
        self.assertIsNotNone(stream)
        try:
            self.assertTrue(stream.ready.wait(30))
            self.assertIsNone(stream.error)
            image = mp.Image(image_format=mp.ImageFormat.SRGB, data=np.zeros((256, 256, 3), np.uint8))
            stream.submit(image, 0, 1)
            self.assertTrue(eventually(lambda: stream.engine.last_timestamp_ms == 0 or stream.error is not None, 10))
            self.assertIsNone(stream.error)
            self.assertEqual(stream.engine.last_timestamp_ms, 0)
            self.assertEqual(stream.drain_events(), [])
        finally:
            stream.close()
        self.assertTrue(stream.closed.wait(10))

    @unittest.skipUnless(importlib.util.find_spec("mediapipe_video"), "optional segmentation package not installed")
    def test_optional_segmentation_adapter(self):
        from workers.gesture.src.segmentation_adapter import SegmentationAdapter

        root = Path(__file__).resolve().parents[2]
        image = mp.Image(image_format=mp.ImageFormat.SRGB, data=np.zeros((48, 64, 3), np.uint8))
        with SegmentationAdapter(root / "checkpoint/selfie_segmenter.tflite") as adapter:
            mask = adapter.mask(image, 0)
            self.assertEqual(mask.shape, (48, 64))
            self.assertEqual(mask.dtype, np.bool_)


