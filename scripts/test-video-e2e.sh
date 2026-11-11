#!/usr/bin/env bash
# Video lab E2E. Bash owns build, services, HTTP, RTP senders, polling and cleanup.
# Embedded Python only handles JSON, mask/metric validation and socket probes.
set -Eeuo pipefail
set +m  # Each setsid child keeps $! as its process-group ID.

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd -- "$SCRIPT_DIR/.." && pwd)
PYTHON_BIN=${PYTHON:-python3}
# AI service là repo riêng: mediapipe (change_background) và gesture (video_effects).
MEDIAPIPE_DIR=${MEDIAPIPE_DIR:-$ROOT/../mediapipe}
GESTURE_DIR=${GESTURE_DIR:-$ROOT/../gesture}   # emoji cho MF lab và kiểm tra pixel gesture
MEDIAPIPE_PYTHON=${MEDIAPIPE_PYTHON:-$PYTHON_BIN}   # Python có dependencies của repo mediapipe
RUN_DIR= FAILURE="" CALL_STARTED=0
AI_MODE=${AI_MODE:-mock} AI_TARGET=${AI_TARGET:-} MODEL=${MEDIAPIPE_MODEL:-}
GESTURE_MODE=off
GESTURE_TARGET=""
REQUIRE_GESTURE=0
GESTURE_ONLY=0
GESTURE_RENDER_MODE=realtime
GESTURE_CALLBACK_MODE=""
GATEWAY_CONFIG=""
GESTURE_FPS=""
MEDIAPIPE_DELEGATE=gpu
OUTPUT_VIDEO=0 BACKGROUND="" PCAP_TCORE=-1 PCAP_TACCESS=-1 IS_PCAP=0
BIN_DIR="" INPUT="" DURATION=30 WIDTH=320 HEIGHT=240 INFER_SIZE=256 EXPECT_PER_LEG=0
TEST_TIMEOUT=${TIMEOUT:-30} GW_PORT=${GW_PORT:-0} CALLBACK_PORT=${CALLBACK_PORT:-0}
CALLBACK_TIMEOUT=5 CALLBACK_WORKERS=2 CALLBACK_RETRIES=1
PLAYOUT_DELAY_MS=300 MASK_MAX_AGE_MS=500 GESTURE_MAX_AGE_MS=500
DRAIN_TIMEOUT=0
ARTIFACTS_DIR=${TMPDIR:-./tmp}
declare -A PIDS=()
CONFIG_OVERRIDE_FLAGS=()

info() { printf '[INFO] %s\n' "$*"; }
fail() { FAILURE=$*; printf '[FAIL] %s\n' "$FAILURE" >&2; exit 1; }
usage() {
  cat <<'HELP'
Usage: bash scripts/test-video-e2e.sh [options]
  --ai-mode mock|external|mediapipe  AI mode (default: mock)
  --gesture-mode off|mock|external  Gesture backend; only with --gesture-only (default: off)
  --gesture-target HOST:PORT        Gesture worker (lab default: 127.0.0.1:50053)
  --require-gesture                Require a rendered reaction per leg; implies --output-video; needs --gesture-only
  --gesture-only                    Run video_effects (gesture) instead of change_background (segmentation)
  --gesture-callback-mode MODE      Override profile mode: legacy (snapshots) | keyword (MF receipt)
  --gesture-fps N                   Override video.services.video_effects.gesture.infer_fps (otherwise read profile)
One service uses one AI: change_background = segmentation only (repo mediapipe), video_effects = gesture only (repo gesture).
Both services have one gateway leg (tcore); results go to the MF tAccess endpoint (ctrl-result single-leg).
  --ai-target HOST:PORT             Segmentation worker (lab default: 127.0.0.1:50052)
  --model FILE                      Local .tflite model for MediaPipe
  --mediapipe-delegate gpu|cpu      MediaPipe delegate (default: gpu)
  --bin-dir DIR                     Use prebuilt binaries; otherwise build gateway and mock tools
  --gateway-config FILE             Gateway defaults (external mode defaults to config/gateway-video-ims.yaml)
                                    Processing defaults from profile; local HTTP/RTP/MF and AI ports for the lab
                                    Explicit CLI options override defaults; source file is preserved
                                    JSON or YAML (YAML needs PyYAML)
  --input FILE                      Image/video or PCAP/PCAPNG (auto-detect); defaults testsrc2
  --pcap-tcore-stream N             Capture stream index for tCore (default: first H264)
  --pcap-taccess-stream N           Capture stream index for tAccess (default: second, or first if only one)
  --output-video                    Save source/mask/composited PNGs and output MP4 for each leg
  --playout-delay-ms N              Realtime playout buffer (default: 300 ms)
  --mask-max-age-ms N               Segmentation expiry (default: 500 ms)
  --gesture-render-mode MODE       realtime (default) | arrival (latency trace) | source (export after drain)
  --gesture-max-age-ms N            Gesture snapshot expiry (default: 500 ms)
  --background FILE                 Replacement JPEG/PNG; default black; requires --output-video
  --duration SECONDS                Duration per leg, 1..300 (default: 30)
  --width N --height N              Even H264 dimensions (default: 320x240)
  --infer-size N                    Override change_background.segmentation infer_width and infer_height
  --expect-per-leg N                Minimum results/task per leg; 0 calculates from duration
  --timeout SECONDS                 Service/cleanup timeout (default: 30)
  --drain-timeout SECONDS           Drain timeout; 0 scales with video duration (default: 0)
  --callback-timeout SECONDS        Override callback.timeout_ms (CLI unit: seconds)
  --callback-workers N              Override result.dispatcher_workers
  --callback-retries N              Override callback.max_retry
  --artifacts-dir DIR               Parent directory for artifacts (default: TMPDIR or ./tmp)
  --gw-port N --callback-port N     HTTP ports; 0 selects free ports
  -h, --help                        Show this help
Environment: AI_MODE, AI_TARGET, MEDIAPIPE_MODEL, TIMEOUT, GW_PORT, CALLBACK_PORT, PYTHON.
External mode reads processing defaults from config/gateway-video-ims.yaml and uses local lab endpoints.
Mock/mediapipe modes keep their generated lab config unless --gateway-config is supplied.
MEDIAPIPE_DIR (default ../mediapipe) and MEDIAPIPE_PYTHON run --ai-mode mediapipe from the mediapipe repo;
GESTURE_DIR (default ../gesture) provides assets/emoji for gesture runs.
VIDEO_GATEWAY_LOG_LEVEL=debug enables verbose per-frame/RLE logging (default: info).
VIDEO_ENGINE_TARGET routes one source stream/leg through an existing video-ai-engine.
VIDEO_ENGINE_METRICS enables engine admission, fan-out and cleanup assertions.
See docs/video-testing.md for build instructions, test profiles and PASS criteria.
HELP
}

start() {
  local name=$1; shift
  setsid -- "$@" >"$RUN_DIR/$name.log" 2>&1 &
  PIDS[$name]=$!
  printf '%s\t%s\n' "$name" "$!" >> "$RUN_DIR/processes.tsv"
}

stream_keyword_logs() {
  # Keep detailed logs in gateway.log; show gesture start/end changes here.
  setsid -- "$PYTHON_BIN" -u "$SCRIPT_DIR/stream-keyword-log.py" "$RUN_DIR/gateway.log" "${PIDS[gateway]}" &
  PIDS[keyword-log]=$!
  printf '%s\t%s\n' keyword-log "${PIDS[keyword-log]}" >> "$RUN_DIR/processes.tsv"
}

stop() {
  local name=$1 pid=${PIDS[$1]:-} deadline
  [[ -n "$pid" ]] || return 0
  kill -TERM -- "-$pid" 2>/dev/null || true
  deadline=$((SECONDS + 5))
  while kill -0 -- "-$pid" 2>/dev/null && (( SECONDS < deadline )); do sleep 0.1; done
  kill -KILL -- "-$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  unset 'PIDS[$name]'
}

cleanup() {
  local code=$?
  trap - EXIT ERR
  trap '' INT TERM
  set +e
  stop build
  stop encode-tcore
  stop encode-taccess
  stop tcore
  stop taccess
  if (( CALL_STARTED )); then
    curl --silent --noproxy '*' --max-time 3 -X DELETE "$CALL_URL" > /dev/null 2>&1
  fi
  stop gateway
  stop keyword-log
  stop callback
  stop ai
  stop gesture-ai
  if (( code != 0 )) && [[ -n "$RUN_DIR" ]]; then
    video_data failure "${FAILURE:-command failed}" || true
    printf 'RESULT: FAIL - %s\nInspect %s\n' "${FAILURE:-command failed}" "$RUN_DIR"
  fi
  exit "$code"
}

check_services() {
  local name
  for name in ai gesture-ai gateway callback; do
    if [[ -n "${PIDS[$name]:-}" ]] && ! kill -0 "${PIDS[$name]}" 2>/dev/null; then
      fail "$name exited; inspect $RUN_DIR/$name.log"
    fi
  done
}

# Predicates return 10 while waiting; all other failures stop the test.
wait_for() {
  local label=$1 seconds=$2 code deadline; shift 2
  deadline=$((SECONDS + seconds))
  while :; do
    check_services
    if "$@"; then return 0; else code=$?; fi
    if (( code != 10 )); then
      if [[ -s "$RUN_DIR/last-error.txt" ]]; then
        fail "$label: $(cat "$RUN_DIR/last-error.txt")"
      fi
      fail "$label failed; inspect $RUN_DIR"
    fi
    if (( SECONDS >= deadline )); then
      if [[ "$label" == 'gateway ready' && -s "$RUN_DIR/gateway-health-error.txt" ]]; then
        fail "timeout after ${seconds}s: $label ($GW_BASE/health/live): $(cat "$RUN_DIR/gateway-health-error.txt")"
      fi
      fail "timeout after ${seconds}s: $label"
    fi
    sleep 0.1
  done
}

http_request() {
  local method=$1 url=$2 output=$3 expected=$4 code
  local args=(--silent --show-error --noproxy '*' --max-time 3 -X "$method" -o "$output" -w '%{http_code}')
  if [[ -n "${5:-}" ]]; then args+=(-H 'Content-Type: application/json' --data-binary "@$5"); fi
  code=$(curl "${args[@]}" "$url") || return 1
  [[ "$code" == "$expected" ]] || { printf '%s %s: HTTP %s, expected %s\n' "$method" "$url" "$code" "$expected" >&2; return 1; }
}

health_ready() {
  curl --silent --show-error --fail --noproxy '*' --max-time 1 "$GW_BASE/health/live" \
    > /dev/null 2>"$RUN_DIR/gateway-health-error.txt" || return 10
}

