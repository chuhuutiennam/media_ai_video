import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace

import cv2
import grpc
import numpy as np

from test_gesture_core import hand
from workers.gesture.src.config import AppConfig
from workers.gesture.src.contract import pb
from workers.gesture.src.server import ReactionState, Worker, jpeg_dimensions


class Aborted(grpc.RpcError):
    def __init__(self, code, message):
        self.status, self.message = code, message


class Context:
    def is_active(self):
        return True

    def abort(self, code, message):
        raise Aborted(code, message)


def native_result(observation):
    return SimpleNamespace(
        hand_landmarks=[] if observation is None else [[SimpleNamespace(x=x, y=y, z=z) for x, y, z in observation.landmarks]],
        gestures=[] if observation is None else [[SimpleNamespace(category_name=observation.gesture, score=observation.score)]],
        handedness=[] if observation is None else [[SimpleNamespace(category_name=observation.handedness, score=1)]],
    )


class FakeTask:
    def __init__(self, callback, deliver=True):
        self.callback, self.deliver, self.closed = callback, deliver, False

    def recognize_async(self, image, timestamp):
        if self.deliver:
            self.callback(native_result(hand() if timestamp < 300 else None), image, timestamp)

    def close(self):
        self.closed = True


def config_request():
    return pb.VideoRequest(session_id="s", stream_id="v", config=pb.VideoConfig(
        ai_service="gesture_recognition", input_format="jpeg", media_epoch="source1", max_hands=4, inference_fps=15))


def frame_request(index, timestamp):
    ok, encoded = cv2.imencode('.jpg', np.zeros((4, 8, 3), np.uint8))
    assert ok
    return pb.VideoRequest(session_id="s", stream_id="v", frame=pb.VideoFrame(
        frame_id=index, timestamp_ms=timestamp, rtp_timestamp=9000 + timestamp * 90,
        extended_rtp_timestamp=9000 + timestamp * 90, original_width=160, original_height=80,
        width=8, height=4, encoding="jpeg", image_data=encoded.tobytes()))


