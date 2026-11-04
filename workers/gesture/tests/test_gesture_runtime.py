import tempfile
import threading
import time
import unittest
from dataclasses import replace
from pathlib import Path
from types import SimpleNamespace

from test_gesture_core import hand

from workers.gesture.src.config import GestureConfig, OverlayConfig, WaveConfig
from workers.gesture.src.gesture import GesturePool, snapshot_result


def result_for(observation=None):
    if observation is None:
        return SimpleNamespace(hand_landmarks=[], gestures=[], handedness=[])
    return SimpleNamespace(
        hand_landmarks=[[SimpleNamespace(x=x, y=y, z=z) for x, y, z in observation.landmarks]],
        gestures=[[SimpleNamespace(category_name=observation.gesture, score=observation.score)]],
        handedness=[[SimpleNamespace(category_name=observation.handedness, score=1)]],
    )


def eventually(predicate, timeout=2):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return True
        time.sleep(0.005)
    return bool(predicate())


class FakeTask:
    def __init__(self, callback):
        self.callback = callback
        self.submissions = []
        self.closed = False

    def recognize_async(self, image, timestamp_ms):
        self.submissions.append((image, timestamp_ms))

    def deliver(self, observation=None, timestamp=None):
        image, ts = self.submissions[-1]
        self.callback(result_for(observation), image, ts if timestamp is None else timestamp)

    def close(self):
        self.closed = True


class RuntimeTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.model = Path(directory.name) / "fake.task"
        self.model.write_bytes(b"model")
        self.config = GestureConfig(enabled=True, model=self.model, max_concurrent_recognizers=1, max_fps=100, static_hold_ms=300,
                                    result_timeout_secs=0.5, max_result_age_ms=1000)
        self.tasks = []
        self.image = SimpleNamespace(width=640, height=360)

    def factory(self, config, model, callback):
        task = FakeTask(callback)
        self.tasks.append(task)
        return task

    def pool(self, config=None, factory=None):
        return GesturePool(config or self.config, WaveConfig(), OverlayConfig(), factory or self.factory)

    def open(self, pool, on_event=None):
        stream = pool.open("session", "stream", on_event)
        self.assertIsNotNone(stream)
        self.addCleanup(stream.close)
        self.assertTrue(stream.ready.wait(2))
        return stream

    def test_disabled_and_missing_model_do_not_create_task(self):
        self.assertIsNone(self.pool(replace(self.config, enabled=False)).open("s", "v"))
        with self.assertLogs("workers.gesture.src.gesture", level="ERROR"):
            self.assertIsNone(self.pool(replace(self.config, model=self.model.with_name("missing"))).open("s", "v"))
        self.assertEqual(self.tasks, [])

    def test_snapshot_owns_landmarks(self):
        result = result_for(hand())
        snapshot = snapshot_result(result)
        result.hand_landmarks[0][0].x = 99
        result.gestures[0][0].category_name = "None"
        self.assertNotEqual(snapshot[0].landmarks[0][0], 99)
        self.assertEqual(snapshot[0].gesture, "Thumb_Up")

    def test_nonblocking_latest_frame_capacity_and_cleanup(self):
        pool = self.pool()
        stream = self.open(pool)
        task = self.tasks[0]
        stream.submit(self.image, 0, 0)
        self.assertTrue(eventually(lambda: len(task.submissions) == 1))
        started = time.monotonic()
        stream.submit(self.image, 100, 1)
        stream.submit(self.image, 200, 2)
        self.assertLess(time.monotonic() - started, 0.2)
        self.assertIsNone(pool.open("other", "stream"))
        task.deliver()
        self.assertTrue(eventually(lambda: len(task.submissions) == 2))
        self.assertEqual(task.submissions[-1][1], 200)
        stream.close()
        self.assertTrue(stream.closed.wait(1))
        self.assertTrue(task.closed)
        self.assertFalse(stream.submit(self.image, 300, 3))
        replacement = self.open(pool)
        self.assertIsNone(replacement.error)

    def test_timeout_disables_branch_and_late_callback_is_ignored(self):
        stream = self.open(self.pool(replace(self.config, result_timeout_secs=0.05)))
        stream.submit(self.image, 0, 0)
        with self.assertLogs("workers.gesture.src.gesture", level="ERROR"):
            self.assertTrue(stream.closed.wait(2))
        self.assertIsInstance(stream.error, TimeoutError)
        self.tasks[0].deliver(hand())
        self.assertEqual(stream.drain_events(), [])

    def test_factory_failure_releases_slot(self):
        def fail(*args):
            raise RuntimeError("bad delegate")
        pool = self.pool(factory=fail)
        with self.assertLogs("workers.gesture.src.gesture", level="ERROR"):
            stream = self.open(pool)
            self.assertTrue(stream.closed.wait(2))
        self.assertIsInstance(stream.error, RuntimeError)
        pool._factory = self.factory
        self.assertIsNone(self.open(pool).error)

    def test_static_events_frame_correlation_and_no_repeated_hold(self):
        events = []
        stream = self.open(self.pool(), events.append)
        task = self.tasks[0]
        for index, ts in enumerate((0, 100, 200, 300, 400)):
            stream.submit(self.image, ts, index)
            self.assertTrue(eventually(lambda: len(task.submissions) == index + 1))
            task.deliver(hand())
            self.assertTrue(eventually(lambda: stream.engine.last_timestamp_ms == ts))
        self.assertEqual(len(events), 1)
        self.assertEqual((events[0].frame_id, events[0].timestamp_ms), (3, 300))
        self.assertEqual(stream.drain_events(), events)

    def test_stale_and_wrong_timestamp_callbacks(self):
        stream = self.open(self.pool(replace(self.config, max_result_age_ms=100)))
        task = self.tasks[0]
        stream.submit(self.image, 0, 0)
        self.assertTrue(eventually(lambda: len(task.submissions) == 1))
        task.deliver(hand(), timestamp=99)
        self.assertEqual(stream.engine.last_timestamp_ms, -1)
        stream.submit(self.image, 200, 1)
        task.deliver(hand(), timestamp=0)
        self.assertTrue(eventually(lambda: len(task.submissions) == 2))
        self.assertEqual(stream.engine.last_timestamp_ms, -1)
        task.deliver()
        self.assertTrue(eventually(lambda: stream.engine.last_timestamp_ms == 200))

    def test_model_initialization_does_not_block_submit(self):
        gate = threading.Event()
        def slow(*args):
            gate.wait(1)
            return self.factory(*args)
        stream = self.pool(factory=slow).open("s", "v")
        self.addCleanup(stream.close)
        started = time.monotonic()
        stream.submit(self.image, 0, 0)
        self.assertLess(time.monotonic() - started, 0.2)
        gate.set()
        self.assertTrue(stream.ready.wait(2))

    def test_callback_error_disables_only_gesture(self):
        stream = self.open(self.pool())
        task = self.tasks[0]
        stream.submit(self.image, 0, 0)
        self.assertTrue(eventually(lambda: len(task.submissions) == 1))
        malformed = result_for(hand())
        malformed.hand_landmarks[0].pop()
        with self.assertLogs("workers.gesture.src.gesture", level="ERROR"):
            task.callback(malformed, self.image, 0)
            self.assertTrue(stream.closed.wait(2))
        self.assertIsInstance(stream.error, ValueError)
        self.assertEqual(stream.drain_events(), [])

    def test_slow_native_close_retains_capacity_until_finished(self):
        gate = threading.Event()
        self.addCleanup(gate.set)
        class SlowClose(FakeTask):
            def close(self):
                gate.wait(3)
                super().close()
        def factory(cfg, model, callback):
            return SlowClose(callback)
        pool = self.pool(replace(self.config, result_timeout_secs=0.05), factory)
        stream = self.open(pool)
        with self.assertLogs("workers.gesture.src.gesture", level="WARNING"):
            stream.close()
        self.assertFalse(stream.closed.is_set())
        self.assertIsNone(pool.open("other", "v"))
        gate.set()
        self.assertTrue(stream.closed.wait(2))
        pool._factory = self.factory
        self.assertIsNotNone(self.open(pool))


if __name__ == "__main__":
    unittest.main()
