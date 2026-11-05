#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd -- "$SCRIPT_DIR/.." && pwd)

BUILD_DIR="${BUILD_DIR:-$ROOT/build/mf}"
BIN_DIR="${BIN_DIR:-$ROOT/bin}"

command -v cmake >/dev/null || {
    echo "cmake is required" >&2
    exit 1
}

mkdir -p "$BUILD_DIR" "$BIN_DIR"

echo "[INFO] Configure C++ MF"

cmake \
    -S "$ROOT/mf" \
    -B "$BUILD_DIR" \
    -DCMAKE_BUILD_TYPE=Release \
    -DCMAKE_RUNTIME_OUTPUT_DIRECTORY="$BIN_DIR"

echo "[INFO] Build C++ MF"

cmake --build "$BUILD_DIR" --parallel 2

echo "[INFO] Built: $BIN_DIR/mf_cpp"
