"""Binary mask utilities: confidence threshold and RLE encoding."""
import numpy as np


def threshold_mask(probabilities, threshold):
    """Convert a MediaPipe confidence float array (HxW or HxWx1) to a boolean mask."""
    plane = np.asarray(probabilities)
    if plane.ndim == 3 and plane.shape[2] == 1:
        plane = plane[:, :, 0]
    if plane.ndim != 2:
        raise ValueError(f"unexpected confidence mask shape: {plane.shape}")
    return plane > threshold


def encode_rle(mask):
    """Row-major binary RLE, starting with background; zero first run allowed."""
    flat = np.asarray(mask, dtype=np.bool_).ravel(order="C")
    if not flat.size:
        raise ValueError("empty mask")
    boundaries = np.flatnonzero(flat[1:] != flat[:-1]) + 1
    counts = np.diff(np.concatenate(([0], boundaries, [flat.size]))).tolist()
    return [0] + counts if flat[0] else counts
