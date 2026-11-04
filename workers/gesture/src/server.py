"""JPEG gRPC gesture worker with per-stream MediaPipe LIVE_STREAM state."""
import argparse
import json
import logging
import math
import os
import signal
import sys
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from dataclasses import replace
from pathlib import Path
from uuid import uuid4

import cv2
import grpc
import mediapipe as mp
import numpy as np
from prometheus_client import start_http_server

from . import metrics
from .config import AppConfig, load_config, validate_gesture_config
from .contract import pb
from .gesture import create_recognizer, snapshot_result
from .gesture_events import GestureEngine
from .log import setup_logging

logger = logging.getLogger(__name__)
ACTIVE_STREAMS = metrics.ACTIVE_STREAMS


class CapacityError(Exception):
    pass


class Cancelled(Exception):
    pass


def jpeg_dimensions(data):
    """Read SOF before allocating decoded pixels; reject malformed JPEG headers."""
    if data[:2] != b"\xff\xd8":
        raise ValueError("image_data must be JPEG")
    offset = 2
    while offset < len(data):
        if data[offset] != 255:
            break
        while offset < len(data) and data[offset] == 255:
            offset += 1
        if offset >= len(data):
            break
        marker = data[offset]
        offset += 1
        if marker in (0xD9, 0xDA):
            break
        if marker == 0x01 or 0xD0 <= marker <= 0xD8:
            continue
        if offset + 2 > len(data):
            break
        length = int.from_bytes(data[offset:offset + 2], "big")
        if length < 2 or offset + length > len(data):
            break
        if marker in (0xC0, 0xC1, 0xC2, 0xC3, 0xC5, 0xC6, 0xC7, 0xC9, 0xCA, 0xCB, 0xCD, 0xCE, 0xCF):
            if length < 8:
                break
            return (int.from_bytes(data[offset + 5:offset + 7], "big"),
                    int.from_bytes(data[offset + 3:offset + 5], "big"))
        offset += length
    raise ValueError("invalid JPEG header")


class ReactionState:
    """Transport-only reactions: no image assets/rendering or borrowed landmarks."""
    def __init__(self, gesture, overlay):
        self.gesture, self.overlay = gesture, overlay
        self.active = {}

    def update(self, frame, events):
        now = frame.timestamp_ms
        visible = {hand.track_id for hand in frame.hands}
        self.active = {key: value for key, value in self.active.items()
                       if key in visible and value[1]["valid_until_ms"] > now}
        for event in events:
            previous = self.active.get(event.track_id)
            if previous and previous[0] == "dynamic" and event.kind == "static":
                continue
            self.active[event.track_id] = (event.kind, dict(event_id=event.event_id, gesture=event.gesture,
                emoji_key=event.emoji_key, started_at_ms=now, valid_until_ms=now + event.duration_ms))
        for hand in frame.hands:
            if (hand.stable_gesture and hand.gesture == hand.stable_gesture and
                    hand.score >= self.gesture.static_score_threshold):
                previous = self.active.get(hand.track_id)
                if previous and previous[0] == "dynamic":
                    continue
                if not previous or previous[1]["gesture"] != hand.stable_gesture:
                    self.active[hand.track_id] = ("static", dict(event_id=uuid4().hex, gesture=hand.stable_gesture,
                        emoji_key=self.gesture.emoji_mapping[hand.stable_gesture], started_at_ms=now,
                        valid_until_ms=now + self.overlay.duration_ms))
                else:
                    previous[1]["valid_until_ms"] = now + self.overlay.duration_ms
        return {key: value[1] for key, value in self.active.items()} if self.overlay.enabled else {}


