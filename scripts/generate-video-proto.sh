#!/usr/bin/env bash
# Generate Go bindings from the canonical video schema (internal/ai/proto/video.proto) and
# copy that schema into the separate AI service repos when they are present:
#   MEDIAPIPE_DIR (default ../mediapipe): change_background, loaded from src/mediapipe_video/video.proto
#   GESTURE_DIR   (default ../gesture):   video_effects, loaded from src/proto/video.proto
# Both repos compile messages only (--python_out) and register /video.VideoStream/Segment by hand,
# so the commented rpc line in the canonical schema does not affect them.
# Requires protoc + protoc-gen-go.
set -euo pipefail
ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
MEDIAPIPE_DIR=${MEDIAPIPE_DIR:-$ROOT/../mediapipe}
GESTURE_DIR=${GESTURE_DIR:-$ROOT/../gesture}
SCHEMA="$ROOT/internal/ai/proto/video.proto"
cd "$ROOT"
protoc --go_out="$ROOT" --go_opt=module=media-ai-gateway \
  internal/ai/proto/video.proto

sync_copy() {  # sync_copy <repo dir> <relative destination>...
  local repo=$1; shift
  if [[ ! -d "$repo" ]]; then
    echo "skip: $repo not found (set the repo path variable to sync its copy)"
    return 0
  fi
  local dest
  for dest in "$@"; do
    mkdir -p "$(dirname -- "$repo/$dest")"
    cp "$SCHEMA" "$repo/$dest"
    echo "synced: $repo/$dest"
  done
}
sync_copy "$MEDIAPIPE_DIR" src/mediapipe_video/video.proto src/proto/video.proto
sync_copy "$GESTURE_DIR" src/proto/video.proto
