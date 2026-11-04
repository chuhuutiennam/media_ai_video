"""Optional local-demo bridge. The gesture core does not import segmentation."""
from queue import Queue


class SegmentationAdapter:
    def __init__(self, model, result_timeout_secs=10):
        # Import only when the caller explicitly requests a background effect.
        try:
            from mediapipe_video.config import MediaPipeConfig
            from mediapipe_video.segmenter import Worker
        except ImportError as exc:
            raise RuntimeError(
                "Background demo requires the optional workers/mediapipe package. "
                "Install both local packages or add workers/mediapipe/src to PYTHONPATH."
            ) from exc
        self.worker = Worker(MediaPipeConfig(model=model, delegate="cpu", result_timeout_secs=result_timeout_secs))
        self.timeout = result_timeout_secs
        self.results = Queue(maxsize=1)
        self.segmenter = None

    def __enter__(self):
        self.segmenter = self.worker.create_segmenter(self._callback)
        return self

    def __exit__(self, *args):
        self.segmenter.close()

    def _callback(self, result, image, timestamp_ms):
        try:
            payload = (timestamp_ms, self.worker.foreground_mask(result, self.worker.seg_config.default_threshold), None)
        except Exception as exc:
            payload = (timestamp_ms, None, exc)
        self.results.put_nowait(payload)

    def mask(self, image, timestamp_ms):
        self.segmenter.segment_async(image, timestamp_ms)
        result_ts, foreground, error = self.results.get(timeout=self.timeout)
        if error is not None:
            raise error
        if result_ts != timestamp_ms:
            raise RuntimeError("segmentation callback timestamp mismatch")
        return foreground
