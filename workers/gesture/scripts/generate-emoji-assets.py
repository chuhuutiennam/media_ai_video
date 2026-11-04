"""Generate original flat hand/heart icons as RGBA PNGs, using OpenCV primitives.

No fonts or downloaded artwork. Run with the worker's Python environment.
"""
import argparse
from pathlib import Path

import cv2
import numpy as np

DESTINATION = Path(__file__).absolute().parents[1] / "assets/emoji"
GOLD = (65, 195, 255, 255)  # BGRA
EDGE = (30, 100, 160, 255)


def polygon(image, points):
    contour = np.array(points, np.int32)
    cv2.fillPoly(image, [contour], GOLD, lineType=cv2.LINE_AA)
    cv2.polylines(image, [contour], True, EDGE, 5, cv2.LINE_AA)


def cartoon_face(image, wink=False, tongue=False, center=(135, 159)):
    x, y = center
    for dx in (-17, 17):
        if wink and dx > 0:
            cv2.ellipse(image, (x + dx, y), (9, 5), 0, 180, 360, (50, 40, 70, 255), 4, cv2.LINE_AA)
        else:
            cv2.ellipse(image, (x + dx, y), (10, 13), 0, 0, 360, (255, 255, 255, 255), -1, cv2.LINE_AA)
            cv2.ellipse(image, (x + dx + 2, y + 1), (5, 8), 0, 0, 360, (55, 35, 65, 255), -1, cv2.LINE_AA)
            cv2.circle(image, (x + dx, y - 3), 2, (255, 255, 255, 255), -1, cv2.LINE_AA)
    for dx in (-28, 28):
        cv2.ellipse(image, (x + dx, y + 14), (9, 5), 0, 0, 360, (145, 130, 255, 255), -1, cv2.LINE_AA)
    cv2.ellipse(image, (x, y + 16), (17, 13), 0, 0, 180, (60, 35, 75, 255), -1, cv2.LINE_AA)
    if tongue:
        cv2.ellipse(image, (x + 3, y + 27), (7, 6), 0, 0, 360, (160, 115, 255, 255), -1, cv2.LINE_AA)


def sticker(image):
    alpha = image[:, :, 3]
    rim = cv2.dilate(alpha, np.ones((9, 9), np.uint8))
    output = np.full_like(image, 255)
    output[:, :, 3] = rim
    mask = alpha.astype(np.float32)[:, :, None] / 255
    output[:, :, :3] = (image[:, :, :3] * mask + output[:, :, :3] * (1 - mask)).astype(np.uint8)
    return output


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=Path, default=DESTINATION)
    destination = parser.parse_args().output_dir
    destination.mkdir(parents=True, exist_ok=True)
    thumbs = [(73, 211), (73, 124), (108, 89), (120, 38), (139, 35), (150, 51), (145, 106),
              (199, 106), (216, 122), (199, 196), (180, 211)]
    shapes = {
        "thumb_up": thumbs,
        "thumb_down": [(x, 256 - y) for x, y in thumbs],
        "victory": [(91, 221), (70, 170), (67, 123), (78, 112), (94, 124), (75, 47),
                    (83, 31), (98, 32), (123, 113), (148, 32), (165, 30), (175, 45),
                    (153, 128), (179, 119), (195, 136), (187, 189), (166, 221)],
        "point_up": [(89, 221), (65, 174), (66, 136), (81, 125), (97, 141), (97, 40),
                     (108, 26), (125, 27), (135, 42), (135, 112), (181, 117), (197, 139),
                     (191, 188), (169, 221)],
        "fist": [(70, 215), (49, 158), (53, 99), (70, 82), (91, 91), (103, 77),
                 (123, 81), (137, 77), (158, 89), (179, 86), (199, 107), (197, 171), (174, 215)],
        "wave": [(88, 224), (49, 170), (28, 122), (33, 107), (48, 105), (75, 140),
                 (58, 68), (65, 52), (80, 52), (98, 119), (93, 40), (103, 28), (119, 33),
                 (128, 112), (138, 45), (152, 37), (166, 48), (160, 124), (180, 80),
                 (196, 76), (207, 90), (189, 166), (168, 220)],
    }
    for name, shape in shapes.items():
        image = np.zeros((256, 256, 4), np.uint8)
        polygon(image, shape)
        cartoon_face(image, wink=name in ("victory", "wave"), tongue=name == "thumb_down",
                     center=(140, 153) if name.startswith("thumb") else (130, 165))
        if name == "wave":
            cv2.ellipse(image, (122, 126), (116, 112), 0, 285, 345, (255, 155, 50, 255), 7, cv2.LINE_AA)
        if not cv2.imwrite(str(destination / f"{name}.png"), sticker(image)):
            raise RuntimeError("cannot write icon")
    image = np.zeros((256, 256, 4), np.uint8)
    heart = np.array([(128, 221), (35, 132), (24, 91), (38, 57), (67, 41), (98, 48),
                      (128, 74), (158, 48), (189, 41), (218, 57), (232, 91), (221, 132)], np.int32)
    cv2.fillPoly(image, [heart], (85, 65, 240, 255), lineType=cv2.LINE_AA)
    cv2.polylines(image, [heart], True, (55, 40, 185, 255), 5, cv2.LINE_AA)
    cartoon_face(image, center=(128, 125))
    if not cv2.imwrite(str(destination / "heart.png"), sticker(image)):
        raise RuntimeError("cannot write icon")


if __name__ == "__main__":
    main()
