"""Test file videos with per-hand gesture logging and anchored cartoon stickers."""
import argparse
import json
import logging
import math
import time
from contextlib import ExitStack
from dataclasses import asdict
from pathlib import Path

import cv2
import mediapipe as mp
import numpy as np

from .config import AppConfig, load_config, validate_gesture_config
from .gesture import GesturePool
from .overlay import EmojiOverlay
from .video import VideoGestureProcessor

logger = logging.getLogger(__name__)


def composite_background(bgr, foreground, effect, background=None):
    if effect == "bg_blur":
        back = cv2.GaussianBlur(bgr, (31, 31), 0)
    elif effect == "bg_replace":
        back = cv2.resize(background, (bgr.shape[1], bgr.shape[0]))
    else:
        back = np.zeros_like(bgr)
    if foreground.shape != bgr.shape[:2]:
        foreground = cv2.resize(foreground.astype(np.uint8), (bgr.shape[1], bgr.shape[0]), interpolation=cv2.INTER_NEAREST).astype(bool)
    return np.where(foreground[:, :, None], bgr, back)


def draw_hands(bgr, hand_frame, mirror=False):
    if hand_frame is None:
        return bgr
    height, width = bgr.shape[:2]
    for hand in hand_frame.hands:
        left, top, right, bottom = hand.bbox_normalized
        if mirror:
            left, right = 1 - right, 1 - left
        x1, y1, x2, y2 = round(left * width), round(top * height), round(right * width), round(bottom * height)
        cv2.rectangle(bgr, (x1, y1), (x2, y2), (80, 235, 110), 2)
        label = f"#{hand.track_id} {hand.gesture} {hand.score:.2f}"
        cv2.putText(bgr, label, (x1, max(16, y1 - 5)), cv2.FONT_HERSHEY_SIMPLEX, 0.5, (80, 235, 110), 1, cv2.LINE_AA)
    return bgr


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", type=Path)
    parser.add_argument("--source", type=Path, required=True, help="input video file (no webcam)")
    parser.add_argument("--mode", choices=("video", "realtime"), default="video",
                        help="video: process every frame; realtime: paced LIVE_STREAM replay with bounded dropping")
    parser.add_argument("--model", type=Path, help="optional segmentation .tflite model")
    parser.add_argument("--gesture-model", type=Path)
    parser.add_argument("--num-hands", type=int, help="maximum hands, 1..8 (default from config)")
    parser.add_argument("--score-threshold", type=float)
    parser.add_argument("--effect", choices=("none", "bg_blur", "bg_replace", "bg_remove"), default="none")
    parser.add_argument("--background", type=Path)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--events", type=Path, help="confirmed gesture JSONL")
    parser.add_argument("--hands-log", type=Path, help="per-frame hands JSONL; defaults beside output or events")
    parser.add_argument("--draw-hands", action="store_true", help="debug hand boxes, track IDs and raw labels")
    parser.add_argument("--no-preview", action="store_true")
    parser.add_argument("--mirror", action="store_true")
    parser.add_argument("--max-frames", type=int)
    parser.add_argument("--metrics-port", type=int)
    args = parser.parse_args()
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    cfg = load_config(args.config) if args.config else AppConfig()
    cfg.gesture.enabled = True
    cfg.gesture.model = args.gesture_model or cfg.gesture.model
    if args.num_hands is not None:
        cfg.gesture.num_hands = args.num_hands
    if args.score_threshold is not None:
        cfg.gesture.static_score_threshold = args.score_threshold
    try:
        validate_gesture_config(cfg.gesture, cfg.wave, cfg.overlay)
    except ValueError as exc:
        parser.error(str(exc))
    if not args.source.is_file():
        parser.error("--source must be an existing video file; webcam is disabled")
    if cfg.gesture.model is None or not cfg.gesture.model.is_file():
        parser.error("set --gesture-model or gesture.model to an existing .task file")
    if args.max_frames is not None and args.max_frames <= 0:
        parser.error("--max-frames must be positive")
    if args.no_preview and not any((args.output, args.events, args.hands_log)):
        parser.error("--no-preview requires --output, --events or --hands-log")
    if args.effect != "none" and (args.model is None or not args.model.is_file()):
        parser.error("set --model for optional background effects")
    if args.metrics_port is not None and not 1 <= args.metrics_port <= 65535:
        parser.error("--metrics-port must be in 1..65535")
    if args.hands_log is None and (args.output or args.events):
        args.hands_log = (args.output or args.events).with_suffix(".hands.jsonl")
    outputs = [p.absolute() for p in (args.output, args.events, args.hands_log) if p is not None]
    if len(set(outputs)) != len(outputs) or args.source.absolute() in outputs:
        parser.error("input/output/event/hand-log paths must be distinct")
    background = cv2.imread(str(args.background)) if args.background else None
    if args.effect == "bg_replace" and background is None:
        parser.error("bg_replace requires a readable --background image")

    with ExitStack() as stack:
        if args.metrics_port is not None:
            from prometheus_client import start_http_server

            http_server, _http_thread = start_http_server(args.metrics_port)
            stack.callback(http_server.server_close)
            stack.callback(http_server.shutdown)
        capture = cv2.VideoCapture(str(args.source))
        stack.callback(capture.release)
        if not capture.isOpened():
            parser.error(f"cannot open video: {args.source}")
        fps = capture.get(cv2.CAP_PROP_FPS)
        fps = fps if math.isfinite(fps) and 0 < fps <= 240 else 30.0
        event_file = stack.enter_context(args.events.open("w", encoding="utf-8")) if args.events else None
        hand_file = stack.enter_context(args.hands_log.open("w", encoding="utf-8")) if args.hands_log else None

        def record_event(event):
            value = json.dumps(asdict(event), ensure_ascii=False)
            logger.info("gesture_event %s", value)
            if event_file:
                event_file.write(value + "\n")
                event_file.flush()

        if args.mode == "video":
            processor = stack.enter_context(VideoGestureProcessor(cfg.gesture, cfg.wave, cfg.overlay))
        else:
            stream = GesturePool(cfg.gesture, cfg.wave, cfg.overlay).open("video-replay", "video")
            if stream is None:
                parser.error("gesture model unavailable")
            stack.callback(stream.close)
            if not stream.ready.wait(15) or stream.error is not None:
                raise RuntimeError(f"cannot initialize gesture recognizer: {stream.error}")
        overlay = EmojiOverlay(cfg.overlay, cfg.gesture.max_result_age_ms)
        segmentation = None
        if args.effect != "none":
            from .segmentation_adapter import SegmentationAdapter

            segmentation = stack.enter_context(SegmentationAdapter(args.model))
        if not args.no_preview:
            stack.callback(cv2.destroyAllWindows)
        writer = None
        started, frame_id, last_hand_timestamp = time.monotonic(), 0, -1
        while args.max_frames is None or frame_id < args.max_frames:
            ok, bgr = capture.read()
            if not ok:
                break
            timestamp_ms = round(frame_id * 1000 / fps)
            frame_id += 1
            if args.mode == "realtime":
                delay = started + timestamp_ms / 1000 - time.monotonic()
                if delay > 0:
                    time.sleep(delay)
            image = mp.Image(image_format=mp.ImageFormat.SRGB, data=cv2.cvtColor(bgr, cv2.COLOR_BGR2RGB))
            if args.mode == "video":
                events, hand_frame = processor.process(image, timestamp_ms, frame_id)
            else:
                stream.submit(image, timestamp_ms, frame_id)
                if stream.error is not None:
                    raise RuntimeError(f"gesture failed: {stream.error}")
                events, hand_frame = stream.drain_events(), stream.latest_hand_frame()
            overlay.update_hands(hand_frame)
            if hand_frame and hand_frame.timestamp_ms > last_hand_timestamp:
                last_hand_timestamp = hand_frame.timestamp_ms
                if hand_file:
                    hand_file.write(json.dumps(hand_frame.to_dict()) + "\n")
                    hand_file.flush()
            for event in events:
                record_event(event)
                overlay.accept(event, timestamp_ms)
            if segmentation:
                bgr = composite_background(bgr, segmentation.mask(image, timestamp_ms), args.effect, background)
            if args.mirror:
                bgr = cv2.flip(bgr, 1)
            if args.draw_hands:
                bgr = draw_hands(bgr, hand_frame, args.mirror)
            bgr = overlay.render(bgr, source_timestamp_ms=timestamp_ms, mirror=args.mirror)
            if args.output:
                if writer is None:
                    writer = cv2.VideoWriter(str(args.output), cv2.VideoWriter_fourcc(*"mp4v"), fps, (bgr.shape[1], bgr.shape[0]))
                    stack.callback(writer.release)
                    if not writer.isOpened():
                        raise RuntimeError(f"cannot open output: {args.output}")
                writer.write(bgr)
            if not args.no_preview:
                cv2.imshow("Video gesture test", bgr)
                if cv2.waitKey(1) & 0xFF in (ord("q"), 27):
                    break
        logger.info("video_complete frames=%d mode=%s hands_log=%s", frame_id, args.mode, args.hands_log)


if __name__ == "__main__":
    main()
