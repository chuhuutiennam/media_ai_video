import unittest
from dataclasses import replace

import cv2
import numpy as np

from workers.gesture.src.config import GestureConfig, OverlayConfig
from workers.gesture.src.demo import composite_background
from workers.gesture.src.gesture_events import GestureEvent
from workers.gesture.src.overlay import EmojiOverlay


def event(**kw):
    return replace(GestureEvent("one", "s", "v", 1, 100, 1, "Thumb_Up", "static", 0.9, "thumb_up", 1500), **kw)


class OverlayTests(unittest.TestCase):
    def test_expiry_duplicates_stale_and_priority(self):
        overlay = EmojiOverlay(OverlayConfig())
        self.assertFalse(overlay.accept(event(), 900, now=1))
        self.assertTrue(overlay.accept(event(), 100, now=1))
        self.assertFalse(overlay.accept(event(), 100, now=1))
        wave = event(event_id="wave", kind="dynamic", gesture="Wave", emoji_key="wave", timestamp_ms=200)
        self.assertTrue(overlay.accept(wave, 200, now=1.1))
        self.assertFalse(overlay.accept(event(event_id="two", timestamp_ms=300), 300, now=1.2))
        frame = np.zeros((120, 160, 3), np.uint8)
        self.assertTrue(np.any(overlay.render(frame, now=1.2)))
        self.assertFalse(np.any(overlay.render(frame, now=3)))

    def test_assets_alpha_blend_and_small_frames(self):
        for key in GestureConfig().emoji_mapping.values():
            overlay = EmojiOverlay(OverlayConfig(size_ratio=0.5))
            path = overlay.assets_dir / f"{key}.png"
            icon = cv2.imread(str(path), cv2.IMREAD_UNCHANGED)
            self.assertEqual(icon.shape, (256, 256, 4))
            self.assertEqual(int(icon[:, :, 3].min()), 0)
            self.assertEqual(int(icon[:, :, 3].max()), 255)
            overlay.accept(event(emoji_key=key), 100, now=0)
            frame = np.full((32, 48, 3), 90, np.uint8)
            output = overlay.render(frame, now=0)
            self.assertEqual(output.shape, frame.shape)
            self.assertTrue(np.array_equal(output[20:], frame[20:]))
            self.assertFalse(np.array_equal(output, frame))
        self.assertEqual(overlay.render(np.zeros((1, 1, 3), np.uint8), now=0).shape, (1, 1, 3))

    def test_background_before_overlay(self):
        frame = np.full((100, 120, 3), 100, np.uint8)
        mask = np.zeros((100, 120), bool)
        mask[:, :60] = True
        back = np.full_like(frame, 20)
        output = composite_background(frame, mask, "bg_replace", back)
        self.assertTrue(np.all(output[:, :60] == 100))
        self.assertTrue(np.all(output[:, 60:] == 20))
        overlay = EmojiOverlay(OverlayConfig())
        overlay.accept(event(), 100, now=0)
        self.assertFalse(np.array_equal(overlay.render(output, now=0), output))


if __name__ == "__main__":
    unittest.main()
