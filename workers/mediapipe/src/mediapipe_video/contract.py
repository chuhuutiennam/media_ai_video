"""Compile video.proto at runtime and expose the generated video_pb2 module."""
import importlib
import sys
import tempfile
from pathlib import Path

from grpc_tools import protoc


def load_contract():
    """Prefer the gateway source; standalone workers use the distributed copy."""
    package_dir = Path(__file__).resolve().parent
    proto_file = package_dir / "video.proto"
    if package_dir.parent.name == "src":
        proto_file = package_dir.parent / "proto" / "video.proto"
        worker_root = package_dir.parent.parent
        canonical = worker_root.parent.parent / "internal" / "ai" / "proto" / "video.proto"
        if canonical.is_file():
            proto_file = canonical
    if not proto_file.is_file():
        proto_file = package_dir / "video.proto"
    proto_dir = proto_file.parent
    with tempfile.TemporaryDirectory(prefix="video-proto-") as generated:
        if protoc.main(["protoc", f"-I{proto_dir}", f"--python_out={generated}", str(proto_file)]):
            raise RuntimeError("cannot compile video.proto")
        sys.path.insert(0, generated)
        try:
            return importlib.import_module("video_pb2")
        finally:
            sys.path.remove(generated)


pb = load_contract()
