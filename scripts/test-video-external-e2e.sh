#!/usr/bin/env bash
# Enabled video AI workers are external. This runner owns gateway/MF/senders only.
set -Eeuo pipefail
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
if [[ "${1:-}" == --help || "${1:-}" == -h ]]; then
  cat <<'HELP'
Usage: bash scripts/test-video-external-e2e.sh --ai-target SEGMENT_HOST:PORT [options]
       bash scripts/test-video-external-e2e.sh --gesture-only --gesture-target GESTURE_HOST:PORT [options]
Enabled workers must already serve the shared JPEG contract. This script manages gateway/MF/senders only.
One service uses one AI: default runs change_background (segmentation, one leg) against the
mediapipe repo service (default 127.0.0.1:50052); --gesture-only runs video_effects (gesture,
one leg) against the gesture repo service (default 127.0.0.1:50053).
GESTURE_DIR (default ../gesture) must contain assets/emoji for gesture runs.
  --gesture-only        Use only the gesture worker; --ai-target is unnecessary
  --input FILE          Video/image or PCAP/PCAPNG; defaults to testsrc2
  --duration SEC        Media duration per leg (default: 6)
  --gesture-render-mode MODE  realtime | arrival (measure delay) | source (export by source time)
  --require-gesture     With --gesture-only: require a rendered reaction (MF tAccess output)
  --bin-dir DIR         Use prebuilt gateway/MF/RTP tools
  --artifacts-dir DIR   Keep MP4s, logs, callbacks and metrics here
  --drain-timeout SEC   Allow lab PNG compositor to drain (default: 300)
Additional options: bash scripts/test-video-e2e.sh --help
HELP
  exit 0
fi
# Gesture worker chỉ dùng cho dịch vụ video_effects (--gesture-only).
GESTURE_MODE=off
for arg in "$@"; do
  if [[ "$arg" == --gesture-only ]]; then GESTURE_MODE=external; fi
done
exec bash "$SCRIPT_DIR/test-video-e2e.sh" --duration 6 --callback-timeout 15 --drain-timeout 300 "$@" \
  --ai-mode external --gesture-mode "$GESTURE_MODE" --output-video
