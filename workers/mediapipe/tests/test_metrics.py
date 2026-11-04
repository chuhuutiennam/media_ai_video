import math

import pytest
from prometheus_client import Histogram

from mediapipe_video import metrics


def test_histogram_snapshot_matches_exported_cumulative_buckets():
    hist = Histogram("test_latency_seconds", "test", buckets=[0.1, 0.5, 1], registry=None)
    assert math.isnan(metrics._hist_avg(hist))
    for value in (0.05, 0.3, 0.7, 1.2):
        hist.observe(value)
    exported = next(iter(hist.collect())).samples
    buckets = [sample.value for sample in exported if sample.name.endswith('_bucket')]
    assert metrics._hist_buckets(hist) == buckets == [1, 2, 3, 4]
    assert metrics._hist_avg(hist) == pytest.approx(2.25 / 4)
    assert metrics._hist_quantile(hist._upper_bounds, buckets, 0.5) == pytest.approx(0.5)
    assert 'latency_avg_ms' in metrics.snapshot()
