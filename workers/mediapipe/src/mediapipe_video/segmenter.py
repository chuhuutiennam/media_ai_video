"""Worker class: MediaPipe ImageSegmenter lifecycle and per-stream gRPC handler."""
import logging
import math
import threading
import time
from queue import Empty, Full, Queue, SimpleQueue

import cv2
import grpc
import mediapipe as mp
import numpy as np

from mediapipe_video import metrics
from mediapipe_video.codec import encode_rle, threshold_mask
from mediapipe_video.config import MediaPipeConfig, SegmentationConfig
from mediapipe_video.contract import pb

logger = logging.getLogger(__name__)


def _abort(context, code, message):
    """Increment stream error metric then abort the gRPC context."""
    metrics.STREAM_ERRORS_TOTAL.labels(reason=code.name.lower()).inc()
    context.abort(code, message)


class Worker:
    def __init__(self, mp_config: MediaPipeConfig, seg_config: SegmentationConfig | None = None):
        self.base_options = mp.tasks.BaseOptions(
            model_asset_buffer=mp_config.model.read_bytes(),
            delegate=mp.tasks.BaseOptions.Delegate[mp_config.delegate.upper()],
        )
        self.foreground_index = mp_config.foreground_index
        self.result_timeout = mp_config.result_timeout_secs
        self.seg_config = seg_config or SegmentationConfig()
        # Semaphore to cap concurrent GPU EGL contexts; 0 means no limit.
        limit = self.seg_config.max_concurrent_segmenters
        self._segmenter_sem = threading.Semaphore(limit) if limit > 0 else None
        # Validate model and delegate before accepting any gRPC calls.
        # LIVE_STREAM requires a callback even for this startup probe.
        with self.create_segmenter(lambda result, image, timestamp_ms: None):
            pass

    def create_segmenter(self, callback):
        return mp.tasks.vision.ImageSegmenter.create_from_options(
            mp.tasks.vision.ImageSegmenterOptions(
                base_options=self.base_options,
                running_mode=mp.tasks.vision.RunningMode.LIVE_STREAM,
                output_confidence_masks=True,
                output_category_mask=False,
                result_callback=callback,
            )
        )

    def foreground_mask(self, result, threshold):
        probabilities = result.confidence_masks or []
        index = self.foreground_index
        if index is None:
            # SelfieSegmenter: one person plane; binary models: background/person.
            if len(probabilities) not in (1, 2):
                raise ValueError("expected 1 or 2 confidence masks; multiclass model requires --foreground-index")
            index = 0 if len(probabilities) == 1 else 1
        if not 0 <= index < len(probabilities):
            raise ValueError("foreground-index outside confidence masks")
        # Threshold inside the callback: numpy_view() borrows MediaPipe memory.
        return threshold_mask(probabilities[index].numpy_view(), threshold)

    def wait_for_result(self, results, timestamp_ms, context):
        deadline = time.monotonic() + self.result_timeout
        while True:
            if not context.is_active():
                return  # stream already cancelled or aborted — status code already set
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                # End this stream so a late callback cannot be matched to another frame.
                _abort(context, grpc.StatusCode.DEADLINE_EXCEEDED, "timed out waiting for MediaPipe callback")
                return
            try:
                timestamp, foreground, error = results.get(timeout=min(self.seg_config.poll_interval_secs, remaining))
            except Empty:
                continue
            if timestamp < timestamp_ms:
                # LIVE_STREAM dropped a frame — discard stale callback, keep waiting
                metrics.FRAME_DROPS_TOTAL.inc()
                continue
            if timestamp != timestamp_ms:
                _abort(context, grpc.StatusCode.INTERNAL, "MediaPipe callback timestamp does not match frame")
                return
            if error is not None:
                raise ValueError(error)
            return foreground

    def Segment(self, requests, context):
        metrics.ACTIVE_STREAMS.inc()
        try:
            yield from self._segment(requests, context)
        finally:
            metrics.STREAMS_TOTAL.inc()
            metrics.ACTIVE_STREAMS.dec()

    def _frame_response(self, req, frame, mask, started, foreground):
        """Finalize latency, metrics and log for one frame, then build its VideoResponse."""
        elapsed = time.perf_counter() - started
        mask.latency_ms = elapsed * 1000
        metrics.FRAMES_TOTAL.labels(status=mask.status).inc()
        metrics.INFERENCE_LATENCY.observe(elapsed)
        metrics.FRAME_BYTES.observe(len(frame.image_data))
        if mask.status == "ok":
            metrics.FOREGROUND_RATIO.observe(float(foreground.mean()))
            logger.info(
                "inference  session=%s  frame=%d  jpeg=%dB  fg=%d/%dpx  latency=%.1fms",
                req.session_id, frame.frame_id, len(frame.image_data),
                int(foreground.sum()), int(foreground.size), mask.latency_ms,
            )
        else:
            logger.error(
                "frame error  session=%s  frame=%d  %s  latency=%.1fms",
                req.session_id, frame.frame_id, mask.error_message, mask.latency_ms,
            )
        return pb.VideoResponse(
            session_id=req.session_id,
            stream_id=req.stream_id,
            mask=mask,
            is_final=True,
        )

    def _segment(self, requests, context):
        first = next(requests, None)
        if first is None or first.WhichOneof("payload") != "config" or not first.session_id or not first.stream_id or first.is_eos:
            _abort(context, grpc.StatusCode.INVALID_ARGUMENT, "first message must contain config, session_id, and stream_id")
            return
        config = first.config
        if config.ai_service not in ("", "segmentation"):
            _abort(context, grpc.StatusCode.INVALID_ARGUMENT, "worker supports segmentation only")
            return
        if config.input_format not in ("", "jpeg"):
            _abort(context, grpc.StatusCode.INVALID_ARGUMENT, "input_format must be jpeg")
            return
        if config.effect_type not in self.seg_config.allowed_effect_types:
            _abort(context, grpc.StatusCode.INVALID_ARGUMENT, "unsupported effect_type")
            return
        # proto3 float default is 0.0 (falsy) — treat 0.0 as "not set", use config default
        threshold = config.threshold if config.threshold != 0.0 else self.seg_config.default_threshold
        if not math.isfinite(threshold) or not 0 <= threshold <= 1:
            _abort(context, grpc.StatusCode.INVALID_ARGUMENT, "threshold must be in [0,1]")
            return
        max_dim = self.seg_config.max_inference_dim
        if not (0 < config.inference_width <= max_dim and 0 < config.inference_height <= max_dim):
            _abort(context, grpc.StatusCode.INVALID_ARGUMENT, f"inference dimensions must be in 1..{max_dim}")
            return

        if self._segmenter_sem and not self._segmenter_sem.acquire(blocking=True, timeout=self.result_timeout):
            metrics.SEGMENTER_EXHAUSTED_TOTAL.inc()
            _abort(context, grpc.StatusCode.RESOURCE_EXHAUSTED, "max concurrent GPU segmenters reached")
            return

        results = SimpleQueue()
        # decode_queue carries (req, frame, mp_image, error_msg, started).
        # None sentinel signals end-of-stream from the producer.
        decode_queue = Queue(maxsize=2)
        stop_event = threading.Event()

        def on_result(result, image, timestamp_ms):
            try:
                foreground = self.foreground_mask(result, threshold)
                results.put((timestamp_ms, foreground, None))
            except Exception as exc:
                # Never let a Python callback exception strand the gRPC consumer.
                results.put((timestamp_ms, None, str(exc)))

        def _producer():
            """Read + validate + decode frames from the gRPC stream into decode_queue.

            Runs in a dedicated thread so the GPU consumer never blocks on network I/O
            or JPEG decode — it only blocks on wait_for_result (pure GPU time).
            """
            try:
                last_ts = -1
                for req in requests:
                    if stop_event.is_set() or not context.is_active():
                        return
                    if req.session_id != first.session_id or req.stream_id != first.stream_id:
                        _abort(context, grpc.StatusCode.INVALID_ARGUMENT, "session_id/stream_id changed within stream")
                        return
                    payload = req.WhichOneof("payload")
                    if req.is_eos and payload is None:
                        return  # EOS sentinel — consumer finishes the queued frames then exits
                    if payload != "frame":
                        _abort(context, grpc.StatusCode.INVALID_ARGUMENT, "expected frame after config")
                        return
                    frame = req.frame
                    started = time.perf_counter()
                    mp_image = None
                    error_msg = None
                    try:
                        if frame.encoding != "jpeg":
                            raise ValueError("encoding must be jpeg")
                        if frame.timestamp_ms <= last_ts:
                            raise ValueError("timestamp_ms must increase within a stream")
                        last_ts = frame.timestamp_ms
                        nparr = np.frombuffer(frame.image_data, np.uint8)
                        bgr = cv2.imdecode(nparr, cv2.IMREAD_COLOR)
                        if bgr is None or bgr.shape[:2] != (config.inference_height, config.inference_width):
                            raise ValueError("JPEG decode failed or dimensions do not match config")
                        rgb = cv2.cvtColor(bgr, cv2.COLOR_BGR2RGB)
                        mp_image = mp.Image(image_format=mp.ImageFormat.SRGB, data=rgb)
                    except (ValueError, OSError, RuntimeError) as exc:
                        error_msg = str(exc)
                    # Use a timeout loop so stop_event can interrupt a full queue.
                    while not stop_event.is_set():
                        try:
                            decode_queue.put((req, frame, mp_image, error_msg, started), timeout=0.1)
                            break
                        except Full:
                            continue
                    if req.is_eos:
                        return  # last frame queued — sentinel follows via finally
            except grpc.RpcError:
                # The request iterator raises on cancellation/abort during call
                # teardown. The consumer still needs the sentinel below.
                logger.debug("request stream ended session=%s", first.session_id)
            finally:
                # Always put the sentinel so the consumer is never left hanging.
                try:
                    decode_queue.put(None, timeout=2.0)
                except Full:
                    pass

        producer_thread = threading.Thread(
            target=_producer, daemon=True,
            name=f"mp-producer-{first.session_id[:8]}",
        )
        producer_thread.start()

        try:
            with self.create_segmenter(on_result) as segmenter:
                metrics.ACTIVE_SEGMENTERS.inc()
                try:
                    while True:
                        try:
                            item = decode_queue.get(timeout=self.result_timeout + 1.0)
                        except Empty:
                            if not producer_thread.is_alive():
                                break  # producer died without sentinel — exit cleanly
                            continue
                        if item is None:
                            break  # producer signalled end-of-stream

                        req, frame, mp_image, error_msg, started = item
                        mask = pb.VideoMask(frame_id=frame.frame_id, rtp_timestamp=frame.rtp_timestamp)
                        foreground = None
                        if error_msg:
                            mask.status, mask.error_message = "error", error_msg

                        # One frame on the GPU at a time (LIVE_STREAM drops overlapping submits);
                        # respond as soon as its callback lands. The producer thread keeps decoding
                        # the next frame into decode_queue meanwhile.
                        if mp_image is not None:
                            segmenter.segment_async(mp_image, frame.timestamp_ms)
                            try:
                                foreground = self.wait_for_result(results, frame.timestamp_ms, context)
                                if foreground is None:
                                    return
                                mask.mask_height, mask.mask_width = foreground.shape
                                mask.rle_counts.extend(encode_rle(foreground))
                                mask.status = "ok"
                            except (ValueError, RuntimeError) as exc:
                                mask.status, mask.error_message = "error", str(exc)

                        yield self._frame_response(req, frame, mask, started, foreground)
                        if req.is_eos:
                            return
                finally:
                    metrics.ACTIVE_SEGMENTERS.dec()
        finally:
            stop_event.set()
            # Drain decode_queue so the producer can unblock from put() and exit cleanly.
            while True:
                try:
                    decode_queue.get_nowait()
                except Empty:
                    break
            producer_thread.join(timeout=self.result_timeout)
            if self._segmenter_sem:
                self._segmenter_sem.release()
