"""gRPC server bootstrap and CLI entry point."""
import argparse
import logging
import math
import os
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

import grpc
import mediapipe as mp

from mediapipe_video.config import AppConfig, load_config
from mediapipe_video.contract import pb
from mediapipe_video.log import setup_logging
from mediapipe_video import metrics as _metrics
from mediapipe_video.segmenter import Worker

logger = logging.getLogger(__name__)


class _VideoStreamHandler(grpc.GenericRpcHandler):
    def __init__(self, worker, pb):
        self._handlers = {
            "/video.VideoStream/Segment": grpc.stream_stream_rpc_method_handler(
                worker.Segment,
                request_deserializer=pb.VideoRequest.FromString,
                response_serializer=pb.VideoResponse.SerializeToString,
            ),
        }

    def service_name(self):
        return "video.VideoStream"

    def service(self, handler_call_details):
        return self._handlers.get(handler_call_details.method)


def main():
    """MediaPipe SelfieSegmenter gRPC worker for the gateway's video.proto contract."""
    parser = argparse.ArgumentParser(description=main.__doc__)
    parser.add_argument("--config", type=Path, help="YAML config file (overridden by explicit CLI flags)")
    parser.add_argument("--addr", help="gRPC bind address")
    parser.add_argument("--model", type=Path, help="path to .tflite model file")
    parser.add_argument("--foreground-index", type=int, help="confidence_masks plane index (default: auto-detect)")
    parser.add_argument("--gpu-device-index", type=int, help="GPU device index; sets CUDA_VISIBLE_DEVICES (default: system default)")
    parser.add_argument("--max-streams", type=int, help="max concurrent gRPC streams")
    parser.add_argument("--delegate", choices=("gpu", "cpu", "auto"), help="compute backend (default: auto)")
    parser.add_argument("--result-timeout", type=float, help="seconds to wait for each LIVE_STREAM callback")
    parser.add_argument("--log-level", choices=("DEBUG", "INFO", "WARNING", "ERROR"), help="log verbosity")
    args = parser.parse_args()

    _DEFAULT_CONFIG = Path("etc/config.yaml")
    config_path = args.config or (_DEFAULT_CONFIG if _DEFAULT_CONFIG.is_file() else None)
    cfg = load_config(config_path) if config_path else AppConfig()

    # CLI flags override config file values when explicitly provided.
    if args.addr:
        cfg.server.addr = args.addr
    if args.model:
        cfg.mediapipe.model = args.model
    if args.foreground_index is not None:
        cfg.mediapipe.foreground_index = args.foreground_index
    if args.gpu_device_index is not None:
        cfg.mediapipe.gpu_device_index = args.gpu_device_index
    if args.max_streams is not None:
        cfg.server.max_streams = args.max_streams
    if args.delegate:
        cfg.mediapipe.delegate = args.delegate
    if args.result_timeout is not None:
        cfg.mediapipe.result_timeout_secs = args.result_timeout
    if args.log_level:
        cfg.logging.level = args.log_level

    setup_logging(cfg.logging)

    if cfg.metrics.enabled:
        _metrics.start_metrics_server(cfg.metrics.port)
    _metrics.start_periodic_logger(cfg.metrics.log_interval_secs)

    if not cfg.mediapipe.model:
        parser.error("model path is required (set --model or config.mediapipe.model in YAML)")
    if not cfg.mediapipe.model.is_file():
        parser.error(f"model file not found: {cfg.mediapipe.model}")
    if cfg.server.max_streams <= 0:
        parser.error("max_streams must be positive")
    if not math.isfinite(cfg.mediapipe.result_timeout_secs) or cfg.mediapipe.result_timeout_secs <= 0:
        parser.error("result_timeout_secs must be finite and positive")

    if cfg.mediapipe.gpu_device_index is not None:
        os.environ["CUDA_VISIBLE_DEVICES"] = str(cfg.mediapipe.gpu_device_index)
        logger.info("gpu_device_index=%d  CUDA_VISIBLE_DEVICES=%d", cfg.mediapipe.gpu_device_index, cfg.mediapipe.gpu_device_index)

    if cfg.mediapipe.delegate == "auto":
        cfg.mediapipe.delegate = "gpu"
        try:
            worker = Worker(cfg.mediapipe, cfg.segmentation)
            logger.info("auto-detect: GPU available → delegate=gpu")
        except (ValueError, RuntimeError) as exc:
            logger.warning("auto-detect: GPU unavailable (%s) → delegate=cpu", exc)
            cfg.mediapipe.delegate = "cpu"
            try:
                worker = Worker(cfg.mediapipe, cfg.segmentation)
            except (ValueError, RuntimeError) as exc2:
                parser.exit(1, f"Cannot initialize MediaPipe LIVE_STREAM (cpu): {exc2}\n")
    else:
        try:
            worker = Worker(cfg.mediapipe, cfg.segmentation)
        except (ValueError, RuntimeError) as exc:
            hint = (
                " GPU requires a MediaPipe build with GPU support and a physical GPU exposed through EGL/OpenGL ES."
                if cfg.mediapipe.delegate == "gpu" else ""
            )
            parser.exit(1, f"Cannot initialize MediaPipe LIVE_STREAM ({cfg.mediapipe.delegate}): {exc}.{hint}\n")

    if cfg.mediapipe.delegate == "gpu" and cfg.segmentation.max_concurrent_segmenters == 0:
        logger.warning(
            "delegate=gpu with max_concurrent_segmenters=0 (no limit) — "
            "consider setting segmentation.max_concurrent_segmenters=4 to 6 in config.yaml "
            "to avoid EGL context exhaustion under high stream concurrency"
        )

    server = grpc.server(
        ThreadPoolExecutor(max_workers=cfg.server.max_streams),
        maximum_concurrent_rpcs=cfg.server.max_streams,
        options=(("grpc.max_receive_message_length", cfg.server.max_receive_message_bytes),),
    )
    server.add_generic_rpc_handlers((_VideoStreamHandler(worker, pb),))
    if not server.add_insecure_port(cfg.server.addr):
        raise RuntimeError(f"cannot bind {cfg.server.addr}")

    server.start()
    logger.info(
        "server ready  addr=%s  model=%s  mediapipe=%s  delegate=%s",
        cfg.server.addr, cfg.mediapipe.model, mp.__version__, cfg.mediapipe.delegate,
    )

    try:
        server.wait_for_termination()
    except KeyboardInterrupt:
        logger.info("shutting down  grace=%.1fs", cfg.server.shutdown_grace_secs)
        server.stop(cfg.server.shutdown_grace_secs).wait()
