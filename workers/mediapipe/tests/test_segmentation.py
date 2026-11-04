import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import cv2
import grpc
import numpy as np

from mediapipe_video.config import MediaPipeConfig
from mediapipe_video.contract import pb
from mediapipe_video.segmenter import Worker


class Context:
    def is_active(self):
        return True

    def abort(self, code, message):
        raise RuntimeError(f"{code}: {message}")


class FakeSegmenter:
    def __init__(self, callback):
        self.callback = callback

    def __enter__(self):
        return self

    def __exit__(self, *args):
        pass

    def segment_async(self, image, timestamp):
        plane = np.ones((image.height, image.width), np.float32)
        result = SimpleNamespace(confidence_masks=[SimpleNamespace(numpy_view=lambda: plane)])
        self.callback(result, image, timestamp)
        plane.fill(0)  # callback must own the binary mask


def requests(count=2):
    yield pb.VideoRequest(session_id="s", stream_id="v", config=pb.VideoConfig(
        effect_type="bg_replace", inference_width=64, inference_height=48, inference_fps=15))
    ok, jpeg = cv2.imencode(".jpg", np.zeros((48, 64, 3), np.uint8))
    assert ok
    for i in range(count):
        yield pb.VideoRequest(session_id="s", stream_id="v", frame=pb.VideoFrame(
            frame_id=i, rtp_timestamp=9000 + i * 6000, timestamp_ms=i * 100,
            encoding="jpeg", image_data=jpeg.tobytes()))
    yield pb.VideoRequest(session_id="s", stream_id="v", is_eos=True)


class WorkerRegressionTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.model = Path(directory.name) / "model"
        self.model.write_bytes(b"fake")

    def worker(self):
        with patch.object(Worker, "create_segmenter", lambda _, cb: FakeSegmenter(cb)):
            worker = Worker(MediaPipeConfig(model=self.model, delegate="cpu"))
        worker.create_segmenter = FakeSegmenter
        return worker

    def test_one_mask_per_frame(self):
        worker = self.worker()
        responses = list(worker.Segment(iter(requests()), Context()))
        self.assertFalse(hasattr(worker, "gesture_pool"))
        self.assertEqual(len(responses), 2)
        for index, response in enumerate(responses):
            self.assertEqual(response.mask.status, "ok")
            self.assertEqual(response.mask.frame_id, index)
            self.assertEqual(response.mask.rtp_timestamp, 9000 + index * 6000)
            self.assertEqual(list(response.mask.rle_counts), [0, 64 * 48])
            self.assertEqual((response.session_id, response.stream_id), ("s", "v"))

    def test_cancelled_request_iterator_drains_without_thread_exception(self):
        def cancelled():
            yield from list(requests(1))[:-1]
            raise grpc.RpcError()

        responses = list(self.worker().Segment(iter(cancelled()), Context()))
        self.assertEqual(len(responses), 1)

