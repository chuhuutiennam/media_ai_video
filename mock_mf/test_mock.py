import json
import struct
import tempfile
import unittest
import zlib
from pathlib import Path

from PIL import Image

from mock_mf import mock


class MockMFTest(unittest.TestCase):
    def test_rle_and_floor_scaling_preserve_foreground(self):
        mask = dict(mask_width=2, mask_height=1, status="ok", rle_counts=[0, 1, 1])
        decoded = mock.decode_mask(mask)
        self.assertEqual(decoded.tobytes(), b"\xff\x00")
        # 2 -> 3 must map [0, 0, 1], matching C++ rather than pixel-center resize.
        scaled = mock.resize_nearest(decoded, (3, 1))
        self.assertEqual(scaled.tobytes(), b"\xff\xff\x00")
        source = Image.new("RGBA", (3, 1), "red")
        for color in ("black", "blue"):
            background = Image.new("RGBA", (3, 1), color)
            output = Image.composite(source, background, scaled)
            self.assertEqual(output.getpixel((0, 0)), source.getpixel((0, 0)))
            self.assertEqual(output.getpixel((1, 0)), source.getpixel((1, 0)))
            self.assertEqual(output.getpixel((2, 0)), background.getpixel((2, 0)))
        for counts in ([], [1], [3], [-1, 3], [False, 2], [0, 2.0], [0, 2**32]):
            with self.subTest(counts=counts), self.assertRaises(ValueError):
                mock.decode_mask(dict(mask, rle_counts=counts))

    def test_matching_wrap_retries_and_no_future_mask(self):
        with tempfile.TemporaryDirectory() as folder:
            directory = Path(folder)
            Image.new("RGBA", (2, 2), "white").save(directory / "source.png")
            timestamps = [0xFFFFFF00, 8744, 17744, 26744]
            frames = [dict(file="source.png", rtp_timestamp=ts, time_sec=i / 10, width=2, height=2)
                      for i, ts in enumerate(timestamps)]
            (directory / "frames.json").write_text(json.dumps(frames))

            def callback(leg, timestamp, frame_id):
                return dict(path=f"/results/{leg}", video_mask=dict(frame_id=frame_id, rtp_timestamp=timestamp,
                            mask_width=2, mask_height=2, rle_counts=[0, 4], status="ok"))

            large_id = 2**53 + 1
            path = directory / "callbacks.json"
            callbacks = [callback("tCore", timestamps[2], large_id), callback("tAccess", timestamps[0], 99),
                         callback("tCore", timestamps[1], 7), callback("tCore", timestamps[1], 7)]
            path.write_text(json.dumps(dict(callbacks=callbacks)))
            summary = mock.render(directory, path)
            self.assertEqual((summary["frames"], summary["masks"]), (4, 2))
            mapping = json.loads((directory / "render.json").read_text())
            self.assertEqual([m["mask_frame_id"] for m in mapping], [0, 7, large_id, large_id])
            self.assertEqual([m["exact_mask"] for m in mapping], [False, True, True, False])
            self.assertEqual(mock.read_image(directory / "output/000001.png").getpixel((0, 0)), (0, 0, 0, 255))
            self.assertEqual(mock.read_image(directory / "output/000002.png").getpixel((0, 0)), (255, 255, 255, 255))
            for bad in ([callback("tCore", 123, 1)], [callbacks[2], callback("tCore", timestamps[1], 8)]):
                path.write_text(json.dumps(dict(callbacks=bad)))
                with self.assertRaises(ValueError):
                    mock.render(directory, path)

    def test_sender_rgb16_has_no_gamma_or_rounding_change(self):
        def chunk(kind, data):
            return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data))

        png = (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", 1, 1, 16, 2, 0, 0, 0))
               + chunk(b"IDAT", zlib.compress(b"\x00\x80\xff\x12\xff\xff\x00")) + chunk(b"IEND", b""))
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "source.png"
            path.write_bytes(png)
            self.assertEqual(mock.read_image(path).getpixel((0, 0)), (128, 18, 255, 255))


if __name__ == "__main__":
    unittest.main()
