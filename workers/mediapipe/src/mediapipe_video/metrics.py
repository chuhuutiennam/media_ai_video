"""Prometheus metrics and periodic log snapshot for mediapipe-video."""
import logging
import math
import threading
import time
from itertools import accumulate
from typing import Any

from prometheus_client import Counter, Gauge, Histogram, start_http_server

logger = logging.getLogger(__name__)

# ── stream ────────────────────────────────────────────────────────────────────

ACTIVE_STREAMS = Gauge(
    "mediapipe_video_active_streams",
    "Number of currently active gRPC streams",
)

STREAMS_TOTAL = Counter(
    "mediapipe_video_streams_total",
    "Total gRPC streams completed",
)

# ── segmenters ────────────────────────────────────────────────────────────────

ACTIVE_SEGMENTERS = Gauge(
    "mediapipe_video_active_segmenters",
    "Number of currently active ImageSegmenter instances",
)

# ── frames ────────────────────────────────────────────────────────────────────

FRAMES_TOTAL = Counter(
    "mediapipe_video_frames_total",
    "Total frames processed",
    ["status"],  # "ok" | "error"
)

FRAME_DROPS_TOTAL = Counter(
    "mediapipe_video_frame_drops_total",
    "Frames dropped by MediaPipe LIVE_STREAM (stale callback discarded)",
)

# ── stream errors ─────────────────────────────────────────────────────────────

SEGMENTER_EXHAUSTED_TOTAL = Counter(
    "mediapipe_video_segmenter_exhausted_total",
    "Streams rejected due to max_concurrent_segmenters limit",
)

STREAM_ERRORS_TOTAL = Counter(
    "mediapipe_video_stream_errors_total",
    "gRPC streams ended with context.abort",
    ["reason"],  # "resource_exhausted" | "invalid_argument" | "deadline_exceeded" | "internal"
)

# ── latency ───────────────────────────────────────────────────────────────────

INFERENCE_LATENCY = Histogram(
    "mediapipe_video_inference_latency_seconds",
    "Per-frame end-to-end processing latency (JPEG decode + inference + RLE encode)",
    buckets=[0.005, 0.010, 0.025, 0.050, 0.100, 0.250, 0.500, 1.0, 2.5, 5.0],
)

# ── frame characteristics ─────────────────────────────────────────────────────

FRAME_BYTES = Histogram(
    "mediapipe_video_frame_bytes",
    "JPEG input frame size in bytes",
    buckets=[4_096, 16_384, 65_536, 131_072, 262_144, 524_288, 1_048_576],
)

FOREGROUND_RATIO = Histogram(
    "mediapipe_video_foreground_ratio",
    "Fraction of pixels classified as foreground [0, 1]",
    buckets=[0.05, 0.10, 0.20, 0.30, 0.40, 0.50, 0.60, 0.70, 0.80, 0.90, 0.95],
)

# ── helpers ───────────────────────────────────────────────────────────────────

def _cval(metric) -> float:
    """Read current value of an unlabeled Counter or Gauge."""
    return metric._value.get()


def _clabel(counter, **labels) -> float:
    """Read current value of a labeled Counter (returns 0 if not yet observed)."""
    return counter.labels(**labels)._value.get()


def _hist_buckets(hist) -> list[float]:
    """Return cumulative bucket counts as a plain list (includes +Inf bucket)."""
    # prometheus_client stores disjoint buckets internally; exported buckets
    # are cumulative. Histograms have no private _count attribute.
    return list(accumulate(b.get() for b in hist._buckets))


def _hist_quantile(upper_bounds: list[float], cum_counts: list[float], q: float) -> float:
    """
    Compute approximate quantile q ∈ [0, 1] from cumulative histogram bucket data.
    Uses linear interpolation within the bucket that contains the quantile.
    Returns NaN when there are no observations.
    """
    total = cum_counts[-1]  # last bucket (≤ +Inf) == total count
    if total == 0:
        return math.nan
    target = q * total
    for i, (bound, cum) in enumerate(zip(upper_bounds, cum_counts)):
        if cum >= target:
            prev_cum = cum_counts[i - 1] if i > 0 else 0.0
            lo = upper_bounds[i - 1] if i > 0 else 0.0
            n = cum - prev_cum
            if n == 0 or math.isinf(bound):
                return lo
            return lo + (bound - lo) * (target - prev_cum) / n
    # All observations fell into the last finite bucket.
    finite = [b for b in upper_bounds if not math.isinf(b)]
    return finite[-1] if finite else 0.0


