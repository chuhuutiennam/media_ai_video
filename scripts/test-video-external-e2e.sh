#!/usr/bin/env bash
# Enabled video AI workers are external. This runner owns gateway/MF/senders only.
set -Eeuo pipefail
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
if [[ "${1:-}" == --help || "${1:-}" == -h ]]; then
  cat <<'HELP'
Usage: bash scripts/test-video-external-e2e.sh [options]
       bash scripts/test-video-external-e2e.sh --gesture-only [options]
Enabled workers must already serve the shared JPEG contract. This script manages gateway/MF/senders only.
One service uses one AI: default runs change_background (segmentation, one leg) against the
mediapipe repo service; --gesture-only runs video_effects (gesture, one leg).
Processing defaults (FPS, callback mode/timeouts, retries, queues) are read from
config/gateway-video-ims.yaml. Lab gateway/MF/RTP use loopback and HTTP without TLS.
AI workers default to 127.0.0.1:50052 (segmentation) and 127.0.0.1:50053 (gesture).
The IMS source file is preserved. Explicit CLI options override processing defaults/AI targets.
GESTURE_DIR (default ../gesture) must contain assets/emoji for legacy sticker runs.
  --gesture-only        Use only the gesture worker; --ai-target is unnecessary
  --gesture-callback-mode MODE  Override profile: legacy (stickers) | keyword (MF receipt)
  --gesture-fps N        Override profile gesture sampling FPS; CPU troubleshooting: 10
  --gateway-config FILE Use another defaults file (default: config/gateway-video-ims.yaml)
  --ai-target HOST:PORT       Segmentation worker (default: 127.0.0.1:50052)
  --gesture-target HOST:PORT  Gesture worker (default: 127.0.0.1:50053)
  --input FILE          Video/image or PCAP/PCAPNG; defaults to testsrc2
  --duration SEC        Media duration per leg (default: 6)
  --gesture-render-mode MODE  realtime | arrival (measure delay) | source (export by source time)
  --require-gesture     With --gesture-only: require a rendered reaction (MF tAccess output)
  --bin-dir DIR         Use prebuilt gateway/MF/RTP tools; omit to build from source
  --artifacts-dir DIR   Keep MP4s, logs, callbacks and metrics here
  --drain-timeout SEC   Allow lab PNG compositor to drain (default: 300)
Additional options: bash scripts/test-video-e2e.sh --help
Keyword example: --gesture-only --gesture-target 127.0.0.1:50053 --gesture-callback-mode keyword
Artifacts include gateway-source.yaml, effective gateway.yaml, and gateway-config-overrides.json.
The runner starts its own gateway and MF test receiver, and acts as DCSF for this test call.
Keyword mode requires at least one mapped reaction; MF logs effects but does not render them.
Console shows gesture start/end frames only when gestures change; full diagnostics remain in gateway.log.
HELP
  exit 0
fi
# Gesture worker chỉ dùng cho dịch vụ video_effects (--gesture-only).
GESTURE_MODE=off
for arg in "$@"; do
  if [[ "$arg" == --gesture-only ]]; then GESTURE_MODE=external; fi
done
exec bash "$SCRIPT_DIR/test-video-e2e.sh" --duration 6 --drain-timeout 300 "$@" \
  --ai-mode external --gesture-mode "$GESTURE_MODE" --output-video
