"""Capture landmarks once, then compare Wave policies without rerunning inference.

Run from repository root: python -m workers.gesture.src.wave_replay --help
"""
import argparse
from collections import Counter
from dataclasses import asdict, replace
import json
import math
from pathlib import Path

from .config import GestureConfig, OverlayConfig, WaveConfig, load_config
from .gesture_events import GestureEngine
from .hand_tracker import HandObservation


def read_frames(path):
    for line in path.read_text().splitlines():
        try:
            row = json.loads(line)
        except json.JSONDecodeError:
            if "wave_frame " not in line:
                continue
            row = json.loads(line.split("wave_frame ", 1)[1])
        if "message" in row:
            message = row["message"]
            if not message.startswith("wave_frame "):
                continue
            row = json.loads(message[len("wave_frame "):])
        if "hands" in row and "timestamp_ms" in row:
            yield row


def compare(frames, gesture=None, wave=None, overlay=None):
    """Separate tracker/state per variant AND per stream. No cross-leg mixing."""
    frames = list(frames)
    gesture, wave, overlay = gesture or GestureConfig(), wave or WaveConfig(), overlay or OverlayConfig()
    report = {}
    for signal in ("palm", "wrist"):
        for counter in ("hysteresis", "steps"):
            for gate, threshold in ((True, 0.5), (True, 0.6), (False, 0.5)):
                cfg = replace(wave, signal=signal, counter=counter, require_open_palm=gate,
                              open_palm_score_threshold=threshold)
                engines, events, reasons = {}, [], Counter()
                for row in frames:
                    key = (row.get("session_id", "replay"), row.get("stream_id", "video"))
                    if key not in engines:
                        engines[key] = GestureEngine(gesture, cfg, overlay, *key)
                    engine = engines[key]
                    hands = tuple(HandObservation(tuple(tuple(p) for p in h["landmarks"]),
                                  h.get("gesture", "None"), h.get("score", 0), h.get("handedness", ""))
                                  for h in row["hands"])
                    hits = engine.process(hands, row["timestamp_ms"], row["frame_id"], row.get("aspect", 1))
                    events.extend(asdict(e) for e in hits if e.gesture == "Wave")
                    for detector in engine.waves.values():
                        if detector.diagnostics.get("timestamp_ms") == row["timestamp_ms"]:
                            reasons[detector.diagnostics.get("reason", "missing")] += 1
                name = f"{signal}/{counter}/{'open-' + str(threshold) if gate else 'ungated'}"
                report[name] = {"config": asdict(cfg), "events": events, "decisions": dict(reasons)}
    return report