senders_done() {
  local name code pending=0
  for name in tcore taccess; do
    [[ -n "${PIDS[$name]:-}" ]] || continue
    if kill -0 "${PIDS[$name]}" 2>/dev/null; then
      pending=1
    else
      if wait "${PIDS[$name]}"; then
        unset 'PIDS[$name]'
      else
        code=$?
        printf '%s sender exited with %s; inspect its log\n' "$name" "$code" >&2
        return 1
      fi
    fi
  done
  (( pending == 0 )) || return 10
}

mf_request() {
  local method=$1 path=$2 file=$3 base code
  base=$(cat "$RUN_DIR/mf-url.txt")
  code=$(curl --silent --show-error --noproxy '*' --http2-prior-knowledge --max-time "$TIMEOUT_SECONDS" \
    -X "$method" -o "$RUN_DIR/$file" -w '%{http_code}' "$base$path") || fail "MF $method $path failed"
  [[ "$code" == 200 ]] || fail "MF $method $path: HTTP $code; inspect $RUN_DIR/$file"
}

mf_render_progress() {
  local phase=$1
  mf_request GET /status "mf-${phase}.json"
  video_data "mf-${phase}" "mf-${phase}.json"
}

verify_snapshot() {
  local phase=$1 filename=$2
  http_request GET "$GW_BASE/metrics" "$RUN_DIR/$filename" 200 || return 1
  if [[ -n "${VIDEO_ENGINE_METRICS:-}" ]]; then
    http_request GET "$VIDEO_ENGINE_METRICS" "$RUN_DIR/engine-$phase.prom" 200 || return 1
  fi
  video_data "$phase"
}