class Worker:
    def __init__(self, config, factory=create_recognizer):
        validate_gesture_config(config.gesture, config.wave, config.overlay)
        if not config.gesture.enabled or config.gesture.model is None:
            raise ValueError("enabled gesture and gesture.model are required")
        for name in ("max_streams", "max_receive_message_bytes", "max_inference_dim"):
            value = getattr(config.server, name)
            if type(value) is not int or value < 1:
                raise ValueError(f"server.{name} must be a positive integer")
        if config.server.max_inference_dim > 4096:
            raise ValueError("server.max_inference_dim must not exceed 4096")
        if not math.isfinite(config.server.shutdown_grace_secs) or config.server.shutdown_grace_secs < 0:
            raise ValueError("server.shutdown_grace_secs must be finite and nonnegative")
        if config.metrics.enabled and (type(config.metrics.port) is not int or not 1 <= config.metrics.port <= 65535):
            raise ValueError("metrics.port must be in 1..65535")
        interval = config.metrics.log_interval_secs
        if isinstance(interval, bool) or not isinstance(interval, (int, float)) or not math.isfinite(interval) or interval < 0:
            raise ValueError("metrics.log_interval_secs must be finite and nonnegative")
        self.config, self.factory = config, factory
        self.model_bytes = config.gesture.model.read_bytes()
        self.slots = threading.BoundedSemaphore(config.gesture.max_concurrent_recognizers)

    def probe(self):
        task = self.factory(self.config.gesture, self.model_bytes, lambda *_: None)
        task.close()

    def Segment(self, requests, context):
        task = None
        acquired = False
        session, stream, frames, outcome = "unknown", "unknown", 0, "complete"
        opened = time.monotonic()
        try:
            first = next(requests, None)
            if first is None or first.WhichOneof("payload") != "config" or first.is_eos or not first.session_id or not first.stream_id:
                raise ValueError("first message must contain config, session_id and stream_id")
            profile = first.config
            session, stream = first.session_id, first.stream_id
            if (profile.ai_service != "gesture_recognition" or profile.input_format != "jpeg" or not profile.media_epoch or
                    not 1 <= profile.max_hands <= 8 or not 1 <= profile.inference_fps <= 60):
                raise ValueError("expected gesture_recognition config with JPEG, media_epoch, FPS and max_hands")
            if (bool(profile.inference_width) != bool(profile.inference_height) or
                    max(profile.inference_width, profile.inference_height) > self.config.server.max_inference_dim):
                raise ValueError("invalid configured JPEG dimensions")
            if not self.slots.acquire(blocking=False):
                raise CapacityError("gesture recognizer capacity exhausted")
            acquired = True
            ACTIVE_STREAMS.inc()
            cfg = replace(self.config.gesture, num_hands=int(profile.max_hands))
            engine = GestureEngine(cfg, self.config.wave, self.config.overlay, first.session_id, first.stream_id)
            reactions = ReactionState(cfg, self.config.overlay)
            epoch = uuid4().hex
            completed = threading.Event()
            lock = threading.Lock()
            expected_ts, result = [-1], [None]

            def callback(native_result, _image, timestamp):
                with lock:
                    if timestamp != expected_ts[0]:
                        return
                    try:
                        result[0] = (snapshot_result(native_result), None)
                    except Exception as exc:
                        result[0] = (None, exc)
                    completed.set()

            task = self.factory(cfg, self.model_bytes, callback)
            metrics.ACTIVE_RECOGNIZERS.inc()
            logger.info("stream_open session=%s stream=%s media_epoch=%s recognizer_epoch=%s delegate=%s max_hands=%d fps=%d",
                        session, stream, profile.media_epoch, epoch, cfg.delegate, cfg.num_hands, profile.inference_fps)
            last_id, last_ts, seq = 0, -1, 0
            for request in requests:
                if not context.is_active():
                    outcome = "cancelled"
                    return
                if (request.session_id, request.stream_id) != (first.session_id, first.stream_id):
                    raise ValueError("session_id/stream_id changed within stream")
                if request.is_eos and request.WhichOneof("payload") is None:
                    return
                if request.WhichOneof("payload") != "frame":
                    raise ValueError("expected frame after config")
                frame = request.frame
                if (frame.frame_id <= last_id or frame.timestamp_ms <= last_ts or frame.encoding != "jpeg" or
                        not 0 <= frame.rtp_timestamp <= 0xFFFFFFFF or not 0 < frame.original_width <= 16384 or
                        not 0 < frame.original_height <= 16384 or not 0 < frame.width <= self.config.server.max_inference_dim or
                        not 0 < frame.height <= self.config.server.max_inference_dim):
                    raise ValueError("invalid JPEG frame metadata or non-monotonic frame/time")
                size = jpeg_dimensions(frame.image_data)
                if size != (frame.width, frame.height) or (profile.inference_width and size != (profile.inference_width, profile.inference_height)):
                    raise ValueError("JPEG dimensions do not match frame/config")
                started = time.monotonic()
                bgr = cv2.imdecode(np.frombuffer(frame.image_data, np.uint8), cv2.IMREAD_COLOR)
                if bgr is None or bgr.shape[:2] != (frame.height, frame.width):
                    raise ValueError("JPEG decode failed")
                image = mp.Image(image_format=mp.ImageFormat.SRGB, data=cv2.cvtColor(bgr, cv2.COLOR_BGR2RGB))
                last_id, last_ts = frame.frame_id, frame.timestamp_ms
                with lock:
                    expected_ts[0], result[0] = frame.timestamp_ms, None
                    completed.clear()
                task.recognize_async(image, frame.timestamp_ms)
                deadline = time.monotonic() + cfg.result_timeout_secs
                while not completed.wait(min(0.05, max(0, deadline - time.monotonic()))):
                    if not context.is_active():
                        raise Cancelled()
                    if time.monotonic() >= deadline:
                        raise TimeoutError("MediaPipe gesture callback timed out")
                with lock:
                    observations, error = result[0]
                    expected_ts[0] = -1
                if error:
                    raise RuntimeError("invalid MediaPipe result") from error
                if len(observations) > cfg.num_hands:
                    raise RuntimeError("recognizer exceeded max_hands")
                events = engine.process(observations, frame.timestamp_ms, frame.frame_id, frame.width / frame.height,
                                        frame.original_width, frame.original_height)
                hand_frame = engine.last_frame
                active = reactions.update(hand_frame, events)
                seq += 1
                snapshot = pb.GestureResult(media_epoch=profile.media_epoch, recognizer_epoch=epoch,
                    response_id=f"{epoch}/{seq}", frame_id=frame.frame_id, rtp_timestamp=frame.rtp_timestamp,
                    extended_rtp_timestamp=frame.extended_rtp_timestamp, timestamp_ms=frame.timestamp_ms,
                    original_width=frame.original_width, original_height=frame.original_height, status="ok")
                for hand in hand_frame.hands:
                    bounds = hand.bbox_normalized
                    if bounds[0] >= bounds[2] or bounds[1] >= bounds[3] or not all(math.isfinite(x) for x in (*bounds, *hand.center_normalized, hand.score)):
                        logger.warning("discarding invalid hand geometry stream=%s track=%s", first.stream_id, hand.track_id)
                        continue
                    out = snapshot.hands.add(track_id=hand.track_id, handedness=hand.handedness, raw_label=hand.gesture,
                        raw_score=max(0.0, min(1.0, hand.score)), stable_label=hand.stable_gesture or "",
                        center=hand.center_normalized, bbox=bounds)
                    if hand.track_id in active:
                        out.active_reaction.CopyFrom(pb.Reaction(**active[hand.track_id]))
                metrics.GESTURE_FRAMES.labels(status="processed").inc()
                metrics.GESTURE_LATENCY.observe(time.monotonic() - started)
                for event in events:
                    metrics.GESTURE_EVENTS.labels(gesture=event.gesture, kind=event.kind).inc()
                    logger.info("gesture_event session=%s stream=%s frame=%d time_ms=%d track=%s gesture=%s kind=%s emoji=%s score=%s",
                                session, stream, frame.frame_id, frame.timestamp_ms, event.track_id, event.gesture,
                                event.kind, event.emoji_key, event.score)
                if hand_frame.hands and logger.isEnabledFor(logging.DEBUG):
                    logger.debug("hand_observation %s", json.dumps(hand_frame.to_dict()))
                frames += 1
                logger.debug("frame_processed session=%s stream=%s frame=%d time_ms=%d hands=%d events=%d latency_ms=%.1f",
                             session, stream, frame.frame_id, frame.timestamp_ms, len(snapshot.hands), len(events),
                             (time.monotonic() - started) * 1000)
                yield pb.VideoResponse(session_id=first.session_id, stream_id=first.stream_id,
                    ai_service="gesture_recognition", seq=seq, gesture=snapshot)
                if request.is_eos:
                    return
        except CapacityError as exc:
            outcome = "capacity"
            logger.warning("stream_rejected session=%s stream=%s reason=capacity error=%s", session, stream, exc)
            metrics.GESTURE_ERRORS.labels(reason="capacity").inc()
            context.abort(grpc.StatusCode.RESOURCE_EXHAUSTED, str(exc))
        except Cancelled:
            outcome = "cancelled"
            return
        except ValueError as exc:
            outcome = "invalid_argument"
            logger.warning("stream_invalid session=%s stream=%s error=%s", session, stream, exc)
            metrics.GESTURE_ERRORS.labels(reason="invalid_argument").inc()
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, str(exc))
        except TimeoutError as exc:
            outcome = "timeout"
            logger.error("stream_timeout session=%s stream=%s callback_timeout_secs=%s error=%s",
                         session, stream, self.config.gesture.result_timeout_secs, exc)
            metrics.GESTURE_ERRORS.labels(reason="timeout").inc()
            context.abort(grpc.StatusCode.DEADLINE_EXCEEDED, str(exc))
        except grpc.RpcError:
            outcome = "rpc_closed"
            logger.info("stream_rpc_closed session=%s stream=%s", session, stream)
            raise
        except Exception as exc:
            outcome = "internal"
            metrics.GESTURE_ERRORS.labels(reason="task").inc()
            logger.exception("gesture stream failed session=%s stream=%s", session, stream)
            context.abort(grpc.StatusCode.INTERNAL, str(exc))
        finally:
            try:
                if task is not None:
                    task.close()
            except Exception:
                logger.exception("recognizer close failed session=%s stream=%s", session, stream)
                raise
            finally:
                if task is not None:
                    metrics.ACTIVE_RECOGNIZERS.dec()
                if acquired:
                    ACTIVE_STREAMS.dec()
                    self.slots.release()
                metrics.STREAMS_TOTAL.inc()
                logger.info("stream_closed session=%s stream=%s outcome=%s frames=%d elapsed_ms=%.1f",
                            session, stream, outcome, frames, (time.monotonic() - opened) * 1000)


