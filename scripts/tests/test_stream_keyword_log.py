import contextlib
import errno
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock


SPEC = importlib.util.spec_from_file_location('stream_keyword_log', Path(__file__).resolve().parents[1] / 'stream-keyword-log.py')
STREAM = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(STREAM)


class KeywordLogReaderTest(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.path = Path(temp.name) / 'gateway.log'

    def test_enodata_preserves_offset_and_partial_line_without_replaying(self):
        first = json.dumps({'msg': 'keyword: after mapping', 'topic_info': 'celebrate'}).encode() + b'\n'
        second = json.dumps({'msg': 'keyword: after mapping', 'topic_info': 'approve'}).encode() + b'\n'
        third = json.dumps({'msg': 'keyword: after mapping', 'topic_info': 'disapprove'}).encode()
        self.path.write_bytes(first + second + third)
        prefix = first + second[:20]
        reader = STREAM.KeywordLogReader(self.path)
        got = []
        with self.path.open('rb') as stream:
            faulty = mock.Mock()
            faulty.fileno.return_value = stream.fileno()
            faulty.read.side_effect = [prefix, OSError(errno.ENODATA, 'No data available')]
            with mock.patch.object(Path, 'open', return_value=contextlib.nullcontext(faulty)):
                with self.assertRaises(OSError):
                    for line in reader.poll():
                        got.append(json.loads(line))
        self.assertEqual(reader.offset, len(prefix))
        self.assertEqual(reader.pending, second[:20])
        got.extend(json.loads(line) for line in reader.poll())
        self.assertEqual(got, [json.loads(first), json.loads(second)])
        with self.path.open('ab') as stream:
            stream.write(b'\n')
        self.assertEqual([json.loads(line) for line in reader.poll()], [json.loads(third)])
        self.assertEqual(list(reader.poll()), [])

    def test_file_replacement_and_truncation_restart_at_beginning(self):
        first = json.dumps({'msg': 'keyword: after mapping', 'topic_info': 'approve'}) + '\n'
        self.path.write_text(first)
        reader = STREAM.KeywordLogReader(self.path)
        self.assertEqual(list(reader.poll()), [first.rstrip()])
        self.path.write_text('{"msg":"keyword: after mapping"}\n')
        self.assertEqual(list(reader.poll()), ['{"msg":"keyword: after mapping"}'])
        self.path.rename(self.path.with_suffix('.old'))
        self.path.write_text(first)
        self.assertEqual(list(reader.poll()), [first.rstrip()])

    def test_follow_retries_transient_read_and_drains_after_gateway_exit(self):
        reader = mock.Mock(offset=12)
        row = dict(msg='keyword: after mapping', session_id='s', frame_id=129, track_id=1,
                   gesture='Thumb_Up', topic_label='EMOTION', topic_info='approve')
        reader.poll.side_effect = [OSError(errno.ENODATA, 'No data available'), [json.dumps(row)]]
        out, err = io.StringIO(), io.StringIO()
        with mock.patch.object(STREAM, 'KeywordLogReader', return_value=reader), \
                mock.patch.object(STREAM, 'process_alive', side_effect=[True, False]), \
                mock.patch.object(STREAM.time, 'sleep'), \
                contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            self.assertEqual(STREAM.follow(self.path, 123), 0)
        self.assertEqual(reader.poll.call_count, 2)
        self.assertIn('retrying at byte 12', err.getvalue())
        self.assertEqual(out.getvalue().splitlines(), [
            '[GESTURE] START session=s track=1 gesture=Thumb_Up frame=129 keyword=EMOTION/approve',
            '[GESTURE] END session=s track=1 gesture=Thumb_Up frames=129..129 keyword=EMOTION/approve',
        ])

    def test_follow_reports_permanent_io_error(self):
        reader = mock.Mock()
        reader.poll.side_effect = OSError(errno.EIO, 'I/O error')
        err = io.StringIO()
        with mock.patch.object(STREAM, 'KeywordLogReader', return_value=reader), \
                mock.patch.object(STREAM, 'process_alive', return_value=True), \
                contextlib.redirect_stderr(err):
            self.assertEqual(STREAM.follow(self.path, 123), 1)
        self.assertIn('keyword log reader failed', err.getvalue())


class GestureSummaryTest(unittest.TestCase):
    def setUp(self):
        self.lines = []
        self.summary = STREAM.GestureSummary(self.lines.append)

    def frame(self, number, hands, session='s', epoch='recognizer'):
        identity = dict(session_id=session, frame_id=number, response_id=f'{epoch}/{number}')
        self.summary.consume(dict(identity, msg='keyword: before mapping'))
        for track, gesture in hands.items():
            self.summary.consume(dict(identity, msg='keyword: after mapping', track_id=track,
                                      gesture=gesture, topic_label='EMOTION', topic_info=gesture.lower()))

    def test_unchanged_gesture_prints_start_and_end_once(self):
        self.frame(1, {})
        for number in (3, 5, 7, 9):
            self.frame(number, {1: 'Wave'})
        self.frame(11, {})
        self.frame(13, {})
        self.summary.finish()
        self.assertEqual(self.lines, [
            '[GESTURE] START session=s track=1 gesture=Wave frame=3 keyword=EMOTION/wave',
            '[GESTURE] END session=s track=1 gesture=Wave frames=3..9 keyword=EMOTION/wave',
        ])

    def test_change_closes_previous_gesture_before_starting_next(self):
        self.frame(1, {1: 'Wave'})
        self.frame(3, {1: 'Wave'})
        self.frame(5, {1: 'Thumb_Up'})
        self.frame(7, {1: 'Thumb_Up'})
        self.summary.finish()
        self.assertEqual(len(self.lines), 4)
        self.assertIn('START session=s track=1 gesture=Wave frame=1', self.lines[0])
        self.assertIn('END session=s track=1 gesture=Wave frames=1..3', self.lines[1])
        self.assertIn('START session=s track=1 gesture=Thumb_Up frame=5', self.lines[2])
        self.assertIn('END session=s track=1 gesture=Thumb_Up frames=5..7', self.lines[3])

    def test_hands_and_sessions_have_independent_ranges(self):
        self.frame(1, {1: 'Wave', 2: 'Victory'})
        self.frame(1, {1: 'Thumb_Up'}, session='other')
        self.frame(3, {2: 'Victory'})
        self.frame(3, {1: 'Thumb_Up'}, session='other')
        self.frame(5, {2: 'Victory'})
        self.summary.finish()
        self.assertEqual(len(self.lines), 6)
        ends = [line for line in self.lines if '[GESTURE] END' in line]
        self.assertTrue(any('session=s track=1 gesture=Wave frames=1..1' in line for line in ends))
        self.assertTrue(any('session=s track=2 gesture=Victory frames=1..5' in line for line in ends))
        self.assertTrue(any('session=other track=1 gesture=Thumb_Up frames=1..3' in line for line in ends))

    def test_gesture_returning_after_absence_starts_new_range(self):
        self.frame(1, {1: 'Wave'})
        self.frame(3, {})
        self.frame(5, {1: 'Wave'})
        self.summary.finish()
        self.assertEqual(len(self.lines), 4)
        self.assertIn('frames=1..1', self.lines[1])
        self.assertIn('frame=5 ', self.lines[2])

    def test_late_snapshot_does_not_end_current_gesture(self):
        self.frame(5, {1: 'Wave'})
        self.frame(3, {})
        self.frame(7, {1: 'Wave'})
        self.summary.finish()
        self.assertEqual(len(self.lines), 2)
        self.assertIn('frames=5..7', self.lines[1])

    def test_epoch_restart_closes_range_and_allows_frame_counter_reset(self):
        self.frame(99, {1: 'Wave'})
        self.frame(1, {1: 'Wave'}, epoch='restarted')
        self.summary.finish()
        self.assertEqual(len(self.lines), 4)
        self.assertIn('frames=99..99', self.lines[1])
        self.assertIn('frame=1 ', self.lines[2])
        self.summary.finish()
        self.assertEqual(len(self.lines), 4)
