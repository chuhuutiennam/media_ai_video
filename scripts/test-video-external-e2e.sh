#!/usr/bin/env bash
# Enabled video AI workers are external. This runner owns gateway/MF/senders only.
set -Eeuo pipefail
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
if [[ "${1:-}" == --help || "${1:-}" == -h ]]; then
  cat <<'HELP'
Usage: bash scripts/test-video-external-e2e.sh --ai-target SEGMENT_HOST:PORT --gesture-target GESTURE_HOST:PORT [options]
Enabled workers must already serve the shared JPEG contract. This script manages gateway/MF/senders only.
  --gesture-only        Use only the gesture worker; --ai-target is unnecessary
  --input FILE          Video/image or PCAP/PCAPNG; defaults to testsrc2
  --duration SEC        Media duration per leg (default: 6)
  --gesture-render-mode MODE  realtime | arrival (measure delay) | source (export by source time)
  --require-gesture     Require a reaction rendered in each leg (use suitable gesture video)
  --bin-dir DIR         Use prebuilt gateway/MF/RTP tools
  --artifacts-dir DIR   Keep MP4s, logs, callbacks and metrics here
  --drain-timeout SEC   Allow lab PNG compositor to drain (default: 300)
Additional options: bash scripts/test-video-e2e.sh --help
HELP
  exit 0
fi
exec bash "$SCRIPT_DIR/test-video-e2e.sh" --duration 6 --callback-timeout 15 --drain-timeout 300 "$@" \
  --ai-mode external --gesture-mode external --output-video
