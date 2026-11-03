#!/usr/bin/env python3
"""Small offline MF for checking callback masks. Bash owns E2E and MP4 encoding."""

import argparse
import json
import math
import sys
from pathlib import Path

from PIL import Image


def require(condition, message):
    if not condition:
        raise ValueError(message)


def integer(value, maximum, name, minimum=0):
    require(type(value) is int and minimum <= value <= maximum, f"invalid {name}: {value}")
    return value


def decode_mask(result):
    width = integer(result["mask_width"], 4096, "mask_width", 1)
    height = integer(result["mask_height"], 4096, "mask_height", 1)
    counts = result["rle_counts"]
    require(result["status"] == "ok" and isinstance(counts, list) and counts, "invalid mask status/RLE")
    pixels = bytearray(width * height)
    offset = 0
    for run, count in enumerate(counts):
        integer(count, 0xFFFFFFFF, "rle_count")
        end = offset + count
        require(end <= len(pixels), "RLE exceeds mask dimensions")
        if run % 2:
            pixels[offset:end] = b"\xff" * count
        offset = end
    require(offset == len(pixels), "RLE does not fill mask dimensions")
    return Image.frombytes("L", (width, height), bytes(pixels))


def read_image(path):
    with Image.open(path, formats=("PNG", "JPEG")) as image:
        require(0 < image.width <= 16384 and 0 < image.height <= 16384
                and image.width * image.height <= 64 * 1024 * 1024, "oversized image")
        return image.convert("RGBA")


def resize_nearest(image, size):
    """Use floor(x * source_width / width), exactly as MF C++ does."""
    if image.size == size:
        return image
    width, height = size
    pixels = image.load()
    xs = [x * image.width // width for x in range(width)]
    output = Image.new(image.mode, size)
    output.putdata([pixels[x, y * image.height // height] for y in range(height) for x in xs])
    return output


def render(frames_dir, callbacks_path, leg="tCore", background=""):
    directory = Path(frames_dir)
    require(leg in ("tCore", "tAccess"), "leg must be tCore or tAccess")
    frames = json.loads((directory / "frames.json").read_text())
    require(isinstance(frames, list) and len(frames) >= 2, "need at least two source frames")
    size = (integer(frames[0]["width"], 16384, "width", 1),
            integer(frames[0]["height"], 16384, "height", 1))
    require(size[0] * size[1] <= 64 * 1024 * 1024, "oversized source frame")
    known = set()
    for frame in frames:
        timestamp = integer(frame["rtp_timestamp"], 0xFFFFFFFF, "rtp_timestamp")
        require(timestamp not in known, f"duplicate source RTP timestamp {timestamp}")
        require((frame["width"], frame["height"]) == size, "source resolution changes within stream")
        require(type(frame["time_sec"]) in (int, float) and math.isfinite(frame["time_sec"])
                and frame["time_sec"] >= 0, "invalid source time")
        known.add(timestamp)

    callbacks = json.loads(Path(callbacks_path).read_text())["callbacks"]
    require(isinstance(callbacks, list), "callbacks must be an array")
    masks = {}
    for callback in callbacks:
        if callback["path"] != f"/results/{leg}":
            continue
        # Compare the same fields as C++; retry timing/extra metadata are ignored.
        mask = {key: callback["video_mask"][key] for key in
                ("frame_id", "rtp_timestamp", "mask_width", "mask_height", "rle_counts", "status")}
        integer(mask["frame_id"], 0x7FFFFFFFFFFFFFFF, "frame_id", 1)
        timestamp = integer(mask["rtp_timestamp"], 0xFFFFFFFF, "rtp_timestamp")
        require(timestamp in known, f"callback has no source frame with RTP timestamp {timestamp}")
        require(timestamp not in masks or masks[timestamp] == mask, f"conflicting masks for {timestamp}")
        masks[timestamp] = mask
    require(masks, f"no callback masks for {leg}")

    backdrop = resize_nearest(read_image(background), size) if background else Image.new("RGBA", size, "black")
    for folder in ("output", "masks"):
        (directory / folder).mkdir(parents=True, exist_ok=True)
    current, mask_id, mapping = None, 0, []
    with (directory / "output.ffconcat").open("w") as concat:
        concat.write("ffconcat version 1.0\n")
        for i, frame in enumerate(frames):
            exact = frame["rtp_timestamp"] in masks
            if exact:
                mask = masks[frame["rtp_timestamp"]]
                mask_id = mask["frame_id"]
                decoded = decode_mask(mask)
                decoded.save(directory / "masks" / f"{mask_id:06d}.png")
                current = resize_nearest(decoded, size)
            source = read_image(directory / frame["file"])
            require(source.size == size, f"source dimensions disagree with frames.json: {frame['file']}")
            output = Image.composite(source, backdrop, current) if current is not None else backdrop
            name = f"output/{i + 1:06d}.png"
            output.save(directory / name)
            duration = (frames[i + 1]["time_sec"] - frame["time_sec"] if i + 1 < len(frames)
                        else frame["time_sec"] - frames[i - 1]["time_sec"])
            require(0 < duration <= 60, f"invalid frame interval {duration}")
            concat.write(f"file '{name}'\nduration {duration:.9f}\n")
            mapping.append(dict(file=name, rtp_timestamp=frame["rtp_timestamp"], time_sec=frame["time_sec"],
                                mask_frame_id=mask_id, exact_mask=exact))
        concat.write(f"file 'output/{len(frames):06d}.png'\n")
    (directory / "render.json").write_text(json.dumps(mapping, indent=2) + "\n")
    return dict(event="rendered", leg=leg, frames=len(frames), masks=len(masks), background=str(background))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--frames-dir", required=True)
    parser.add_argument("--callbacks", dest="callbacks_path", required=True)
    parser.add_argument("--leg", choices=("tCore", "tAccess"), default="tCore")
    parser.add_argument("--background", default="", help="replacement JPEG/PNG; default black")
    args = parser.parse_args()
    try:
        print(json.dumps(render(**vars(args))))
    except (OSError, ValueError, KeyError, TypeError) as error:
        print(f"mf mock: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
