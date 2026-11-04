"""Bundle the gateway contract, or its snapshot in a standalone worker source."""
from pathlib import Path

from setuptools import setup
from setuptools.command.build_py import build_py
from setuptools.command.sdist import sdist


def contract_source():
    worker_root = Path(__file__).resolve().parent
    canonical = worker_root.parent.parent / "internal" / "ai" / "proto" / "video.proto"
    if canonical.is_file():
        return canonical
    return worker_root / "src" / "proto" / "video.proto"


class BuildPy(build_py):
    def run(self):
        super().run()
        target = Path(self.build_lib) / "mediapipe_video" / "video.proto"
        self.copy_file(str(contract_source()), str(target))


class SDist(sdist):
    def make_release_tree(self, base_dir, files):
        super().make_release_tree(base_dir, files)
        target = Path(base_dir) / "src" / "proto" / "video.proto"
        target.parent.mkdir(parents=True, exist_ok=True)
        # sdist may hard-link source files: replace the release copy before writing.
        target.unlink(missing_ok=True)
        self.copy_file(str(contract_source()), str(target))


setup(cmdclass={"build_py": BuildPy, "sdist": SDist})