def _hist_avg(hist) -> float:
    """Average value of all observations; NaN if none."""
    count = _hist_buckets(hist)[-1]
    return hist._sum.get() / count if count else math.nan


# ── snapshot ──────────────────────────────────────────────────────────────────

def snapshot() -> dict[str, Any]:
    """
    Return a point-in-time snapshot of all metric values.
    Histogram stats (avg, percentiles) are computed over the full lifetime of the process.
    For interval-based percentiles use start_periodic_logger — it tracks bucket deltas.
    """
    bounds = INFERENCE_LATENCY._upper_bounds
    cum = _hist_buckets(INFERENCE_LATENCY)
    p50 = _hist_quantile(bounds, cum, 0.50)
    p95 = _hist_quantile(bounds, cum, 0.95)
    p99 = _hist_quantile(bounds, cum, 0.99)
    return {
        "active_streams":      int(_cval(ACTIVE_STREAMS)),
        "active_segmenters":   int(_cval(ACTIVE_SEGMENTERS)),
        "streams_total":       _cval(STREAMS_TOTAL),
        "frames_ok_total":     _clabel(FRAMES_TOTAL, status="ok"),
        "frames_error_total":  _clabel(FRAMES_TOTAL, status="error"),
        "frame_drops_total":   _cval(FRAME_DROPS_TOTAL),
        "segmenter_exhausted_total":        _cval(SEGMENTER_EXHAUSTED_TOTAL),
        "stream_errors_resource_exhausted": _clabel(STREAM_ERRORS_TOTAL, reason="resource_exhausted"),
        "stream_errors_invalid_argument":   _clabel(STREAM_ERRORS_TOTAL, reason="invalid_argument"),
        "stream_errors_deadline_exceeded":  _clabel(STREAM_ERRORS_TOTAL, reason="deadline_exceeded"),
        "stream_errors_internal":           _clabel(STREAM_ERRORS_TOTAL, reason="internal"),
        "latency_p50_ms":      p50 * 1000,
        "latency_p95_ms":      p95 * 1000,
        "latency_p99_ms":      p99 * 1000,
        "latency_avg_ms":      _hist_avg(INFERENCE_LATENCY) * 1000,
        "frame_bytes_avg":     _hist_avg(FRAME_BYTES),
        "foreground_ratio_avg": _hist_avg(FOREGROUND_RATIO),
    }


# ── periodic logger ───────────────────────────────────────────────────────────

def _fmt(v: float, decimals: int = 1) -> str:
    """Format float; return 'n/a' for NaN."""
    return "n/a" if math.isnan(v) else f"{v:.{decimals}f}"