class GestureServerTests(unittest.TestCase):
    def worker(self, deliver=True, factory=None):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        cfg = AppConfig()
        cfg.gesture.enabled = True
        cfg.gesture.model = Path(directory.name) / 'model.task'
        cfg.gesture.model.write_bytes(b'fake')
        cfg.gesture.max_concurrent_recognizers = 1
        cfg.gesture.result_timeout_secs = 0.05
        tasks = []
        def create(_config, _bytes, callback):
            task = FakeTask(callback, deliver)
            tasks.append(task)
            return task
        return Worker(cfg, factory or create), tasks

    def test_frame_identity_hold_reaction_and_clear(self):
        worker, tasks = self.worker()
        requests = [config_request()] + [frame_request(i + 1, ts) for i, ts in enumerate((0, 100, 200, 300))]
        requests.append(pb.VideoRequest(session_id='s', stream_id='v', is_eos=True))
        output = list(worker.Segment(iter(requests), Context()))
        self.assertEqual(len(output), 4)
        for index, response in enumerate(output):
            self.assertEqual(response.WhichOneof('result'), 'gesture')
            self.assertEqual((response.session_id, response.stream_id, response.ai_service), ('s', 'v', 'gesture_recognition'))
            self.assertEqual(response.seq, index + 1)
            self.assertEqual(response.gesture.frame_id, index + 1)
            self.assertEqual(response.gesture.original_width, 160)
            self.assertEqual(response.gesture.media_epoch, 'source1')
            self.assertEqual(response.gesture.rtp_timestamp, 9000 + index * 9000)
        self.assertTrue(output[2].gesture.hands[0].HasField('active_reaction'))
        self.assertEqual(output[2].gesture.hands[0].active_reaction.gesture, 'Thumb_Up')
        self.assertEqual(len(output[3].gesture.hands), 0)
        self.assertTrue(tasks[0].closed)

    def test_reject_invalid_config_and_frames(self):
        for field, value in [('ai_service', 'segmentation'), ('input_format', 'RGB24'), ('max_hands', 0), ('media_epoch', '')]:
            with self.subTest(field=field):
                worker, tasks = self.worker()
                request = config_request()
                setattr(request.config, field, value)
                with self.assertRaises(Aborted) as error:
                    list(worker.Segment(iter([request]), Context()))
                self.assertEqual(error.exception.status, grpc.StatusCode.INVALID_ARGUMENT)
                self.assertFalse(tasks)
        for change in ('identity', 'jpeg', 'dimensions', 'timestamp'):
            with self.subTest(frame=change):
                worker, tasks = self.worker()
                frame = frame_request(1, 0)
                if change == 'identity': frame.stream_id = 'other'
                if change == 'jpeg': frame.frame.image_data = b'not JPEG'
                if change == 'dimensions': frame.frame.width = 9
                if change == 'timestamp': frame.frame.timestamp_ms = -1
                with self.assertRaises(Aborted) as error:
                    list(worker.Segment(iter([config_request(), frame]), Context()))
                self.assertEqual(error.exception.status, grpc.StatusCode.INVALID_ARGUMENT)
                self.assertTrue(tasks[0].closed)

    def test_capacity_and_generator_cancellation_release_slot(self):
        worker, tasks = self.worker()
        stream = worker.Segment(iter([config_request(), frame_request(1, 0)]), Context())
        next(stream)
        with self.assertRaises(Aborted) as error:
            list(worker.Segment(iter([config_request()]), Context()))
        self.assertEqual(error.exception.status, grpc.StatusCode.RESOURCE_EXHAUSTED)
        stream.close()
        self.assertTrue(tasks[0].closed)
        self.assertEqual(len(list(worker.Segment(iter([config_request(), frame_request(1, 0)]), Context()))), 1)

    def test_timeout_and_startup_failure_release_slot(self):
        worker, tasks = self.worker(deliver=False)
        with self.assertRaises(Aborted) as error:
            list(worker.Segment(iter([config_request(), frame_request(1, 0)]), Context()))
        self.assertEqual(error.exception.status, grpc.StatusCode.DEADLINE_EXCEEDED)
        self.assertTrue(tasks[0].closed)
        self.assertTrue(worker.slots.acquire(blocking=False))
        worker.slots.release()
        def failed(*_):
            raise RuntimeError('model unavailable')
        worker, _ = self.worker(factory=failed)
        with self.assertRaises(Aborted) as error:
            list(worker.Segment(iter([config_request(), frame_request(1, 0)]), Context()))
        self.assertEqual(error.exception.status, grpc.StatusCode.INTERNAL)
        self.assertTrue(worker.slots.acquire(blocking=False))
        worker.slots.release()

    def test_new_rpc_has_new_epoch(self):
        worker, _ = self.worker()
        def run():
            return list(worker.Segment(iter([config_request(), frame_request(1, 0)]), Context()))[0].gesture
        first, second = run(), run()
        self.assertNotEqual(first.recognizer_epoch, second.recognizer_epoch)
        self.assertNotEqual(first.response_id, second.response_id)

    def test_wave_reaction_priority_expiry_and_hand_disappearance(self):
        cfg = AppConfig()
        state = ReactionState(cfg.gesture, cfg.overlay)
        tracked = SimpleNamespace(track_id=1, stable_gesture='Thumb_Up', gesture='Thumb_Up', score=0.9)
        frame = SimpleNamespace(timestamp_ms=0, hands=[tracked])
        static = state.update(frame, [])[1]
        wave = SimpleNamespace(track_id=1, kind='dynamic', event_id='wave1', gesture='Wave', emoji_key='wave', duration_ms=500)
        frame.timestamp_ms = 100
        self.assertEqual(state.update(frame, [wave])[1]['gesture'], 'Wave')
        frame.timestamp_ms = 200
        self.assertEqual(state.update(frame, [])[1]['gesture'], 'Wave')
        frame.timestamp_ms = 600
        renewed = state.update(frame, [])[1]
        self.assertEqual(renewed['gesture'], 'Thumb_Up')
        self.assertNotEqual(renewed['event_id'], static['event_id'])
        frame.hands = []
        self.assertEqual(state.update(frame, []), {})

    def test_jpeg_header_dimensions(self):
        frame = frame_request(1, 0).frame
        self.assertEqual(jpeg_dimensions(frame.image_data), (8, 4))
        with self.assertRaises(ValueError):
            jpeg_dimensions(b'\xff\xd8\xff\xc0\x00\x11')