def negative_controls(frames):
    """Derived landmark controls, NOT recordings of actual negative scenes.

    A camera oscillation with the same hand trajectory is fundamentally
    ambiguous without a background/body reference. Report it explicitly.
    """
    frames = list(frames)
    base = next((h for row in frames for h in row["hands"] if h.get("gesture") == "Open_Palm"), None)
    if base is None:
        raise ValueError("controls require an observed Open_Palm")
    center = sum(base["landmarks"][i][0] for i in (0, 5, 9, 13, 17)) / 5
    controls = {}
    start = frames[0]["timestamp_ms"]
    duration = max(1, frames[-1]["timestamp_ms"] - start)
    for kind in ("held_open", "one_way", "jitter", "hand_switch", "camera_pan", "camera_oscillation"):
        rows = []
        for i, frame in enumerate(frames):
            elapsed = frame["timestamp_ms"] - start
            x = 0.5
            if kind in ("one_way", "camera_pan"):
                x = 0.2 + 0.5 * elapsed / duration
            elif kind == "jitter":
                x += 0.003 * (-1) ** i
            elif kind == "hand_switch":
                x = 0.15 if (elapsed // 400) % 2 else 0.85
            elif kind == "camera_oscillation":
                x += 0.14 * math.sin(2 * math.pi * 2 * elapsed / 1000)
            hand = dict(base, landmarks=[[px + x - center, py, pz] for px, py, pz in base["landmarks"]],
                        score=0.95)
            rows.append(dict(frame, session_id=kind, stream_id="control",
                             hands=[] if kind == "hand_switch" and elapsed % 400 < 80 else [hand]))
        controls[kind] = rows
    return controls


def capture(video, destination, config, width, height, fps):
    # VIDEO mode makes an offline reference deterministic and processes every
    # selected timestamp; this does not change the worker's LIVE_STREAM runtime.
    import cv2
    import mediapipe as mp
    from .gesture import create_recognizer, snapshot_result

    source = cv2.VideoCapture(str(video))
    if not source.isOpened():
        raise ValueError(f"cannot open {video}")
    native_fps = source.get(cv2.CAP_PROP_FPS)
    if native_fps <= 0:
        source.release()
        raise ValueError("video must have a positive FPS")
    count, next_ms = 0, 0.0
    try:
        with create_recognizer(config.gesture, config.gesture.model.read_bytes()) as task, destination.open("w") as out:
            index = 0
            while True:
                ok, frame = source.read()
                if not ok:
                    break
                timestamp = round(index * 1000 / native_fps)
                index += 1
                if timestamp < next_ms:
                    continue
                next_ms += 1000 / fps
                frame = cv2.resize(frame, (width, height))
                image = mp.Image(image_format=mp.ImageFormat.SRGB, data=cv2.cvtColor(frame, cv2.COLOR_BGR2RGB))
                hands = snapshot_result(task.recognize_for_video(image, timestamp))
                out.write(json.dumps({"session_id": str(video), "stream_id": "offline",
                    "frame_id": index, "timestamp_ms": timestamp, "aspect": width / height,
                    "hands": [asdict(h) for h in hands]}) + "\n")
                count += 1
    finally:
        source.release()
    return count


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    inputs = parser.add_mutually_exclusive_group(required=True)
    inputs.add_argument("--video", type=Path)
    inputs.add_argument("--replay", type=Path, help="landmark JSONL or worker DEBUG log")
    parser.add_argument("--config", type=Path)
    parser.add_argument("--capture", type=Path, help="JSONL destination; required with --video")
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--controls-output", type=Path, help="evaluate derived negative landmark controls")
    parser.add_argument("--width", type=int, default=320)
    parser.add_argument("--height", type=int, default=240)
    parser.add_argument("--fps", type=float, default=15)
    args = parser.parse_args()
    if args.video and (not args.config or not args.capture):
        parser.error("--video requires --config and --capture")
    if args.width <= 0 or args.height <= 0 or not 0 < args.fps <= 120:
        parser.error("positive dimensions and 0 < fps <= 120 required")
    cfg = load_config(args.config) if args.config else None
    if args.video:
        capture(args.video, args.capture, cfg, args.width, args.height, args.fps)
    frames = list(read_frames(args.capture if args.video else args.replay))
    if not frames:
        parser.error("no landmark frames found (worker logs require DEBUG)")
    report = compare(frames, cfg.gesture if cfg else None, cfg.wave if cfg else None, cfg.overlay if cfg else None)
    args.output.write_text(json.dumps({"frames": len(frames), "variants": report}, indent=2) + "\n")
    if args.controls_output:
        controls = {}
        for name, rows in negative_controls(frames).items():
            variants = compare(rows, cfg.gesture if cfg else None, cfg.wave if cfg else None, cfg.overlay if cfg else None)
            controls[name] = {key: len(item["events"]) for key, item in variants.items()}
        args.controls_output.write_text(json.dumps({"provenance": "derived landmark controls; not real negative videos",
            "known_limitation": "camera oscillation cannot be separated from hand motion without scene reference",
            "controls": controls}, indent=2) + "\n")
    for name, item in report.items():
        print(f"{name}: {len(item['events'])} Wave events")


if __name__ == "__main__":
    main()