def start_periodic_logger(interval_secs: float) -> None:
    """
    Start a background daemon thread that logs a metric snapshot every interval_secs.

    Counters are reported as cumulative totals and as deltas since the last snapshot
    (+NNN) so operators can quickly see activity in the current window.

    Histogram latency percentiles are computed from bucket deltas (interval window),
    giving an accurate view of latency *during* the interval rather than since startup.
    """
    if interval_secs <= 0:
        return

    def _loop() -> None:
        # Previous snapshot state for delta computation.
        prev_counters: dict[str, float] = {}
        prev_lat_buckets: list[float] = []
        prev_lat_sum: float = 0.0
        prev_lat_count: float = 0.0

        bounds = INFERENCE_LATENCY._upper_bounds

        while True:
            time.sleep(interval_secs)
            try:
                cur = snapshot()

                # ── counter deltas ────────────────────────────────────────────
                counter_keys = (
                    "streams_total", "frames_ok_total", "frames_error_total", "frame_drops_total",
                    "segmenter_exhausted_total",
                    "stream_errors_resource_exhausted", "stream_errors_invalid_argument",
                    "stream_errors_deadline_exceeded", "stream_errors_internal",
                )
                deltas = {
                    k: cur[k] - prev_counters.get(k, 0.0)
                    for k in counter_keys
                }

                # ── interval-based latency percentiles ────────────────────────
                cur_lat_buckets = _hist_buckets(INFERENCE_LATENCY)
                cur_lat_sum   = INFERENCE_LATENCY._sum.get()
                cur_lat_count = cur_lat_buckets[-1]

                if prev_lat_buckets:
                    delta_buckets = [c - p for c, p in zip(cur_lat_buckets, prev_lat_buckets)]
                    delta_count = cur_lat_count - prev_lat_count
                    delta_sum   = cur_lat_sum   - prev_lat_sum
                    ip50 = _hist_quantile(bounds, delta_buckets, 0.50) * 1000
                    ip95 = _hist_quantile(bounds, delta_buckets, 0.95) * 1000
                    ip99 = _hist_quantile(bounds, delta_buckets, 0.99) * 1000
                    iavg = (delta_sum / delta_count * 1000) if delta_count else math.nan
                else:
                    # First tick — fall back to cumulative stats.
                    ip50, ip95, ip99, iavg = (
                        cur["latency_p50_ms"], cur["latency_p95_ms"],
                        cur["latency_p99_ms"], cur["latency_avg_ms"],
                    )

                # ── error rate ────────────────────────────────────────────────
                total_frames = deltas["frames_ok_total"] + deltas["frames_error_total"]
                err_rate_pct = (
                    deltas["frames_error_total"] / total_frames * 100
                    if total_frames > 0 else math.nan
                )
                drop_rate_pct = (
                    deltas["frame_drops_total"] / (total_frames + deltas["frame_drops_total"]) * 100
                    if (total_frames + deltas["frame_drops_total"]) > 0 else math.nan
                )

                # ── stream error totals ───────────────────────────────────────
                _err_keys = (
                    "stream_errors_resource_exhausted", "stream_errors_invalid_argument",
                    "stream_errors_deadline_exceeded", "stream_errors_internal",
                )
                stream_errors_cur   = sum(cur[k]    for k in _err_keys)
                stream_errors_delta = sum(deltas[k] for k in _err_keys)

                logger.info(
                    "metrics_snapshot"
                    "  interval=%.0fs"
                    "  active_streams=%d  active_segmenters=%d"
                    "  streams=%.0f(+%.0f)"
                    "  frames_ok=%.0f(+%.0f)  frames_error=%.0f(+%.0f)  frame_drops=%.0f(+%.0f)"
                    "  err_rate=%s%%  drop_rate=%s%%"
                    "  stream_errors=%.0f(+%.0f)  segmenter_exhausted=%.0f(+%.0f)"
                    "  latency_p50=%sms  latency_p95=%sms  latency_p99=%sms  latency_avg=%sms"
                    "  frame_bytes_avg=%sB  foreground_ratio_avg=%s",
                    interval_secs,
                    cur["active_streams"], cur["active_segmenters"],
                    cur["streams_total"],       deltas["streams_total"],
                    cur["frames_ok_total"],     deltas["frames_ok_total"],
                    cur["frames_error_total"],  deltas["frames_error_total"],
                    cur["frame_drops_total"],   deltas["frame_drops_total"],
                    _fmt(err_rate_pct), _fmt(drop_rate_pct),
                    stream_errors_cur,   stream_errors_delta,
                    cur["segmenter_exhausted_total"], deltas["segmenter_exhausted_total"],
                    _fmt(ip50), _fmt(ip95), _fmt(ip99), _fmt(iavg),
                    _fmt(cur["frame_bytes_avg"], 0),
                    _fmt(cur["foreground_ratio_avg"], 3),
                )

                # ── save state for next tick ──────────────────────────────────
                prev_counters    = {k: cur[k] for k in counter_keys}
                prev_lat_buckets = cur_lat_buckets
                prev_lat_sum     = cur_lat_sum
                prev_lat_count   = cur_lat_count

            except Exception:
                logger.exception("metrics periodic snapshot failed")

    t = threading.Thread(target=_loop, daemon=True, name="metrics-logger")
    t.start()
    logger.info("metrics periodic logger started  interval=%.0fs", interval_secs)


# ── HTTP server ───────────────────────────────────────────────────────────────

def start_metrics_server(port: int) -> None:
    start_http_server(port)
    logger.info("metrics server ready  port=%d  path=/metrics", port)
