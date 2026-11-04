import io
import json
import logging
import tempfile
import threading
import unittest
from pathlib import Path
from unittest.mock import patch

from workers.gesture.src.config import LogConfig, load_config
from workers.gesture.src.log import setup_logging
from workers.gesture.src import metrics


class LoggingTests(unittest.TestCase):
    def test_config_loads_file_rotation_and_metrics_interval(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / 'config.yaml'
            path.write_text('logging:\n  file: logs/gesture.log\n  backup_days: 7\nmetrics:\n  log_interval_secs: 5\n')
            cfg = load_config(path)
        self.assertEqual(cfg.logging.file, Path('logs/gesture.log'))
        self.assertEqual(cfg.logging.backup_days, 7)
        self.assertEqual(cfg.metrics.log_interval_secs, 5)

    def test_json_file_preserves_message_source_and_exception(self):
        root = logging.getLogger()
        original_handlers, original_level = root.handlers[:], root.level
        # Do not close the test runner's capture handlers through basicConfig(force=True).
        root.handlers = []
        try:
            with tempfile.TemporaryDirectory() as folder:
                path = Path(folder) / 'logs/gesture.log'
                console = io.StringIO()
                with patch('sys.stderr', console):
                    setup_logging(LogConfig(level='INFO', file=path, backup_days=7))
                try:
                    raise ValueError('checkpoint không hợp lệ')
                except ValueError:
                    logging.getLogger('gesture-test').exception('startup_failed model=bad.task')
                for handler in root.handlers:
                    handler.flush()
                self.assertIn('ERROR   startup_failed model=bad.task', console.getvalue())
                self.assertNotIn('{"timestamp"', console.getvalue())
                entry = json.loads(path.read_text())
                self.assertEqual(entry['level'], 'ERROR')
                self.assertEqual(entry['logger'], 'gesture-test')
                self.assertIn('startup_failed', entry['message'])
                self.assertIn('ValueError: checkpoint không hợp lệ', entry['exception'])
                self.assertIn('test_logging.py:', entry['source'])
                file_handler = next(h for h in root.handlers if hasattr(h, 'backupCount'))
                self.assertEqual(file_handler.backupCount, 7)
                self.assertTrue(file_handler.utc)
        finally:
            for handler in root.handlers:
                handler.close()
            root.handlers, root.level = original_handlers, original_level

    def test_metrics_snapshot_uses_histogram_count_and_sum(self):
        before = metrics.snapshot()
        metrics.GESTURE_FRAMES.labels(status='logging_test').inc()
        metrics.GESTURE_LATENCY.observe(0.05)
        after = metrics.snapshot()
        self.assertEqual(after['frames'].get('logging_test', 0), before['frames'].get('logging_test', 0) + 1)
        self.assertIsNotNone(after['latency_avg_ms'])
        self.assertGreater(after['latency_avg_ms'], 0)

    def test_periodic_metrics_logger_can_be_disabled_and_stopped(self):
        called = threading.Event()
        with patch.object(metrics.logger, 'info', side_effect=lambda *_: called.set()):
            stopped = metrics.start_periodic_logger(0)
            self.assertFalse(called.is_set())
            stopped.set()
            stopped = metrics.start_periodic_logger(0.01)
            called.clear()
            try:
                self.assertTrue(called.wait(1))
            finally:
                stopped.set()
        for value in (-1, float('nan'), float('inf'), True):
            with self.assertRaises(ValueError):
                metrics.start_periodic_logger(value)
