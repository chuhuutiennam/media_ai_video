#!/usr/bin/env python3
"""Summarize gesture changes while retaining the log offset across WSL read errors."""

import errno
import json
import os
from pathlib import Path
import signal
import sys
import time


MESSAGES = {'keyword: before mapping', 'keyword: after mapping'}


class GestureSummary:
    def __init__(self, emit=None):
        self.emit = emit or (lambda line: print(line, flush=True))
        self.frames = {}
        self.active = {}

    def _end(self, key):
        span = self.active.pop(key)
        session, track = key
        self.emit(f'[GESTURE] END session={session} track={track} gesture={span["gesture"]} '
                  f'frames={span["first"]}..{span["last"]} '
                  f'keyword={span["topic_label"]}/{span["topic_info"]}')

    def _flush(self, session):
        frame = self.frames.pop(session)
        for key in list(self.active):
            if key[0] == session and key[1] not in frame['hands']:
                self._end(key)
        for track, mapped in sorted(frame['hands'].items()):
            key = (session, track)
            current = self.active.get(key)
            if current and any(current[field] != mapped[field] for field in mapped):
                self._end(key)
                current = None
            if current is None:
                current = dict(mapped, first=frame['id'], last=frame['id'])
                self.active[key] = current
                self.emit(f'[GESTURE] START session={session} track={track} gesture={current["gesture"]} '
                          f'frame={current["first"]} keyword={current["topic_label"]}/{current["topic_info"]}')
            else:
                current['last'] = frame['id']

    def consume(self, row):
        session, frame_id = row.get('session_id'), row.get('frame_id')
        if not session or not isinstance(frame_id, int) or frame_id <= 0:
            return
        response = row.get('response_id', '')
        epoch = response.rsplit('/', 1)[0] if '/' in response else ''
        frame = self.frames.get(session)
        if frame:
            if frame['epoch'] == epoch and frame_id < frame['id']:
                return  # Late snapshots do not change the current gesture.
            if (frame['id'], frame['response']) != (frame_id, response):
                self._flush(session)
                if frame['epoch'] != epoch:
                    for key in list(self.active):
                        if key[0] == session:
                            self._end(key)
                frame = None
        if frame is None:
            frame = dict(id=frame_id, response=response, epoch=epoch, hands={})
            self.frames[session] = frame
        if row['msg'] == 'keyword: after mapping':
            track = row.get('track_id')
            mapped = {field: row.get(field) for field in ('gesture', 'topic_label', 'topic_info')}
            if isinstance(track, int) and track > 0 and all(mapped.values()):
                frame['hands'][track] = mapped

    def finish(self):
        for session in list(self.frames):
            self._flush(session)
        for key in list(self.active):
            self._end(key)


class KeywordLogReader:
    def __init__(self, path):
        self.path = Path(path)
        self.offset = 0
        self.pending = b''
        self.identity = None

    def poll(self):
        with self.path.open('rb') as stream:
            stat = os.fstat(stream.fileno())
            identity = (stat.st_dev, stat.st_ino)
            if identity != self.identity or stat.st_size < self.offset:
                self.offset, self.pending = 0, b''
            self.identity = identity
            stream.seek(self.offset)
            while True:
                chunk = stream.read(65536)
                if not chunk:
                    return
                self.offset += len(chunk)
                lines = (self.pending + chunk).split(b'\n')
                self.pending = lines.pop()
                for line in lines:
                    try:
                        row = json.loads(line)
                    except (ValueError, UnicodeDecodeError):
                        continue
                    if isinstance(row, dict) and row.get('msg') in MESSAGES:
                        yield line.decode('utf-8')


def process_alive(pid):
    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False
    except PermissionError:
        return True


def follow(path, pid, stop_requested=lambda: False):
    reader = KeywordLogReader(path)
    summary = GestureSummary()
    dead_since, warned = None, False
    while True:
        if (stop_requested() or not process_alive(pid)) and dead_since is None:
            dead_since = time.monotonic()
        try:
            for line in reader.poll():
                summary.consume(json.loads(line))
            warned = False
            if dead_since is not None:
                summary.finish()
                return 0
        except OSError as error:
            if error.errno not in (errno.ENOENT, errno.ENODATA):
                print(f'[WARN] keyword log reader failed: {error}; full log: {path}', file=sys.stderr)
                summary.finish()
                return 1
            if error.errno == errno.ENODATA and not warned:
                print(f'[WARN] keyword log read temporarily unavailable; retrying at byte {reader.offset}: {path}',
                      file=sys.stderr)
                warned = True
            if dead_since is not None and time.monotonic() - dead_since >= 2:
                print(f'[WARN] keyword log final read failed: {error}; inspect {path}', file=sys.stderr)
                summary.finish()
                return 1
        time.sleep(0.1)


if __name__ == '__main__':
    gateway_pid = int(sys.argv[2])
    if gateway_pid <= 0:
        raise SystemExit('gateway PID must be positive')
    stopped = [False]
    def request_stop(_signal, _frame):
        stopped[0] = True
    signal.signal(signal.SIGTERM, request_stop)
    signal.signal(signal.SIGINT, request_stop)
    sys.exit(follow(sys.argv[1], gateway_pid, lambda: stopped[0]))