main() {
  while (( $# )); do
    case "$1" in
      -h|--help) usage; return 0 ;;
      --output-video) OUTPUT_VIDEO=1; shift ;;
      --gesture-only) GESTURE_ONLY=1; shift ;;
      --require-gesture) REQUIRE_GESTURE=1; OUTPUT_VIDEO=1; shift ;;
      --mf-mode)
        (( $# >= 2 )) && [[ "$2" == cpp ]] || fail 'Only C++ MF is supported; use --mf-mode cpp'
        shift 2 ;;
      --gesture-mode|--gesture-target|--gesture-callback-mode|--gesture-fps|--gateway-config|--ai-mode|--ai-target|--model|--mediapipe-delegate|--bin-dir|--input|--pcap-tcore-stream|--pcap-taccess-stream|--background|--duration|--width|--height|--infer-size|--expect-per-leg|--timeout|--gw-port|--callback-port|--callback-timeout|--callback-workers|--callback-retries|--artifacts-dir|--drain-timeout|--playout-delay-ms|--mask-max-age-ms|--gesture-max-age-ms|--gesture-render-mode)
        (( $# >= 2 )) && [[ "$2" != --* ]] || fail "missing value for $1"
        CONFIG_OVERRIDE_FLAGS+=("$1")
        case "$1" in
          --playout-delay-ms) PLAYOUT_DELAY_MS=$2 ;;
          --mask-max-age-ms) MASK_MAX_AGE_MS=$2 ;;
          --gesture-max-age-ms) GESTURE_MAX_AGE_MS=$2 ;;
          --gesture-render-mode) GESTURE_RENDER_MODE=$2 ;;
          --gesture-callback-mode) GESTURE_CALLBACK_MODE=$2 ;;
          --gateway-config) GATEWAY_CONFIG=$2 ;;
          --gesture-fps) GESTURE_FPS=$2 ;;
          --gesture-mode) GESTURE_MODE=$2 ;;
          --gesture-target) GESTURE_TARGET=$2 ;;
          --ai-mode) AI_MODE=$2 ;; --ai-target) AI_TARGET=$2 ;; --model) MODEL=$2 ;;
          --mediapipe-delegate) MEDIAPIPE_DELEGATE=$2 ;;
          --pcap-tcore-stream) PCAP_TCORE=$2 ;; --pcap-taccess-stream) PCAP_TACCESS=$2 ;;
          --background) BACKGROUND=$2 ;;
          --bin-dir) BIN_DIR=$2 ;; --input) INPUT=$2 ;; --duration) DURATION=$2 ;;
          --width) WIDTH=$2 ;; --height) HEIGHT=$2 ;; --infer-size) INFER_SIZE=$2 ;;
          --expect-per-leg) EXPECT_PER_LEG=$2 ;; --timeout) TEST_TIMEOUT=$2 ;;
          --gw-port) GW_PORT=$2 ;; --callback-port) CALLBACK_PORT=$2 ;;
          --callback-timeout) CALLBACK_TIMEOUT=$2 ;; --callback-workers) CALLBACK_WORKERS=$2 ;;
          --callback-retries) CALLBACK_RETRIES=$2 ;; --artifacts-dir) ARTIFACTS_DIR=$2 ;;
          --drain-timeout) DRAIN_TIMEOUT=$2 ;;
        esac
        shift 2 ;;
      *) fail "unknown option: $1" ;;
    esac
  done
  if [[ "$AI_MODE" == external && -z "$GATEWAY_CONFIG" ]]; then
    GATEWAY_CONFIG="$ROOT/config/gateway-video-ims.yaml"
  fi
  if [[ "$GESTURE_RENDER_MODE" != realtime ]]; then OUTPUT_VIDEO=1; fi
  [[ "$MEDIAPIPE_DELEGATE" == gpu || "$MEDIAPIPE_DELEGATE" == cpu ]] || fail 'MediaPipe delegate must be gpu or cpu'
  [[ -z "$BACKGROUND" || "$OUTPUT_VIDEO" == 1 ]] || fail '--background requires --output-video'
  [[ "$GESTURE_MODE" == off || "$GESTURE_MODE" == mock || "$GESTURE_MODE" == external ]] || fail 'gesture mode must be off, mock or external'
  if (( GESTURE_ONLY )); then
    [[ "$GESTURE_MODE" != off ]] || fail '--gesture-only requires a gesture backend'
    [[ -z "$BACKGROUND" ]] || fail '--background is not supported for gesture-only; source video is preserved'
  fi
  [[ "$REQUIRE_GESTURE" == 0 || "$GESTURE_MODE" != off ]] || fail '--require-gesture requires gesture mode'
  # 1 dịch vụ ↔ 1 AI: gesture chỉ chạy như dịch vụ video_effects riêng.
  [[ "$GESTURE_MODE" == off || "$GESTURE_ONLY" == 1 ]] || fail '--gesture-mode requires --gesture-only (one service uses one AI; change_background is segmentation only)'
  [[ -z "$GESTURE_CALLBACK_MODE" || "$GESTURE_CALLBACK_MODE" == legacy || "$GESTURE_CALLBACK_MODE" == keyword ]] || fail 'gesture callback mode must be legacy or keyword'
  if [[ "$GESTURE_CALLBACK_MODE" == keyword ]]; then
    [[ "$GESTURE_MODE" != off ]] || fail 'keyword mode requires --gesture-only and a gesture backend'
    [[ "$REQUIRE_GESTURE" == 0 ]] || fail '--require-gesture verifies rendered stickers; keyword mode verifies MF receipt automatically'
  fi
  if [[ "$AI_MODE" == mediapipe && "$GESTURE_ONLY" == 0 ]]; then
    [[ -d "$MEDIAPIPE_DIR/src/mediapipe_video" ]] || fail "mediapipe repo not found: $MEDIAPIPE_DIR (set MEDIAPIPE_DIR)"
  fi
  command -v "$PYTHON_BIN" > /dev/null || fail "Python 3 is required"
  mkdir -p -- "$ARTIFACTS_DIR"
  ARTIFACTS_DIR=$(cd -- "$ARTIFACTS_DIR" && pwd)
  RUN_DIR=$(mktemp -d "$ARTIFACTS_DIR/video-e2e-XXXXXXXX")
  trap cleanup EXIT
  trap 'FAILURE=${FAILURE:-"command failed at line $LINENO"}' ERR
  trap 'FAILURE="interrupted"; exit 1' INT TERM
  info "Artifacts: $RUN_DIR"
  info 'Step 1 - Preflight'
  video_data init "$AI_MODE" "$AI_TARGET" "$MODEL" "$BIN_DIR" "$INPUT" "$DURATION" "$WIDTH" "$HEIGHT" \
    "$INFER_SIZE" "$EXPECT_PER_LEG" "$TEST_TIMEOUT" "$GW_PORT" "$CALLBACK_PORT" "$OUTPUT_VIDEO" \
    "$CALLBACK_TIMEOUT" "$CALLBACK_WORKERS" "$CALLBACK_RETRIES" "$DRAIN_TIMEOUT" "$GESTURE_MODE" "$GESTURE_ONLY" "$GESTURE_TARGET" "$REQUIRE_GESTURE" "$GESTURE_RENDER_MODE" "$GESTURE_CALLBACK_MODE" "$GATEWAY_CONFIG" "$GESTURE_FPS" "${CONFIG_OVERRIDE_FLAGS[*]}"
  read -r GW_PORT CALLBACK_PORT EXPECT_PER_LEG TIMEOUT_SECONDS SENDER_TIMEOUT DURATION WIDTH HEIGHT DRAIN_TIMEOUT < "$RUN_DIR/runtime.txt"
  AI_TARGET=$(video_data get options.json ai_target)
  GESTURE_TARGET=$(video_data get options.json gesture_target)
  GESTURE_CALLBACK_MODE=$(video_data get options.json gesture_callback_mode)
  CALLBACK_TIMEOUT=$(video_data get options.json callback_timeout_sec)
  CALLBACK_WORKERS=$(video_data get options.json callback_workers)
  CALLBACK_RETRIES=$(video_data get options.json callback_retries)
  if [[ "$GESTURE_MODE" != off ]]; then
    info "Gesture sampling: $(video_data get options.json gesture_infer_fps) FPS"
  fi
  if [[ "$GESTURE_MODE" != off && "$GESTURE_CALLBACK_MODE" == legacy ]]; then
    [[ -d "$GESTURE_DIR/assets/emoji" ]] || fail "gesture emoji assets not found: $GESTURE_DIR/assets/emoji"
  fi
  INPUT=$(video_data get options.json input)
  IS_PCAP=$(video_data get options.json pcap)
  MODEL=$(video_data get options.json model)
  MF_LEGS=$(video_data get options.json mf_legs)
  BACKGROUND=$(video_data background "$BACKGROUND")
  cd "$ROOT"
  GW_BASE=$(video_data get options.json gateway_base)
  local rtp_ip
  rtp_ip=$(video_data get options.json rtp_ip)
  if [[ -n "$GATEWAY_CONFIG" ]]; then
    info "Gateway defaults: $(video_data get options.json gateway_config_source) (source: $RUN_DIR/gateway-source.yaml; runtime: $RUN_DIR/gateway.yaml)"
    info "Gateway lab/CLI overrides: $(video_data get options.json gateway_config_override_paths)"
    info "Runtime config SHA-256: $(video_data get options.json gateway_config_effective_sha256)"
  else
    info "Gateway profile: generated lab config by video_data init in $SCRIPT_DIR/test-video-e2e.sh"
  fi
  CALL_URL="$GW_BASE/v1/vonras/call-sessions/${RUN_DIR##*/}"
  local tool name
  for tool in curl setsid ffmpeg; do command -v "$tool" > /dev/null || fail "$tool is required"; done
  ffmpeg -hide_banner -encoders > "$RUN_DIR/ffmpeg-encoders.txt" 2>&1
  grep -q libx264 "$RUN_DIR/ffmpeg-encoders.txt" || fail 'FFmpeg must include libx264'
  local names=(media-ai-gateway mock-video-rtp-sender)
  if [[ "$GESTURE_MODE" == mock || ( "$GESTURE_ONLY" == 0 && "$AI_MODE" == mock ) ]]; then names+=(mock-ai-worker); fi
  curl --version | grep -q HTTP2 || fail 'curl with HTTP/2 support is required for C++ MF'
  if [[ -n "$BIN_DIR" ]]; then
    BIN_DIR=$(video_data get options.json bin_dir)
    info "Step 2 - Use binaries from $BIN_DIR"
    names+=(mf_cpp)
    for name in "${names[@]}"; do [[ -x "$BIN_DIR/$name" ]] || fail "missing executable: $BIN_DIR/$name"; done
  else
    for tool in go pkg-config "${CXX:-c++}" "${CC:-cc}"; do
      command -v "$tool" > /dev/null || fail "$tool is required to build; alternatively use --bin-dir"
    done
    [[ "${CGO_ENABLED:-1}" == 1 ]] || fail 'Video gateway requires CGO_ENABLED=1'
    pkg-config --exists libavcodec libavutil libswscale
    BIN_DIR="$RUN_DIR/bin"
    mkdir -p "$BIN_DIR"
    info 'Step 2 - Build gateway and mock tools'
    for name in "${names[@]}"; do
      start build env CGO_ENABLED=1 go build -buildvcs=false -o "$BIN_DIR/$name" "./cmd/$name"
      if ! wait "${PIDS[build]}"; then cat "$RUN_DIR/build.log" >&2; fail "build $name failed"; fi
      unset 'PIDS[build]'
    done
    command -v cmake > /dev/null || fail 'cmake is required to build mf_cpp'
    start build cmake -S "$ROOT/mf" -B "$RUN_DIR/mf-build" -DCMAKE_BUILD_TYPE=Release \
      -DCMAKE_RUNTIME_OUTPUT_DIRECTORY="$BIN_DIR"
    if ! wait "${PIDS[build]}"; then cat "$RUN_DIR/build.log" >&2; fail 'configure mf_cpp failed'; fi
    unset 'PIDS[build]'
    start build cmake --build "$RUN_DIR/mf-build" --parallel 2
    if ! wait "${PIDS[build]}"; then cat "$RUN_DIR/build.log" >&2; fail 'build mf_cpp failed'; fi
    unset 'PIDS[build]'
  fi

  if [[ "$IS_PCAP" == 1 ]] || (( OUTPUT_VIDEO )); then
    "$BIN_DIR/mock-video-rtp-sender" --help > "$RUN_DIR/sender-help.txt" 2>&1
    for name in inspect pcap-stream frames-dir mf-start-url defer-source-png; do
      grep -q -- "-$name" "$RUN_DIR/sender-help.txt" || fail "sender binary lacks --$name; rebuild mock-video-rtp-sender"
    done
  fi
  "$BIN_DIR/mf_cpp" --help > "$RUN_DIR/mf-help.txt" 2>&1
  grep -q -- '--gesture-render-mode' "$RUN_DIR/mf-help.txt" || fail 'mf_cpp lacks gesture render modes; rebuild MF'
  grep -q -- '--events' "$RUN_DIR/mf-help.txt" || fail 'mf_cpp binary is outdated; run scripts/build-mf-cpp.sh'
  grep -q -- '--legs' "$RUN_DIR/mf-help.txt" || fail 'mf_cpp lacks --legs (single-leg services); rebuild MF'
  if [[ "$GESTURE_CALLBACK_MODE" == keyword ]]; then
    grep -q -- 'keyword-mock' "$RUN_DIR/mf-help.txt" || fail 'mf_cpp lacks keyword support; rebuild MF or omit --bin-dir'
  fi
  if (( OUTPUT_VIDEO )); then
    grep -q -- '--frames-root' "$RUN_DIR/mf-help.txt" || fail 'mf_cpp has no renderer; run scripts/build-mf-cpp.sh'
    grep -q -- 'deferred-source-png' "$RUN_DIR/mf-help.txt" || fail 'mf_cpp lacks raw capture/deferred PNG support; rebuild MF and sender'
  fi

  if [[ "$IS_PCAP" == 1 ]]; then
    "$BIN_DIR/mock-video-rtp-sender" --inspect --input "$INPUT" > "$RUN_DIR/input.json"
    video_data input "$PCAP_TCORE" "$PCAP_TACCESS"
    read -r IS_PCAP PCAP_TCORE PCAP_TACCESS DURATION EXPECT_PER_LEG < "$RUN_DIR/input-runtime.txt"
    if [[ "$IS_PCAP" == 1 ]]; then
      info "Replay PCAP: tCore stream $PCAP_TCORE, tAccess stream $PCAP_TACCESS, ${DURATION}s (see input.json)"
    fi
  elif [[ "$PCAP_TCORE" != -1 || "$PCAP_TACCESS" != -1 ]]; then
    fail 'PCAP stream selection requires --input pointing to a capture'
  fi

  info "Callback lab config: ${CALLBACK_WORKERS} senders, ${CALLBACK_TIMEOUT}s/attempt, ${CALLBACK_RETRIES} retries"
  info 'Step 3 - Start AI and gateway'
  if (( ! GESTURE_ONLY )); then
    case "$AI_MODE" in
      mock) start ai "$BIN_DIR/mock-ai-worker" --addr "$AI_TARGET" --log-level debug ;;
      mediapipe) start ai env PYTHONPATH="$MEDIAPIPE_DIR/src${PYTHONPATH:+:$PYTHONPATH}" \
        "$MEDIAPIPE_PYTHON" -m mediapipe_video --addr "$AI_TARGET" --model "$MODEL" --delegate "$MEDIAPIPE_DELEGATE" ;;
    esac
    info "Segmentation backend: mode=$AI_MODE endpoint=$AI_TARGET"
    if [[ "$AI_MODE" == external ]]; then
      info 'External segmentation must already be running; this runner does not start or stop it'
    fi
    wait_for "segmentation AI TCP ready ($AI_TARGET, mode=$AI_MODE)" "$TIMEOUT_SECONDS" video_data tcp "$AI_TARGET"
  fi
  if [[ "$GESTURE_MODE" != off ]]; then
    if [[ "$GESTURE_MODE" == mock ]] && ! { [[ "$GESTURE_ONLY" == 0 && "$AI_MODE" == mock && "$GESTURE_TARGET" == "$AI_TARGET" ]]; }; then
      start gesture-ai "$BIN_DIR/mock-ai-worker" --addr "$GESTURE_TARGET" --log-level debug
    fi
    wait_for "gesture AI TCP ready ($GESTURE_TARGET, mode=$GESTURE_MODE)" "$TIMEOUT_SECONDS" video_data tcp "$GESTURE_TARGET"
  fi
  if [[ -n "$GATEWAY_CONFIG" ]]; then video_data gateway-port-free; fi
  start gateway "$BIN_DIR/media-ai-gateway" --config "$RUN_DIR/gateway.yaml"
  if [[ "$GESTURE_MODE" != off && "$GESTURE_CALLBACK_MODE" == keyword ]]; then
    info "Gesture start/end frames are summarized below; full log: $RUN_DIR/gateway.log"
    stream_keyword_logs
  fi
  wait_for 'gateway ready' "$TIMEOUT_SECONDS" health_ready

  info 'Step 4 - Metrics baseline'
  verify_snapshot baseline metrics-before.prom

  info 'Step 5 - Start C++ MF callback receiver; POST ANSWER and ctrl-result'
  local mf_args=(--port "$CALLBACK_PORT" --call-id "${RUN_DIR##*/}" --events "$RUN_DIR/mf-events.jsonl")
  if (( OUTPUT_VIDEO )); then mf_args+=(--frames-root "$RUN_DIR/output" --background "$BACKGROUND"); fi
  if [[ "$GESTURE_MODE" != off && "$GESTURE_CALLBACK_MODE" == legacy ]]; then mf_args+=(--emoji-dir "$GESTURE_DIR/assets/emoji"); fi
  if (( GESTURE_ONLY )); then mf_args+=(--gesture-only); fi
  mf_args+=(--legs "$MF_LEGS")
  mf_args+=(--gesture-render-mode "$GESTURE_RENDER_MODE" --playout-delay-ms "$PLAYOUT_DELAY_MS" --mask-max-age-ms "$MASK_MAX_AGE_MS" --gesture-max-age-ms "$GESTURE_MAX_AGE_MS")
  start callback "$BIN_DIR/mf_cpp" "${mf_args[@]}"
  wait_for 'callback ready' "$TIMEOUT_SECONDS" video_data callback-ready
  CALL_STARTED=1
  http_request POST "$CALL_URL/notify-event" "$RUN_DIR/answer.json" 201 "$RUN_DIR/notify.json"
  http_request POST "$CALL_URL/ctrl-result" "$RUN_DIR/ctrl-response.json" 200 "$RUN_DIR/ctrl-result.json"
  video_data signaling
  verify_snapshot active metrics-active.prom

  # video-legs.txt: "<gateway leg> <video RTP port> <MF leg>" cho từng leg có trong ANSWER.
  # Dịch vụ 1 leg: gateway tcore → MF tAccess (ctrl-result single-leg dùng callback của tAccess).
  local legs=() leg_line session_leg port mf_leg ssrc leg_args mf_base=""
  mapfile -t legs < "$RUN_DIR/video-legs.txt"
  info "Step 6 - Send H264 RTP to ${#legs[@]} leg(s) (${DURATION}s)"
  local sender_args=(--width "$WIDTH" --height "$HEIGHT" --fps 30 --duration "${DURATION}s")
  [[ -z "$INPUT" ]] || sender_args+=(--input "$INPUT")
  if (( OUTPUT_VIDEO )); then mf_base=$(cat "$RUN_DIR/mf-url.txt"); fi
  for leg_line in "${legs[@]}"; do
    read -r session_leg port mf_leg <<< "$leg_line"
    leg_args=()
    ssrc=88001
    if [[ "$session_leg" == taccess ]]; then ssrc=88002; fi
    if [[ "$IS_PCAP" == 1 ]]; then
      if [[ "$session_leg" == tcore ]]; then leg_args+=(--pcap-stream "$PCAP_TCORE"); else leg_args+=(--pcap-stream "$PCAP_TACCESS"); fi
    fi
    if (( OUTPUT_VIDEO )); then
      leg_args+=(--defer-source-png --mf-start-url "$mf_base/start/$mf_leg" --frames-dir "$RUN_DIR/output/${mf_leg,,}")
    fi
    start "$session_leg" "$BIN_DIR/mock-video-rtp-sender" --target "$rtp_ip:$port" --ssrc "$ssrc" "${sender_args[@]}" "${leg_args[@]}"
  done
  if (( OUTPUT_VIDEO )) && [[ "$GESTURE_RENDER_MODE" != source ]]; then
    wait_for 'MF records first realtime frame' "$SENDER_TIMEOUT" mf_render_progress first
  fi
  wait_for 'video senders' "$SENDER_TIMEOUT" senders_done
  video_data senders

  info "Step 7 - Verify selected task results and drain all frames (minimum $EXPECT_PER_LEG/leg, timeout ${DRAIN_TIMEOUT}s)"
  wait_for 'all packets, JPEG frames, AI results and callbacks' "$DRAIN_TIMEOUT" verify_snapshot drained metrics-after.prom
  stop keyword-log
  info 'Step 8 - Metrics OK; required results drained'
  if (( OUTPUT_VIDEO )); then
    mf_request GET /status mf-live.json
    video_data mf-live mf-live.json
    mf_request POST /finalize mf-finalized.json
    wait_for 'MF realtime playout and recording complete' "$DRAIN_TIMEOUT" mf_render_progress finalized
  fi
  info 'Step 9 - DELETE; wait for sessions, pipelines, AI streams and RTP ports to be released'
  http_request DELETE "$CALL_URL" "$RUN_DIR/delete-response.txt" 204
  CALL_STARTED=0
  wait_for 'session and RTP port cleanup' "$TIMEOUT_SECONDS" verify_snapshot released metrics-cleanup.prom
  if (( OUTPUT_VIDEO )); then
    info 'Step 10 - Export video from C++ MF recording'
    local leg dir encode_name
    for leg_line in "${legs[@]}"; do
      read -r _ _ leg <<< "$leg_line"
      dir="$RUN_DIR/output/${leg,,}"
      encode_name="encode-${leg,,}"

      start "$encode_name" ffmpeg -hide_banner -loglevel error -nostdin -y -f concat -safe 0 -i "$dir/output.ffconcat" \
        -vf 'pad=ceil(iw/2)*2:ceil(ih/2)*2' -enc_time_base 1:90000 -vsync vfr -an -c:v libx264 -threads 1 -pix_fmt yuv420p \
        -movflags +faststart "$dir/output.mp4"
      if ! wait "${PIDS[$encode_name]}"; then cat "$RUN_DIR/$encode_name.log" >&2; fail "encode $leg failed"; fi
      unset 'PIDS[$encode_name]'
      info "Video $leg: $dir/output.mp4"
    done
    video_data output "$BACKGROUND"
  fi
  if [[ "$GESTURE_MODE" != off && "$OUTPUT_VIDEO" == 1 && "$GESTURE_CALLBACK_MODE" == legacy ]]; then
    "$PYTHON_BIN" "$ROOT/scripts/validate-gesture-output.py" "$RUN_DIR" "$GESTURE_DIR/assets/emoji" || fail 'gesture pixel/timeline validation failed'
  fi
  video_data summary
}

