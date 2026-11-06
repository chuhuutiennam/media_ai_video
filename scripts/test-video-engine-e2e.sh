#!/usr/bin/env bash
# Full RTP -> gateway -> one-source engine -> workers -> HTTP/2 MF -> PNG/MP4.
set -Eeuo pipefail
ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
PYTHON_BIN=${PYTHON:-python3}
PARENT=${TMPDIR:-/tmp}
SEGMENTATION_TARGET= GESTURE_TARGET= MODE=segmentation
RUN_DIR= ENGINE_PID= PREBUILT=
ARGS=()
while (( $# )); do
  case "$1" in
    -h|--help)
      cat <<'HELP'
Usage: bash scripts/test-video-engine-e2e.sh [options]
  --segmentation-target HOST:PORT  Existing model worker; default starts a mock
  --gesture-target HOST:PORT       Existing model worker; default starts a mock
  --segmentation-only              Run change_background: segmentation only, one leg (default)
  --gesture-only                   Run video_effects: gesture only, one leg
  --artifacts-dir DIR              Parent for engine and full media artifacts
  --bin-dir DIR                    Reuse engine/gateway/mock/sender/MF binaries
All other test-video-e2e.sh options (duration/input/PCAP/background/etc.) pass through.
Default: change_background, mock workers, 3 seconds, PNG/MP4. One service uses one AI: no combined mode.
HELP
      exit 0 ;;
    --segmentation-target|--gesture-target|--artifacts-dir|--bin-dir)
      (( $# >= 2 )) || { echo "missing value for $1" >&2; exit 2; }
      case "$1" in --segmentation-target) SEGMENTATION_TARGET=$2;; --gesture-target) GESTURE_TARGET=$2;; --artifacts-dir) PARENT=$2;; --bin-dir) PREBUILT=$2;; esac
      shift 2 ;;
    --segmentation-only) MODE=segmentation; shift ;;
    --gesture-only) MODE=gesture; shift ;;
    --ai-mode|--ai-target|--gesture-mode)
      echo "$1 is managed by this runner; use --segmentation-target/--gesture-target" >&2; exit 2 ;;
    *) ARGS+=("$1"); shift ;;
  esac
done
mkdir -p -- "$PARENT"
RUN_DIR=$(mktemp -d "$PARENT/video-engine-e2e-XXXXXXXX")
RUN_DIR=$(cd -- "$RUN_DIR" && pwd)
cleanup() {
  local code=$?
  trap - EXIT
  if [[ -n "$ENGINE_PID" ]]; then
    kill -TERM -- "-$ENGINE_PID" 2>/dev/null || true
    for _ in {1..40}; do kill -0 -- "-$ENGINE_PID" 2>/dev/null || break; sleep 0.1; done
    kill -KILL -- "-$ENGINE_PID" 2>/dev/null || true
    wait "$ENGINE_PID" 2>/dev/null || true
  fi
  printf 'Engine E2E artifacts: %s\n' "$RUN_DIR"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT TERM
printf '[INFO] Engine E2E artifacts: %s\n' "$RUN_DIR"
read -r ENGINE_PORT METRICS_PORT SEG_PORT GEST_PORT < <("$PYTHON_BIN" - <<'PY'
import socket
sockets = [socket.socket() for _ in range(4)]
try:
    for s in sockets: s.bind(('127.0.0.1', 0))
    print(*(s.getsockname()[1] for s in sockets))
finally:
    for s in sockets: s.close()
PY
)
SEG_MODE=external GEST_MODE=external
if [[ -z "$SEGMENTATION_TARGET" ]]; then SEG_MODE=mock; SEGMENTATION_TARGET="127.0.0.1:$SEG_PORT"; fi
if [[ -z "$GESTURE_TARGET" ]]; then GEST_MODE=mock; GESTURE_TARGET="127.0.0.1:$GEST_PORT"; fi
mkdir -p "$RUN_DIR/bin"
if [[ -n "$PREBUILT" ]]; then PREBUILT=$(cd -- "$PREBUILT" && pwd); fi
cd "$ROOT"
export GOCACHE=${GOCACHE:-/tmp/media-ai-go-cache}
NAMES=(video-ai-engine media-ai-gateway mock-video-rtp-sender)
if [[ ( "$MODE" != gesture && "$SEG_MODE" == mock ) || ( "$MODE" != segmentation && "$GEST_MODE" == mock ) ]]; then
  NAMES+=(mock-ai-worker)
fi
if [[ -z "$PREBUILT" ]]; then
  printf '[INFO] Building engine, gateway and required test tools\n'
  # One invocation reuses package discovery. VCS metadata is unnecessary for
  # disposable test binaries and can make large working trees very slow.
  go build -buildvcs=false -mod=vendor -o "$RUN_DIR/bin/" "${NAMES[@]/#/./cmd/}"
  BUILD_DIR="$RUN_DIR/mf-build" BIN_DIR="$RUN_DIR/bin" bash scripts/build-mf-cpp.sh > "$RUN_DIR/mf-build.log" 2>&1
  PREBUILT="$RUN_DIR/bin"
fi
for name in "${NAMES[@]}" mf_cpp; do
  [[ -x "$PREBUILT/$name" ]] || { echo "missing executable $PREBUILT/$name" >&2; exit 1; }
done
ENGINE_TARGET="127.0.0.1:$ENGINE_PORT"
ENGINE_METRICS="http://127.0.0.1:$METRICS_PORT/metrics"
setsid "$PREBUILT/video-ai-engine" --addr "$ENGINE_TARGET" --metrics-addr "127.0.0.1:$METRICS_PORT" \
  --segmentation-target "$SEGMENTATION_TARGET" --gesture-target "$GESTURE_TARGET" \
  > "$RUN_DIR/engine.log" 2>&1 &
ENGINE_PID=$!
for _ in {1..100}; do
  kill -0 "$ENGINE_PID" 2>/dev/null || { cat "$RUN_DIR/engine.log" >&2; exit 1; }
  if curl --silent --fail --noproxy '*' "$ENGINE_METRICS" > "$RUN_DIR/engine-start.prom"; then break; fi
  sleep 0.1
done
curl --silent --show-error --fail --noproxy '*' "$ENGINE_METRICS" > "$RUN_DIR/engine-start.prom"
TASK_ARGS=(--gesture-mode "$GEST_MODE")
case "$MODE" in
  segmentation) TASK_ARGS=(--gesture-mode off);;
  gesture) TASK_ARGS+=(--gesture-only --require-gesture);;
esac
VIDEO_ENGINE_TARGET="$ENGINE_TARGET" VIDEO_ENGINE_METRICS="$ENGINE_METRICS" \
  bash scripts/test-video-e2e.sh --bin-dir "$PREBUILT" --artifacts-dir "$RUN_DIR" \
    --ai-mode "$SEG_MODE" --ai-target "$SEGMENTATION_TARGET" --gesture-target "$GESTURE_TARGET" \
    --duration 3 --output-video "${TASK_ARGS[@]}" "${ARGS[@]}"
curl --silent --show-error --fail --noproxy '*' "$ENGINE_METRICS" > "$RUN_DIR/engine-final.prom"
printf 'RESULT: PASS - one source stream per leg, independent tasks, MF output and cleanup\n'
