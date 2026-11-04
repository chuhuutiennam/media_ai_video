#!/usr/bin/env bash
# Generate Go bindings and sync the worker copy from the canonical video schema.
# Requires protoc + protoc-gen-go.
set -euo pipefail
ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"
protoc --go_out="$ROOT" --go_opt=module=media-ai-gateway \
  internal/ai/proto/video.proto
cp "$ROOT/internal/ai/proto/video.proto" \
  "$ROOT/workers/mediapipe/src/proto/video.proto"

cp "$ROOT/internal/ai/proto/video.proto" \
  "$ROOT/workers/gesture/src/proto/video.proto"
