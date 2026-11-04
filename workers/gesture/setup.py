"""Include sibling assets/ in wheels while keeping the user's flat src/ layout."""
import shutil
from pathlib import Path

from setuptools import setup
from setuptools.command.build_py import build_py
from setuptools.command.sdist import sdist


def contract_source():
    root = Path(__file__).resolve().parent
    canonical = root.parent.parent / "internal/ai/proto/video.proto"
    return canonical if canonical.is_file() else root / "src/proto/video.proto"


class BuildWithAssets(build_py):
    def run(self):
        super().run()
        shutil.copytree(Path(__file__).parent / "assets", Path(self.build_lib) / "mediapipe_gesture/assets", dirs_exist_ok=True)
        self.copy_file(str(contract_source()), str(Path(self.build_lib) / "mediapipe_gesture/video.proto"))


class SDist(sdist):
    def make_release_tree(self, base_dir, files):
        super().make_release_tree(base_dir, files)
        target = Path(base_dir) / "src/proto/video.proto"
        target.parent.mkdir(parents=True, exist_ok=True)
        target.unlink(missing_ok=True)
        self.copy_file(str(contract_source()), str(target))


setup(cmdclass={"build_py": BuildWithAssets, "sdist": SDist})