# Stateless data operations. No process launching, HTTP requests or polling here.
video_data() {
  "$PYTHON_BIN" - "$RUN_DIR" "$@" <<'PY_VIDEO_DATA'
import errno
import hashlib
import json
import math
import os
import socket
import subprocess
import sys
import time
import traceback
from pathlib import Path
from urllib.parse import urlsplit

VIDEO = "media_ai_video_"


ERROR_METRICS = (
    VIDEO + "pool_dropped_total", VIDEO + "frames_dropped_total", VIDEO + "decode_errors_total",
    VIDEO + "ai_send_errors_total", VIDEO + "ai_recv_errors_total", VIDEO + "ai_result_errors_total",
    VIDEO + "gesture_results_dropped_total", VIDEO + "mask_results_dropped_total",
    "media_ai_dispatcher_send_errors_total", "media_ai_result_queue_dropped_total",
)


def require(condition, message):
    # Explicit checks also run when PYTHONOPTIMIZE is set on the lab machine.
    if not condition:
        raise RuntimeError(message)


def artifact_io(path, operation, *args):
    """Bound retries for transient ENODATA from WSL/9p; retain path on failure."""
    path = Path(path)
    for attempt in range(3):
        try:
            return getattr(path, operation)(*args)
        except OSError as exc:
            if exc.errno == errno.ENODATA and attempt < 2:
                print(f'[WARN] {operation} {path}: {exc}; retry {attempt + 1}/2', file=sys.stderr)
                time.sleep(0.05 * (attempt + 1))
                continue
            raise OSError(exc.errno, f'{operation}: {exc.strerror or str(exc)}', str(path)) from exc


def free_port(kind=socket.SOCK_STREAM):
    with socket.socket(type=kind) as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def udp_range():
    for _ in range(100):
        start = free_port(socket.SOCK_DGRAM)
        start += start % 2  # rtp.even_ports (mặc định): port_start phải chẵn; 8 cổng = 4 cổng chẵn
        if start > 65528:
            continue
        sockets = []
        try:
            for port in range(start, start + 8):
                sock = socket.socket(type=socket.SOCK_DGRAM)
                sockets.append(sock)
                sock.bind(("127.0.0.1", port))
            return start, start + 7
        except (OSError, OverflowError):
            pass
        finally:
            for sock in sockets:
                sock.close()
    raise RuntimeError("cannot find a UDP port range")


def delta(current, baseline, key):
    require(key in current and key in baseline, f"missing metric {key}; rebuild the gateway with video metrics")
    return current[key] - baseline[key]


def ai_failure_detail(run):
    path = Path(run) / 'gateway.log'
    try:
        data = artifact_io(path, 'read_bytes')
    except OSError:
        return {}
    detail = {}
    for line in data.splitlines(keepends=True):
        if not line.endswith(b'\n'):
            break
        try:
            row = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(row, dict) and row.get('msg') in ('ai: video recv error', 'ai: video send error', 'ai: video stream stopped') and row.get('error'):
            detail = {key: row[key] for key in ('ts', 'msg', 'session_id', 'stream_id', 'ai_service', 'reason', 'error') if key in row}
    return detail


def check_errors(current, baseline, allow_frame_eviction=False, run=None):
    for key in ERROR_METRICS:
        if allow_frame_eviction and key == VIDEO + "frames_dropped_total":
            continue
        amount = delta(current, baseline, key)
        message = f"{key} increased by {amount}"
        if amount and run is not None and key in (VIDEO + 'ai_recv_errors_total', VIDEO + 'ai_send_errors_total'):
            detail = ai_failure_detail(run)
            if detail:
                message += f"; AI={detail.get('ai_service', 'video')} session={detail.get('session_id', '')}: {detail['error']}"
        require(amount == 0, message)

    # Older gateways did not export video jitter at all; zero audio jitter
    # counters are not evidence that the video path has no drops.
    for suffix in ('ingress_parse_errors_total', 'ingress_queue_dropped_total',
                   'jitter_late_dropped_total', 'jitter_duplicate_dropped_total',
                   'jitter_output_dropped_total', 'jitter_lost_total',
                   'jitter_overflow_dropped_total', 'decoder_packet_gaps_total',
                   'decoder_wait_idr_episodes_total'):
        key = VIDEO + suffix
        if key in current and key in baseline:
            amount = delta(current, baseline, key)
            require(amount == 0, f'video RTP before decode: {key} increased by {amount}')


def callback_log(path):
    """Read complete JSON lines; a concurrent write may leave the last line partial."""
    if not artifact_io(path, 'exists'):
        return []
    events = []
    for line in artifact_io(path, 'read_bytes').splitlines(keepends=True):
        if not line.endswith(b"\n"):
            break
        event = json.loads(line)
        require(event.get("event") not in ("parse_error", "error", "timeout"),
                f"callback server error: {event}")
        events.append(event)
    return events


def callback_events(run):
    return callback_log(run / 'mf-events.jsonl')


def keyword_evidence(run, minimum, duration, legs):
    """Correlate actual gateway mapping/request/ACK logs with MF receipt logs."""
    rows = []
    path = run / 'gateway.log'
    if artifact_io(path, 'exists'):
        for line in artifact_io(path, 'read_bytes').splitlines(keepends=True):
            if not line.endswith(b'\n'):
                break
            try:
                row = json.loads(line)
            except json.JSONDecodeError:
                continue
            if row.get('msg', '').startswith('keyword: '):
                rows.append(row)
    sources = {mf_leg: {} for _, mf_leg in legs}
    sessions = {f'{run.name}-{session_leg}': mf_leg for session_leg, mf_leg in legs}
    before, after, sending, requests, acks, delivered = {}, set(), {}, {}, set(), {}
    for row in rows:
        message = row['msg']
        if message == 'keyword: before mapping':
            leg = sessions.get(row.get('session_id'))
            require(leg is not None, 'keyword source crossed session')
            g = row.get('gesture_result', row)
            require(row['response_id'] == g['response_id'] and g['status'] == 'ok', 'invalid keyword source identity')
            sources[leg][g['response_id']] = g
            before[(row['session_id'], g['response_id'])] = g
        elif message == 'keyword: after mapping':
            after.add((row['session_id'], row['response_id'], row['topic_label'], row['topic_info']))
        elif message == 'keyword: sending':
            sending[row['idempotency_key']] = row
            if row.get('hands'):
                require(all('raw_label' in hand and 'raw_score' in hand and 'gesture' in hand for hand in row['hands']), 'missing source gesture information')
                after.add((row['session_id'], row['response_id'], row['topic_label'], row['topic_info']))
        elif message == 'keyword: http request':
            key = row['idempotency_key']
            payload = json.loads(row['body'])
            require(row['method'] == 'POST', 'keyword request must use POST')
            require(set(payload) == {'topic_label', 'topic_info'}, 'keyword HTTP body contains unexpected fields')
            previous = requests.get(key)
            require(previous is None or previous['body'] == row['body'], 'retry changed keyword body')
            requests[key] = row
        elif message == 'keyword: http response' and 200 <= row['status'] < 300:
            require(row['protocol'] == 'HTTP/2.0', 'keyword ACK must use HTTP/2')
            acks.add(row['idempotency_key'])
        elif message == 'keyword: delivered':
            delivered[row['idempotency_key']] = row
    counts = {leg: len(frames) for leg, frames in sources.items()}
    keywords = {leg: 0 for leg in sources}
    received = set()
    complete = all(count >= minimum for count in counts.values())
    for frames in sources.values():
        if frames:
            ordered = sorted(frames.values(), key=lambda g: g['frame_id'])
            span = ((ordered[-1]['rtp_timestamp'] - ordered[0]['rtp_timestamp']) & 0xffffffff) / 90000
            complete = complete and span >= duration * 0.75
    for body in callback_events(run):
        if body.get('event') != 'keyword_received':
            continue
        leg = next((leg for leg in sources if body.get('contextId') == f'ctx-{leg}' and body.get('terminationId') == f'term-{leg}'), None)
        require(leg is not None, 'MF keyword crossed media resource')
        expected_path = f'/nmf-mrm/v2/contexts/ctx-{leg}/termination/term-{leg}/keyword'
        require(body['path'] == expected_path and body['protocol'] == 'HTTP/2.0', 'invalid MF keyword path/protocol')
        key = body['idempotency_key']
        require(key and key not in received, 'MF activated a duplicate keyword event')
        received.add(key)
        keywords[leg] += 1
        if key not in requests or key not in sending or key not in delivered or key not in acks:
            complete = False  # The request may still be completing; poll again.
            continue
        request, event = requests[key], sending[key]
        payload = json.loads(request['body'])
        require(request['url'].endswith(expected_path), 'RTPGW keyword URL differs from MF receipt')
        require(payload == {name: body[name] for name in ('topic_label', 'topic_info')}, 'MF received different keyword content')
        source = (event['session_id'], event['response_id'])
        require(source in before, 'keyword has no RTPGW before-mapping evidence')
        require((*source, payload['topic_label'], payload['topic_info']) in after, 'keyword has no RTPGW after-mapping evidence')
        require(all(delivered[key][name] == payload[name] for name in payload), 'delivered keyword differs from request')
    complete = complete and all(keywords.values()) and received == set(delivered) == acks == set(requests)
    return complete, counts, keywords


def leg_resources(legs, mf_leg):
    """mediaResources mong đợi trên callback. Dịch vụ 1 leg: session tcore mang cả resource tCore
    và tAccess (ctrl-result single-leg); 2 leg: mỗi session chỉ mang resource của leg mình."""
    names = ('tCore', 'tAccess') if len(legs) == 1 else (mf_leg,)
    return {name: {"contextId": f"ctx-{name}", "terminationId": f"term-{name}"} for name in names}


def validate(state, call_id, infer_size, minimum, duration, legs, gesture=False, segmentation=True, strict_frames=True, rtp_ip='127.0.0.1'):
    # legs: [[gateway leg, MF leg], ...] — 1 leg: [['tcore', 'tAccess']]; 2 leg: tcore→tCore, taccess→tAccess.
    require(state["answer_status"] == 201, "notify-event ANSWER failed")
    require(state["ctrl_result_status"] == 200, "ctrl-result failed")
    expected_service = 'change_background' if segmentation else 'video_effects'
    require(state['notify_event']['selectedService'] == expected_service, 'unexpected service profile')
    answer = state["answer"]
    require(answer["rtp_ip"] == rtp_ip, 'ANSWER RTP IP differs from the selected gateway profile')
    for key in ("codec", "sample_rate", "tcore_rtp_port", "taccess_rtp_port",
                "tcore_local_non_dc_media", "taccess_local_non_dc_media"):
        require(not answer.get(key), f"video-only call unexpectedly allocated audio: {key}")
    session_legs = [session_leg for session_leg, _ in legs]
    for leg in ("tcore", "taccess"):
        if leg not in session_legs:
            require(not answer.get(f"{leg}_video_rtp_port") and not answer.get(f"{leg}_local_non_dc_video_media"),
                    f"{expected_service}: unexpected video leg {leg}")
    ports = [answer.get(f"{leg}_video_rtp_port") or 0 for leg in session_legs]
    require(all(0 < p < 65536 for p in ports) and len(set(ports)) == len(ports), "invalid video RTP ports")
    for leg, port in zip(session_legs, ports):
        sdp = answer[f"{leg}_local_non_dc_video_media"]
        require(sdp["sdpmLine"] == f"video {port} RTP/AVP 96", f"invalid {leg} video SDP")
        require("rtpmap:96 H264/90000" in sdp["sdpaLines"], f"invalid {leg} codec/clock")
    mf_to_session = {mf_leg: session_leg for session_leg, mf_leg in legs}
    frames = {mf_leg: {} for _, mf_leg in legs}
    for body in state["callbacks"]:
        path = body.get("path") or ""
        leg = path.rsplit("/", 1)[-1]
        require(path.startswith("/results/") and leg in frames, f"unexpected callback URL path: {path}")
        if gesture and body.get("kind") == "video_gesture":
            continue
        require(segmentation, 'gesture-only service received an unexpected segmentation/audio callback')
        require(body["protocol"] == "HTTP/2.0", f"{leg}: callback must use HTTP/2")
        require(body["event_type"] == "video.mask.result" and body["is_final"] is True, f"{leg}: invalid event/final")
        require(body["kind"] == "video_mask", f"{leg}: invalid result kind")
        require(body["session_id"] == f"{call_id}-{mf_to_session[leg]}", f"{leg}: wrong session")
        require(body["stream_id"] == body["session_id"], f"{leg}: wrong stream")
        require(body["mediaResources"] == leg_resources(legs, leg), f"{leg}: wrong callback termination")
        mask = body["video_mask"]
        require(mask["status"] == "ok", f"{leg}: AI mask error: {mask.get('error_message')}")
        require(mask["mask_width"] == infer_size and mask["mask_height"] == infer_size, f"{leg}: wrong mask dimensions")
        require(type(mask["frame_id"]) is int and mask["frame_id"] > 0, f"{leg}: invalid frame ID")
        require(type(mask["rtp_timestamp"]) is int and 0 <= mask["rtp_timestamp"] <= 0xFFFFFFFF, f"{leg}: invalid RTP timestamp")
        counts = mask["rle_counts"]
        require(counts and all(type(n) is int and n >= 0 for n in counts), f"{leg}: invalid RLE counts")
        require(sum(counts) == infer_size * infer_size, f"{leg}: RLE area mismatch")
        previous = frames[leg].get(mask["frame_id"])
        require(previous is None or previous == mask, f"{leg}: conflicting duplicate frame")
        frames[leg][mask["frame_id"]] = mask
    if not segmentation:
        return True, {leg: 0 for leg in frames}
    complete = True
    for leg, masks in frames.items():
        ids = sorted(masks)
        if len(ids) < minimum or (strict_frames and not gesture and ids != list(range(1, len(ids) + 1))):
            complete = False
            continue
        # Wrap-safe RTP coverage rejects streams that only work at the beginning.
        span = ((masks[ids[-1]]["rtp_timestamp"] - masks[ids[0]]["rtp_timestamp"]) & 0xFFFFFFFF) / 90000
        if span < duration * 0.75:
            complete = False
    return complete, {leg: len(masks) for leg, masks in frames.items()}


def read_metrics(path):
    return {parts[0]: float(parts[1]) for line in artifact_io(path, 'read_text').splitlines()
            if line and not line.startswith('#') and len(parts := line.split()) == 2}


def data_main(folder, action, *args):
    run = Path(folder)
    def save(name, value):
        artifact_io(run / name, 'write_text', json.dumps(value, indent=2))
    def load(name):
        return json.loads(artifact_io(run / name, 'read_text'))
    if action == 'failure':
        result = {'result': 'FAIL', 'error': args[0]}
        detail = ai_failure_detail(run)
        if detail:
            result['ai_failure'] = detail
        save('result.json', result)
        return 0
    if action == 'get':
        print(load(args[0])[args[1]])
        return 0
    if action == 'background':
        if args[0]:
            require(Path(args[0]).is_file(), f'background file not found: {args[0]}')
            print(Path(args[0]).resolve())
        return 0
    if action == 'init':
        mode, target, model, binary_dir, input_file = args[:5]
        output_video = args[13]
        duration, width, height, size, minimum, timeout, gw_port, cb_port = (
            convert(value) for convert, value in zip((float, int, int, int, int, float, int, int), args[5:13]))
        callback_timeout, callback_workers, callback_retries = (
            convert(value) for convert, value in zip((float, int, int), args[14:17]))
        drain_timeout = float(args[17])
        gesture_mode = args[18]
        gesture = gesture_mode != "off"
        segmentation = args[19] != "1"
        service = 'change_background' if segmentation else 'video_effects'
        gesture_target, require_gesture = args[20], args[21] == '1'
        gesture_render_mode = args[22] if len(args) > 22 else 'realtime'
        gesture_callback_mode = args[23] if len(args) > 23 else ''
        gateway_config_source = str(Path(args[24]).resolve()) if len(args) > 24 and args[24] else ''
        gesture_fps_arg = args[25] if len(args) > 25 else ''
        explicit_options = set(args[26].split()) if len(args) > 26 else set()
        gesture_fps = int(gesture_fps_arg or 15)
        require(1 <= gesture_fps <= 60, '--gesture-fps must be in 1..60')
        profile, profile_bytes, profile_targets = None, None, []
        profile_changes, effective_profile_bytes = [], None
        rtp_ip = '127.0.0.1'
        if gateway_config_source:
            profile_bytes = Path(gateway_config_source).read_bytes()
            try:
                profile = json.loads(profile_bytes)
            except json.JSONDecodeError:
                try:
                    import yaml
                except ImportError:
                    raise RuntimeError('--gateway-config YAML requires PyYAML in the selected PYTHON environment')
                profile = yaml.safe_load(profile_bytes)
            require(isinstance(profile, dict), 'gateway profile must be an object')
            require(mode == 'external' and (not gesture or gesture_mode == 'external'), '--gateway-config requires external AI')
            video = profile.get('video', {})
            definition = video.get('services', {}).get(service)
            require(video.get('enabled') and isinstance(definition, dict) and definition.get('enabled', True),
                    f'gateway profile must enable video.services.{service}')
            require(definition.get('legs', 'tcore') == 'tcore', 'this runner requires a tcore single-leg service')
            expected_task = 'video_segment' if segmentation else 'gesture_recognition'
            require(definition.get('ai', {}).get('task') == expected_task, 'gateway profile task differs from selected test')

            def override(option, keys, value):
                if option != 'lab' and option not in explicit_options:
                    return
                node = profile
                for key in keys[:-1]:
                    node = node.setdefault(key, {})
                before = node.get(keys[-1])
                if before != value:
                    node[keys[-1]] = value
                    profile_changes.append(dict(option=option, path='.'.join(keys), before=before, after=value))

            # The IMS file supplies processing settings. All endpoints owned
            # by this runner are local and isolated from deployed IMS services.
            gw_port = gw_port or free_port()
            start, end = udp_range()
            for field, value in dict(http_addr=f'127.0.0.1:{gw_port}',
                                     public_url=f'http://127.0.0.1:{gw_port}',
                                     cert_file='', key_file='', metrics_addr='').items():
                option = '--gw-port' if field in ('http_addr', 'public_url') and '--gw-port' in explicit_options else 'lab'
                override(option, ['server', field], value)
            for field, value in dict(listen_addr='127.0.0.1:0', bind_ip='127.0.0.1',
                                     public_ip='127.0.0.1', port_start=start, port_end=end).items():
                override('lab', ['rtp', field], value)
            override('lab', ['callback', 'url'], '')  # ctrl-result supplies the test MF URL.
            override('lab', ['dcsf', 'hosts'], [])
            override('lab', ['pcap', 'enabled'], False)
            for name, option, endpoint in (
                ('change_background', '--ai-target', '127.0.0.1:50052'),
                ('video_effects', '--gesture-target', '127.0.0.1:50053'),
            ):
                if name in video.get('services', {}):
                    keys = ['video', 'services', name, 'ai', 'targets']
                    override('lab', keys, [endpoint])
                    override(option, keys, [target if name == 'change_background' else gesture_target])

            override('--gesture-fps', ['video', 'services', 'video_effects', 'gesture', 'infer_fps'], gesture_fps)
            override('--gesture-callback-mode', ['callback', 'gesture_keywords', 'mode'], gesture_callback_mode)
            override('--infer-size', ['video', 'services', 'change_background', 'segmentation', 'infer_width'], size)
            override('--infer-size', ['video', 'services', 'change_background', 'segmentation', 'infer_height'], size)
            override('--callback-timeout', ['callback', 'timeout_ms'], math.ceil(callback_timeout * 1000))
            override('--callback-workers', ['result', 'dispatcher_workers'], callback_workers)
            override('--callback-retries', ['callback', 'max_retry'], callback_retries)
            gesture_settings = video.get('services', {}).get('video_effects', {}).get('gesture', {})
            segmentation_settings = video.get('services', {}).get('change_background', {}).get('segmentation', {})
            gesture_fps = int(gesture_settings.get('infer_fps', 15))
            require(1 <= gesture_fps <= 60, 'video.services.video_effects.gesture.infer_fps must be in 1..60')
            profile_targets = definition['ai'].get('targets', [])
            require(profile_targets and all(isinstance(t, str) and t for t in profile_targets), 'profile service requires AI targets')
            if segmentation:
                target = profile_targets[0]
            else:
                gesture_target = profile_targets[0]
            size = int(segmentation_settings.get('infer_width', 256))
            require(size == int(segmentation_settings.get('infer_height', 256)), 'video lab verifier requires square inference dimensions')
            cb = profile.get('callback', {})
            callback_timeout = cb.get('timeout_ms', 1000) / 1000
            callback_retries = cb.get('max_retry', 3)
            callback_workers = profile.get('result', {}).get('dispatcher_workers', 16)
            actual_mode = cb.get('gesture_keywords', {}).get('mode', 'legacy') if gesture else 'legacy'
            gesture_callback_mode = actual_mode
            require(actual_mode != 'keyword' or profile.get('log', {}).get('level', 'info') in ('info', 'debug'),
                    'keyword evidence requires log.level=info or debug in the gateway profile')
            public_url = profile['server']['public_url']
            rtp_ip = profile['rtp']['public_ip']
            require(not os.environ.get('VIDEO_ENGINE_TARGET'), '--gateway-config selects its own AI routing; unset VIDEO_ENGINE_TARGET')
            require(not profile.get('audio', {}).get('enabled', True), '--gateway-config must be a video-only profile with audio.enabled=false')
        gesture_callback_mode = gesture_callback_mode or 'legacy'
        require(gesture_callback_mode in ('legacy', 'keyword'), 'invalid --gesture-callback-mode')
        require(gesture_callback_mode != 'keyword' or gesture, 'keyword mode requires a gesture backend')
        require(gesture_callback_mode != 'keyword' or not require_gesture, 'keyword mode verifies receipt, not rendered stickers')
        require(gesture_render_mode in ('realtime', 'arrival', 'source'), 'invalid --gesture-render-mode')
        require(gesture or gesture_render_mode == 'realtime', 'gesture render modes require a gesture backend')
        require(gesture_mode in ('off', 'mock', 'external'), 'invalid gesture mode')
        require(gesture_mode != 'external' or gesture_target, 'external gesture requires --gesture-target')
        require(segmentation or gesture, 'gesture-only requires gesture mode')
        require(not (segmentation and gesture), 'one service uses one AI: gesture requires --gesture-only')
        # [gateway leg, MF leg]: change_background và video_effects chỉ leg tcore, kết quả về MF tAccess
        # (ctrl-result single-leg). validate()/leg_resources() vẫn hỗ trợ dịch vụ 2 leg (legs: both).
        legs = [['tcore', 'tAccess']]
        require(mode in ('mock', 'external', 'mediapipe'), 'invalid --ai-mode')
        require(math.isfinite(duration) and 1 <= duration <= 300, 'duration must be in 1..300 seconds')
        require(math.isfinite(timeout) and timeout > 0 and minimum >= 0, 'timeout must be positive; expect-per-leg must be nonnegative')
        require(0 < width <= 1920 and 0 < height <= 1080 and width % 2 == height % 2 == 0, 'positive even dimensions, at most 1920x1080 required')
        require(1 <= size <= 1024, 'infer-size must be in 1..1024')
        require(all(0 <= p < 65536 for p in (gw_port, cb_port)), 'TCP ports must be in 0..65535')
        require(math.isfinite(callback_timeout) and 0 < callback_timeout <= 300,
                'callback-timeout must be in (0, 300] seconds')
        require(1 <= callback_workers <= (256 if profile else 16), 'invalid callback worker count')
        require(0 <= callback_retries <= 10, 'callback-retries must be in 0..10')
        require(math.isfinite(drain_timeout) and drain_timeout >= 0, 'drain-timeout must be nonnegative')
        require(not segmentation or mode != 'external' or target, 'external segmentation requires --ai-target')
        require(not segmentation or mode != 'mediapipe' or Path(model).is_file(), 'mediapipe requires --model pointing to a local file')
        require(not input_file or Path(input_file).is_file(), f'input file not found: {input_file}')
        capture = False
        if input_file:
            with open(input_file, 'rb') as source:
                capture = source.read(4) in [bytes.fromhex(magic) for magic in
                    ('d4c3b2a1', 'a1b2c3d4', '4d3cb2a1', 'a1b23c4d', '0a0d0d0a')]
        target = target or (profile_targets[0] if profile else f'127.0.0.1:{free_port()}')
        if gesture and not gesture_target:
            if mode == 'mock':
                gesture_target = target
            else:
                gesture_target = f'127.0.0.1:{free_port()}'
                while gesture_target == target:
                    gesture_target = f'127.0.0.1:{free_port()}'
        engine_target = os.environ.get('VIDEO_ENGINE_TARGET', '')
        strict_mock = not engine_target and (not segmentation or mode == 'mock') and (not gesture or gesture_mode == 'mock')
        gw_port = gw_port or free_port()
        start, end = (profile['rtp']['port_start'], profile['rtp']['port_end']) if profile else udp_range()
        task_infer_fps = gesture_fps if gesture else (segmentation_settings.get('infer_fps', 5) if profile else 5)
        minimum = minimum or max(2, int(duration * task_infer_fps * 0.6))
        callback_timeout_ms = math.ceil(callback_timeout * 1000)
        # Dispatcher wraps the entire Send(), including every attempt and the
        # sink's 200ms retry backoff. Keep that outer deadline longer than HTTP.
        dispatch_timeout_ms = callback_timeout_ms * (callback_retries + 1) + 200 * callback_retries + 1000
        # A live C++ receiver also writes all source-rate PNGs. Allow its queue
        # to drain after real-time RTP playback, independently of service startup.
        drain_timeout = math.ceil(drain_timeout or max(timeout, duration * 3 + dispatch_timeout_ms / 1000))
        if profile is not None:
            override('lab', ['session', 'idle_timeout_sec'],
                     max(profile.get('session', {}).get('idle_timeout_sec', 0),
                         math.ceil(duration + drain_timeout + timeout + 60)))
            effective_profile_bytes = (json.dumps(profile, indent=2, ensure_ascii=False) + '\n').encode()
        save('options.json', dict(mode=mode, mf_mode='cpp', output_video=output_video == '1', ai_target=target, bin_dir=str(Path(binary_dir).resolve()),
            gateway_config_source=gateway_config_source, gateway_config_sha256=hashlib.sha256(profile_bytes).hexdigest() if profile_bytes is not None else '',
            gateway_config_effective_sha256=hashlib.sha256(effective_profile_bytes).hexdigest() if effective_profile_bytes is not None else '',
            gateway_config_overrides=profile_changes, gateway_config_override_paths=', '.join(change['path'] for change in profile_changes) or '(none)',
            gateway_base=public_url if profile else f'http://127.0.0.1:{gw_port}', rtp_ip=rtp_ip, profile_targets=profile_targets,
            task_infer_fps=task_infer_fps,
            gesture_infer_fps=gesture_fps,
            input=str(Path(input_file).resolve()) if input_file else '', model=str(Path(model).resolve()) if model else '',
            duration=duration, infer_size=size, minimum=minimum, gesture_render_mode=gesture_render_mode, gesture_callback_mode=gesture_callback_mode, gesture=gesture, gesture_mode=gesture_mode, gesture_target=gesture_target, require_gesture=require_gesture, strict_mock=strict_mock, engine=bool(engine_target), segmentation=segmentation, service=service, legs=legs, mf_legs=len(legs), minimum_auto=int(args[9]) == 0, pcap=int(capture),
            callback_timeout_ms=callback_timeout_ms, callback_timeout_sec=callback_timeout, callback_workers=callback_workers,
            callback_retries=callback_retries, dispatch_timeout_ms=dispatch_timeout_ms,
            drain_timeout_sec=drain_timeout))
        (run / 'runtime.txt').write_text(f'{gw_port} {cb_port} {minimum} {math.ceil(timeout)} {math.ceil(duration + 65)} {duration} {width} {height} {drain_timeout}\n')
        if profile_bytes is not None:
            # Preserve the IMS source and write the lab/CLI overlay for this run.
            (run / 'gateway-source.yaml').write_bytes(profile_bytes)
            (run / 'gateway.yaml').write_bytes(effective_profile_bytes)
            save('gateway-config-overrides.json', profile_changes)
        else:
            gateway_config = {
                'gateway': {'id': 'gw-video-test', 'raw_rtp_enabled': True, 'webrtc_enabled': False, 'mock_result_pump': False},
                'server': {'http_addr': f'127.0.0.1:{gw_port}', 'public_url': f'http://127.0.0.1:{gw_port}', 'shutdown_timeout_sec': 2},
                'rtp': {'listen_addr': '127.0.0.1:0', 'bind_ip': '127.0.0.1', 'public_ip': '127.0.0.1', 'port_start': start, 'port_end': end},
                'session': {'max_sessions': 10, 'per_session_packet_queue': 1024, 'idle_timeout_sec': math.ceil(duration + drain_timeout + timeout + 60)},
                'pipeline': {'jitter_out_queue_size': 1024},
                # Shared pool also carries video RPCs. After a finite file finishes,
                # Python workers can reject idle PINGs while the PNG lab still drains.
                'audio': {'enabled': False},  # gateway chỉ video: ANSWER dịch vụ audio → 400
                'ai': {'grpc_target': '', 'keepalive_time_sec': 0},
                'result': {'dispatcher_workers': callback_workers, 'queue_size': 4096,
                           'drop_partial_when_full': False, 'send_timeout_ms': dispatch_timeout_ms},
                'callback': {'timeout_ms': callback_timeout_ms, 'max_retry': callback_retries},
                'video': {'enabled': True, 'worker_count': 2, 'job_queue_size': 1024, 'infer_width': size,
                          'infer_height': size, 'infer_fps': 5, 'frame_queue_size': 8, 'default_effect_type': 'bg_replace' if segmentation else '',
                          'gesture': {'infer_fps': gesture_fps, 'max_dimension': 640, 'max_hands': 4, 'frame_queue_size': 8},
                          'ai': {'max_retry': 0, 'stream_timeout_sec': math.ceil(duration + drain_timeout + timeout + 120)}},
                'log': {'level': os.environ.get('VIDEO_GATEWAY_LOG_LEVEL', 'info')},
            }
            if engine_target:
                # Engine chưa vào video.services: dùng config cũ (cả 2 dịch vụ chỉ leg tcore, gesture khi bật).
                gateway_config['video']['gesture']['enabled'] = gesture
                gateway_config['video']['ai'] = dict(engine_target=engine_target, source_max_dimension=max(640, size), max_retry=0)
            else:
                # Danh mục dịch vụ: đúng 1 dịch vụ đang test, đúng 1 AI service của nó. Thông số AI nằm
                # trong dịch vụ; khoá chung video.infer_*/gesture/default_effect_type bị gateway từ chối.
                video = gateway_config['video']
                segmentation_params = {key: video.pop(key) for key in ('infer_fps', 'infer_width', 'infer_height')}
                gesture_params = video.pop('gesture')
                video.pop('default_effect_type')
                task, ai_address = ('video_segment', target) if segmentation else ('gesture_recognition', gesture_target)
                definition = {'legs': 'tcore',
                              'ai': {'task': task, 'targets': [ai_address], 'max_streams': 32}}
                if segmentation:
                    definition['effect_type'] = 'bg_replace'
                    definition['segmentation'] = segmentation_params
                else:
                    definition['gesture'] = gesture_params
                video['services'] = {service: definition}
            if gesture_callback_mode == 'keyword':
                gateway_config['callback']['gesture_keywords'] = {'mode': 'keyword'}
                require(gateway_config['log']['level'] in ('info', 'debug'),
                        'keyword evidence requires VIDEO_GATEWAY_LOG_LEVEL=info or debug')
            save('gateway.yaml', gateway_config)
        save('notify.json', {'callId': run.name, 'event': 'ANSWER', 'selectedService': service})
        return 0
    if action == 'tcp':
        options = load('options.json')
        for address in options.get('profile_targets') or [args[0]]:
            host, port = address.rsplit(':', 1)
            try:
                with socket.create_connection((host, int(port)), timeout=0.2):
                    return 0
            except OSError:
                pass
        return 10

    if action == 'gateway-port-free':
        raw = (run / 'gateway.yaml').read_text()
        try:
            profile = json.loads(raw)
        except json.JSONDecodeError:
            import yaml
            profile = yaml.safe_load(raw)
        addr = urlsplit('//' + profile['server']['http_addr'])
        host = addr.hostname or '0.0.0.0'
        with socket.socket(socket.AF_INET6 if ':' in host else socket.AF_INET, socket.SOCK_STREAM) as sock:
            try:
                sock.bind((host, addr.port))
            except OSError as error:
                raise RuntimeError(f'gateway HTTP address unavailable ({profile["server"]["http_addr"]}): {error}; runner starts its own gateway') from error
        return 0

    if action == 'callback-ready':
        ready = next((event for event in callback_events(run) if event.get('event') == 'ready'), None)
        if ready is None:
            return 10
        require(0 < ready['port'] < 65536, 'callback server must report its actual port; rebuild the binary')
        (run / 'mf-url.txt').write_text(f"http://127.0.0.1:{ready['port']}\n")
        save('ctrl-result.json', {'callId': run.name, 'mediaResources': {
            leg: {'contextId': f'ctx-{leg}', 'termination': {'terminationId': f'term-{leg}'},
                    'callbackUrl': f'http://127.0.0.1:{ready["port"]}/results/{leg}'} for leg in ('tCore', 'tAccess')}})
        if load('options.json').get('gesture_callback_mode') == 'keyword':
            control = load('ctrl-result.json')
            # Single-leg API takes task overrides from tCore, but MF destination
            # and resource identity belong to tAccess.
            control['mediaResources']['tCore']['callbackUrls'] = {'gesture_recognition':
                f'http://127.0.0.1:{ready["port"]}/nmf-mrm/v2/contexts/ctx-tAccess/termination/term-tAccess/keyword'}
            save('ctrl-result.json', control)
        return 0
    options = load('options.json')
    if action == 'mf-first':
        status = load(args[0])
        require(status['errors'] == 0, f"MF render failure: {status}")
        if not any(leg['frames_written'] > 0 for leg in status['legs'].values()):
            return 10
        save('mf-first-frame.json', status)
        return 0
    if action in ('mf-live', 'mf-finalized'):
        status = load(args[0])
        require(status['errors'] == 0, f"MF render failure: {status}")
        if action == 'mf-finalized' and not status['finalized']:
            return 10
        progress = load('progress.json')
        if options.get('gesture_callback_mode') == 'keyword':
            require(status['keywords_received'] == sum(progress['keywords'].values()), 'MF keyword count mismatch')
        for leg, count in progress['masks'].items():
            folder = run / 'output' / leg.lower()
            state = status['legs'][leg]
            require(state['masks'] == count, f'{leg}: MF mask count mismatch')
            expected_gestures = 0 if options.get('gesture_callback_mode') == 'keyword' else progress['gestures'][leg]
            require(state['gestures'] == expected_gestures, f'{leg}: MF gesture count mismatch')
            if action == 'mf-live':
                continue
            frames = json.loads((folder / 'frames.json').read_text())
            mapping = json.loads((folder / 'render.json').read_text())
            require(state['finalized'] and len(mapping) == len(frames), f'{leg}: incomplete recording')
            require(state['frames_written'] + state['frames_dropped'] == len(frames), f'{leg}: recording accounting mismatch')
            masks = [c for c in load('callbacks.json')['callbacks'] if c['path'] == f'/results/{leg}' and c['kind'] == 'video_mask']
            for i, row in enumerate(mapping):
                require(row['rtp_timestamp'] == frames[i]['rtp_timestamp'], f'{leg}: timeline changed')
                if row['dropped']:
                    require(i > 0 and row['file'] == mapping[i-1]['file'] and row['stickers'] == mapping[i-1]['stickers'],
                            f'{leg}: dropped frame must repeat emitted image')
                    continue
                eligible = [c['video_mask'] for c in masks if (options.get('gesture_render_mode') == 'source' or c['render']['accepted_at_ms'] <= row['deadline_ms']) and
                            ((frames[i]['rtp_timestamp'] - c['video_mask']['rtp_timestamp']) & 0xffffffff) < 0x80000000]
                selected = min(eligible, key=lambda m: ((frames[i]['rtp_timestamp'] - m['rtp_timestamp']) & 0xffffffff), default=None)
                age = ((frames[i]['rtp_timestamp'] - selected['rtp_timestamp']) & 0xffffffff) // 90 if selected else None
                expected_id = selected['frame_id'] if selected and age <= state['mask_max_age_ms'] else 0
                require(row['mask_frame_id'] == expected_id, f'{leg}: deadline mask selection mismatch')
                require(not row['exact_mask'] or (selected and selected['rtp_timestamp'] == row['rtp_timestamp']), f'{leg}: invalid exact mask')
            if not options['segmentation']:
                require(count == 0 and all(row['mask_frame_id'] == 0 for row in mapping), f'{leg}: unexpected segmentation')
            if options.get('gesture_callback_mode') == 'keyword':
                require(all(not row['stickers'] for row in mapping), f'{leg}: keyword mock must not render stickers')
            require(all((folder / row['file']).is_file() for row in mapping), f'{leg}: missing recorded PNG')
        return 0
    if action == 'input':
        capture = load('input.json')
        core, access = map(int, args[:2])
        if capture['pcap']:
            streams = capture['streams']
            require(len(streams) <= 2 or core >= 0 and access >= 0,
                    'capture has more than 2 H264 streams; select --pcap-tcore-stream and --pcap-taccess-stream from input.json')
            core = 0 if core == -1 else core
            access = min(1, len(streams)-1) if access == -1 else access
            require(0 <= core < len(streams) and 0 <= access < len(streams), 'invalid capture stream index')
            options['duration'] = min(options['duration'], streams[core]['duration_sec'], streams[access]['duration_sec'])
            require(options['duration'] >= 1, 'selected capture must contain at least 1 second of video')
            if options['minimum_auto']:
                options['minimum'] = max(2, int(options['duration'] * options.get('task_infer_fps', 5) * 0.6))
            options['pcap_streams'] = {'tCore': core, 'tAccess': access}
            save('options.json', options)
        else:
            require(core == access == -1, 'PCAP stream selection requires a PCAP/PCAPNG input')
        (run / 'input-runtime.txt').write_text(f"{int(capture['pcap'])} {core} {access} {options['duration']} {options['minimum']}\n")
        return 0
    if action == 'signaling':
        state = dict(notify_event=load('notify.json'), answer_status=201, answer=load('answer.json'),
                     ctrl_result=load('ctrl-result.json'), ctrl_result_status=200, callbacks=[])
        validate(state, run.name, options['infer_size'], options['minimum'], options['duration'], options['legs'], options['gesture'], options['segmentation'], options['strict_mock'], options.get('rtp_ip', '127.0.0.1'))
        save('signaling.json', state)
        (run / 'video-legs.txt').write_text(''.join(
            f"{session_leg} {state['answer'][f'{session_leg}_video_rtp_port']} {mf_leg}\n" for session_leg, mf_leg in options['legs']))
        return 0
    if action == 'senders':
        save('senders.json', {leg: json.loads((run / f'{leg}.log').read_text().strip().splitlines()[-1]) for leg, _ in options['legs']})
        return 0
    if action == 'summary':
        result = load('result.json')
        print(f"RESULT: PASS ({result['mode']}, {result['service']}, gesture-render={result.get('gesture_render_mode', 'realtime')}) - {result['packets']} packets; masks {result['masks_per_leg']}; gestures {result['gestures_per_leg']}; metrics and RTP cleanup OK")
        if options.get('gesture_callback_mode') == 'keyword':
            print(f"Keyword mapping/request/HTTP2 ACK/MF receipt: PASS {result['keywords_per_leg']}; MF effect rendering: mock only")
        return 0
    if action == 'output':
        result = load('result.json')
        result['realtime_playout'] = load('mf-finalized.json')
        result['gesture_render_mode'] = options.get('gesture_render_mode', 'realtime')
        result['output_videos'] = {leg: str(run / 'output' / leg.lower() / 'output.mp4') for _, leg in options['legs']}
        result['background'] = (args[0] or 'black') if options['segmentation'] else 'source'
        for path in result['output_videos'].values():
            require(Path(path).is_file() and Path(path).stat().st_size > 0, f'output video missing: {path}')
        result['mp4_timeline'] = {}
        for leg, path in result['output_videos'].items():
            frames = load(f'output/{leg.lower()}/frames.json')
            probe = json.loads(subprocess.check_output(['ffprobe', '-v', 'error', '-select_streams', 'v:0',
                '-show_frames', '-show_entries', 'frame=best_effort_timestamp_time', '-of', 'json', path], text=True))
            pts = [float(f['best_effort_timestamp_time']) for f in probe['frames']]
            # The concat end marker repeats the last emitted image to preserve
            # its duration; it is not a new render or a late-AI correction.
            require(len(pts) == len(frames) + 1, f'{leg}: MP4 dropped or duplicated source frames')
            require(all(abs(t - f['time_sec']) <= 3 / 90000 for t, f in zip(pts, frames)),
                    f'{leg}: MP4 timestamps differ from recorded realtime timeline')
            result['mp4_timeline'][leg] = {'source_frames': len(frames), 'encoded_frames': len(pts),
                                          'end_marker_frames': 1, 'pts_verified': True}
        save('result.json', result)
        return 0
    baseline = read_metrics(run / 'metrics-before.prom')
    if action == 'baseline':
        check_errors(baseline, baseline, allow_frame_eviction=not options['strict_mock'])
        require(baseline['media_ai_sessions_active'] == 0, 'expected an isolated gateway')
        if artifact_io(run / 'engine-baseline.prom', 'exists'):
            require(read_metrics(run / 'engine-baseline.prom')['video_engine_sources_active'] == 0, 'expected an isolated engine')
        return 0
    filenames = {'active': 'metrics-active.prom', 'drained': 'metrics-after.prom', 'released': 'metrics-cleanup.prom'}
    require(action in filenames, f'unknown data operation: {action}')
    current = read_metrics(run / filenames[action])
    if action != 'drained':
        check_errors(current, baseline, allow_frame_eviction=not options['strict_mock'], run=run)
    if action == 'active':
        legs = len(options['legs'])
        require(current['media_ai_rtp_ports_available'] == baseline['media_ai_rtp_ports_available'] - legs, f'expected exactly {legs} video port(s)')
        require(current['media_ai_ai_streams_active'] == 0, 'unexpected audio AI stream')
        # 1 dịch vụ ↔ 1 AI: mỗi leg đúng một stream (task direct hoặc source Engine).
        require(current[VIDEO + 'ai_streams_active'] == legs, 'unexpected video source/task stream count')
        if artifact_io(run / 'engine-active.prom', 'exists'):
            engine = read_metrics(run / 'engine-active.prom')
            require(engine['video_engine_sources_active'] == legs, 'expected one engine source per leg')
            require(engine['video_engine_segmentation_active'] == legs * int(options['segmentation']), 'wrong segmentation reservation count')
            require(engine['video_engine_gesture_active'] == legs * int(options['gesture']), 'wrong gesture reservation count')
        return 0
    counters = (VIDEO + 'pool_processed_total', VIDEO + 'frames_total', VIDEO + 'ai_frames_sent_total',
                VIDEO + 'ai_results_received_total', 'media_ai_dispatcher_sent_total')
    if action == 'drained':
        state = load('signaling.json')
        state['callbacks'] = [event for event in callback_events(run) if event.get('event') == 'callback']
        save('callbacks.json', state)
        valid, counts = validate(state, run.name, options['infer_size'], options['minimum'], options['duration'], options['legs'], options['gesture'], options['segmentation'], options['strict_mock'], options.get('rtp_ip', '127.0.0.1'))
        packets = sum(sender['packets'] for sender in load('senders.json').values())
        amounts = {key: delta(current, baseline, key) for key in counters}
        mf_to_session = {mf_leg: session_leg for session_leg, mf_leg in options['legs']}
        gesture_counts = {mf_leg: 0 for mf_leg in mf_to_session}
        keyword_counts = {mf_leg: 0 for mf_leg in mf_to_session}
        if options.get('gesture_callback_mode') == 'keyword':
            require(not state['callbacks'], 'keyword mode received legacy callbacks; rebuild gateway or check generated config')
            keyword_complete, gesture_counts, keyword_counts = keyword_evidence(run, options['minimum'], options['duration'], options['legs'])
            valid = valid and keyword_complete and delta(current, baseline, VIDEO + 'gesture_results_received_total') == sum(gesture_counts.values())
        elif options['gesture']:
            for body in state['callbacks']:
                if body.get('kind') != 'video_gesture':
                    continue
                leg = body['path'].rsplit('/', 1)[1]
                session_leg = mf_to_session[leg]
                require(body['event_type'] == 'video.gesture.result' and body['protocol'] == 'HTTP/2.0', 'invalid gesture callback type/protocol')
                require(body['session_id'] == f'{run.name}-{session_leg}' and body['stream_id'] == f'{run.name}:{session_leg}:video', 'gesture crossed leg/session')
                require(body['mediaResources'] == leg_resources(options['legs'], leg), 'gesture resource mismatch')
                g = body['gesture_result']
                require(g['status'] == 'ok' and len(g['hands']) <= 4, 'invalid gesture snapshot')
                gesture_counts[leg] += 1
            valid = valid and all(n >= options['minimum'] for n in gesture_counts.values())
            for leg in gesture_counts:
                snapshots = [body['gesture_result'] for body in state['callbacks']
                             if body.get('kind') == 'video_gesture' and body['path'] == f'/results/{leg}']
                if snapshots:
                    span = ((snapshots[-1]['rtp_timestamp'] - snapshots[0]['rtp_timestamp']) & 0xFFFFFFFF) / 90000
                    valid = valid and span >= options['duration'] * 0.75
            valid = valid and delta(current, baseline, VIDEO + 'gesture_results_received_total') == sum(gesture_counts.values())
        total = sum(counts.values()) + sum(gesture_counts.values())
        save('progress.json', {'packets_sent': packets, 'masks': counts, 'gestures': gesture_counts, 'keywords': keyword_counts,
                               'frames_evicted': delta(current, baseline, VIDEO + 'frames_dropped_total'),
                               'rtp_stages': {key: delta(current, baseline, key) for key in current
                                              if key.startswith((VIDEO + 'ingress_', VIDEO + 'jitter_'))}, **amounts})
        check_errors(current, baseline, allow_frame_eviction=not options['strict_mock'], run=run)
        dropped = delta(current, baseline, VIDEO + 'frames_dropped_total')
        if options.get('engine'):
            frames_complete = amounts[counters[1]] == amounts[counters[2]] + dropped
            results_complete = all(amounts[key] == total for key in counters[3:])
            if artifact_io(run / 'engine-drained.prom', 'exists'):
                engine, initial = read_metrics(run / 'engine-drained.prom'), read_metrics(run / 'engine-baseline.prom')
                frames_complete = frames_complete and delta(engine, initial, 'video_engine_source_frames_total') == amounts[counters[2]]
                results_complete = results_complete and delta(engine, initial, 'video_engine_mask_results_total') == sum(counts.values()) and delta(engine, initial, 'video_engine_gesture_results_total') == sum(gesture_counts.values())
                require(delta(engine, initial, 'video_engine_task_errors_total') == 0, 'engine task failed')
                require(delta(engine, initial, 'video_engine_result_drops_total') == 0, 'engine dropped output results')
        else:
            frames_complete = amounts[counters[1]] == total + dropped
            results_complete = all(amounts[key] == total for key in counters[2:])
        complete = (valid and amounts[counters[0]] == packets and frames_complete and results_complete
                    and delta(current, baseline, VIDEO + 'pool_submitted_total') == packets
                    and current[VIDEO + 'pool_queue_len'] == current['media_ai_dispatcher_queue_len'] == 0)
        return 0 if complete else 10
    complete = (current['media_ai_sessions_active'] == baseline['media_ai_sessions_active']
                and current['media_ai_rtp_ports_available'] == baseline['media_ai_rtp_ports_available']
                and current[VIDEO + 'pool_sessions_active'] == current[VIDEO + 'ai_streams_active'] == 0)
    if not complete:
        return 10
    if artifact_io(run / 'engine-released.prom', 'exists'):
        engine = read_metrics(run / 'engine-released.prom')
        if any(engine[f'video_engine_{name}_active'] for name in ('sources', 'segmentation', 'gesture')):
            return 10
    after = read_metrics(run / 'metrics-after.prom')
    for key in counters:
        require(current[key] == after[key], f'counter changed after drain/DELETE: {key}')
    progress = load('progress.json')
    save('result.json', dict(result='PASS', mode=options['mode'], mf_mode=options['mf_mode'], call_id=run.name, packets=progress['packets_sent'],
        gateway_config_source=options.get('gateway_config_source', ''), gateway_config_sha256=options.get('gateway_config_sha256', ''),
        masks_per_leg=progress['masks'], gestures_per_leg=progress.get('gestures', {}), duration_sec=options['duration'],
        service=options['service'], engine_fanout=options.get('engine', False), segmentation_enabled=options['segmentation'], gesture_enabled=options['gesture'],
        gesture_mode=options['gesture_mode'], require_gesture=options['require_gesture'], strict_mock=options['strict_mock'],
        gesture_callback_mode=options.get('gesture_callback_mode', 'legacy'), keywords_per_leg=progress.get('keywords', {}),
        min_masks_per_leg=options['minimum'] if options['segmentation'] else 0,
        min_gestures_per_leg=options['minimum'] if options['gesture'] else 0,
        frames_evicted=progress['frames_evicted'], ports_available=current['media_ai_rtp_ports_available']))
    return 0


if __name__ == '__main__':
    try:
        sys.exit(data_main(*sys.argv[1:]))
    except (OSError, ValueError, KeyError, TypeError, RuntimeError) as exc:
        diagnostic = traceback.format_exc()
        print(diagnostic, file=sys.stderr, end='')
        if len(sys.argv) > 1 and Path(sys.argv[1]).is_dir():
            try:
                artifact_io(Path(sys.argv[1]) / 'last-error.txt', 'write_text', str(exc) + '\n')
                artifact_io(Path(sys.argv[1]) / 'last-error-traceback.txt', 'write_text', diagnostic)
            except OSError as report_error:
                print(f'[FAIL] cannot save error report: {report_error}', file=sys.stderr)
        print(f'[FAIL] {exc}', file=sys.stderr)
        sys.exit(1)
PY_VIDEO_DATA
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then main "$@"; fi