class VideoStreamHandler(grpc.GenericRpcHandler):
    def __init__(self, worker):
        self.handler = grpc.stream_stream_rpc_method_handler(worker.Segment,
            request_deserializer=pb.VideoRequest.FromString, response_serializer=pb.VideoResponse.SerializeToString)

    def service(self, details):
        return self.handler if details.method == "/video.VideoStream/Segment" else None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", type=Path)
    parser.add_argument("--addr")
    parser.add_argument("--model", type=Path, help="gesture_recognizer.task")
    parser.add_argument("--delegate", choices=("cpu", "gpu"))
    parser.add_argument("--max-streams", type=int)
    parser.add_argument("--result-timeout", type=float, help="seconds to wait for a LIVE_STREAM callback")
    parser.add_argument("--metrics-port", type=int, help="0 disables metrics HTTP server")
    parser.add_argument("--log-level", choices=("DEBUG", "INFO", "WARNING", "ERROR"))
    args = parser.parse_args()
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s")
    server, periodic_stop = None, None
    try:
        cfg = load_config(args.config) if args.config else AppConfig()
        cfg.gesture.enabled = True
        if args.addr:
            cfg.server.addr = args.addr
        if args.model:
            cfg.gesture.model = args.model
        if args.delegate:
            cfg.gesture.delegate = args.delegate
        if args.result_timeout is not None:
            cfg.gesture.result_timeout_secs = args.result_timeout
        if args.max_streams is not None:
            cfg.server.max_streams = args.max_streams
        if args.log_level:
            cfg.logging.level = args.log_level
        if args.metrics_port is not None:
            cfg.metrics.enabled, cfg.metrics.port = args.metrics_port != 0, args.metrics_port
        setup_logging(cfg.logging)
        logger.debug("startup_config pid=%s python=%s mediapipe=%s config=%s grpc=%s metrics=%s delegate=%s model=%s rpc_limit=%s recognizers=%s max_hands=%s result_timeout_secs=%s static_threshold=%s wave=%s log_file=%s",
                    os.getpid(), sys.executable, mp.__version__, args.config or "defaults", cfg.server.addr,
                    cfg.metrics.port if cfg.metrics.enabled else "disabled", cfg.gesture.delegate, cfg.gesture.model,
                    cfg.server.max_streams, cfg.gesture.max_concurrent_recognizers, cfg.gesture.num_hands,
                    cfg.gesture.result_timeout_secs, cfg.gesture.static_score_threshold, cfg.wave.enabled, cfg.logging.file)
        worker = Worker(cfg)
        logger.info("Initializing gesture model (delegate=%s) ...", cfg.gesture.delegate)
        worker.probe()
        logger.info("Gesture model initialized")
        if cfg.metrics.enabled:
            start_http_server(cfg.metrics.port)
            logger.info("metrics server ready port=%s path=/metrics", cfg.metrics.port)
        server = grpc.server(ThreadPoolExecutor(max_workers=cfg.server.max_streams),
                             maximum_concurrent_rpcs=cfg.server.max_streams,
                             options=(("grpc.max_receive_message_length", cfg.server.max_receive_message_bytes),
                                      ("grpc.so_reuseport", 0)))
        server.add_generic_rpc_handlers((VideoStreamHandler(worker),))
        if not server.add_insecure_port(cfg.server.addr):
            raise RuntimeError(f"cannot bind {cfg.server.addr}")
        server.start()
        periodic_stop = metrics.start_periodic_logger(cfg.metrics.log_interval_secs)
        logger.info("gesture server ready addr=%s delegate=%s", cfg.server.addr, cfg.gesture.delegate)
    except Exception as exc:
        logger.exception("startup_failed config=%s error=%s; check model/delegate, ports, venv dependencies and logging path", args.config, exc)
        if server is not None:
            server.stop(0).wait()
        parser.exit(1, f"Cannot start gesture worker: {exc}. Review startup_failed and config {args.config}.\n")

    def shutdown(received_signal, _frame):
        logger.info("server_shutdown signal=%s grace_secs=%s", received_signal, cfg.server.shutdown_grace_secs)
        server.stop(cfg.server.shutdown_grace_secs)

    signal.signal(signal.SIGTERM, shutdown)
    try:
        server.wait_for_termination()
    except KeyboardInterrupt:
        shutdown(signal.SIGINT, None)
    finally:
        if periodic_stop is not None:
            periodic_stop.set()
        server.stop(cfg.server.shutdown_grace_secs).wait()
        logger.info("server_stopped metrics=%s", json.dumps(metrics.snapshot()))


if __name__ == "__main__":
    main()
