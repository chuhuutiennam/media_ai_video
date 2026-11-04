import builtins
import importlib
import unittest
from unittest.mock import patch

import workers.gesture.src.demo


class PackageBoundaryTests(unittest.TestCase):
    def test_default_runner_does_not_import_segmentation(self):
        original_import = builtins.__import__

        def import_without_segmentation(name, *args, **kwargs):
            if name == "mediapipe_video" or name.startswith("mediapipe_video."):
                raise AssertionError("standalone gesture must not import segmentation")
            return original_import(name, *args, **kwargs)

        with patch("builtins.__import__", side_effect=import_without_segmentation):
            importlib.reload(workers.gesture.src.demo)

    def test_optional_background_has_clear_missing_dependency_error(self):
        from workers.gesture.src.segmentation_adapter import SegmentationAdapter

        original_import = builtins.__import__

        def import_without_segmentation(name, *args, **kwargs):
            if name.startswith("mediapipe_video"):
                raise ImportError("segmentation deliberately unavailable")
            return original_import(name, *args, **kwargs)

        with patch("builtins.__import__", side_effect=import_without_segmentation):
            with self.assertRaisesRegex(RuntimeError, "optional workers/mediapipe"):
                SegmentationAdapter(None)
