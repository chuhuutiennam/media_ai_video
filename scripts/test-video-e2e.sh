#!/usr/bin/env bash
# Video lab E2E: Bash owns build, services, HTTP, RTP senders, polling and cleanup.
# Embedded Python only handles JSON, mask/metric validation and socket probes.
set -Eeuo pipefail
set +m  # Each setsid child keeps $! as its process-group ID.

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd -- "$SCRIPT_DIR/.." && pwd)
PYTHON_BIN=${PYTHON:-python3}
RUN_DIR="" FAILURE="" CALL_STARTED=0
AI_MODE=${AI_MODE:-mock} AI_TARGET=${AI_TARGET:-} MODEL=${MEDIAPIPE_MODEL:-}
MEDIAPIPE_DELEGATE=gpu
OUTPUT_VIDEO=0 BACKGROUND="" PCAP_TCORE=-1 PCAP_TACCESS=-1 IS_PCAP=0
BIN_DIR="" INPUT="" DURATION=4 WIDTH=320 HEIGHT=240 INFER_SIZE=256 EXPECT_PER_LEG=0
TEST_TIMEOUT=${TIMEOUT:-30} GW_PORT=${GW_PORT:-0} CALLBACK_PORT=${CALLBACK_PORT:-0}
declare -A PIDS=()

info() { printf '[INFO] %s\n' "$*"; }
fail() { FAILURE=$*; printf '[FAIL] %s\n' "$FAILURE" >&2; exit 1; }
usage() {
    cat <<'HELP'
Usage: bash scripts/test-video-e2e.sh [options]
  --ai-mode mock|external|mediapipe  AI mode (default: mock)
  --ai-target HOST:PORT             Existing AI; required in external mode
  --model FILE                     Local .tflite model for MediaPipe
  --mediapipe-delegate gpu|cpu      MediaPipe delegate (default: gpu)
  --bin-dir DIR                    Use prebuilt binaries; otherwise build gateway and mock tools
  --input FILE                     Image/video or PCAP/PCAPNG (auto-detect); default testsrc2
  --pcap-tcore-stream N            Capture stream index for tCore (default: first H264)
  --pcap-taccess-stream N          Capture stream index for tAccess (default: second, or first if only one)
  --output-video                   Save source/mask/composited PNGs and output MP4 for both legs
  --background FILE                Replacement JPEG/PNG; default black; requires --output-video
  --duration SECONDS               Duration per leg, 1..300 (default: 4)
  --width N --height N             Even H264 dimensions (default: 320x240)
  --infer-size N                   JPEG/mask dimensions (default: 256)
  --expect-per-leg N               Minimum masks per leg; 0 calculates from duration
  --timeout SECONDS                Service/drain/cleanup timeout (default: 30)
  --gw-port N --callback-port N    HTTP ports; 0 selects free ports
  -h, --help                       Show this help
Environment: AI_MODE, AI_TARGET, MEDIAPIPE_MODEL, TIMEOUT, GW_PORT, CALLBACK_PORT, PYTHON.
See scripts/lab-test-setup.md for build/copy instructions and PASS criteria.
HELP
}

start() {
    local name=$1; shift
    setsid -- "$@" >"$RUN_DIR/$name.log" 2>&1 &
    PIDS[$name]=$!
    printf '%s\t%s\n' "$name" "$!" >> "$RUN_DIR/processes.tsv"
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
    stop mf-tcore
    stop mf-taccess
    stop encode-tcore
    stop encode-taccess
    stop tcore
    stop taccess
    if (( CALL_STARTED )); then
        curl --silent --noproxy '*' --max-time 3 -X DELETE "$CALL_URL" > /dev/null 2>&1
    fi
    stop gateway
    stop callback
    stop ai
    if (( code != 0 )) && [[ -n "$RUN_DIR" ]]; then
        video_data failure "${FAILURE:-command failed}" || true
        printf 'RESULT: FAIL — %s\nInspect %s\n' "${FAILURE:-command failed}" "$RUN_DIR"
    fi
    exit "$code"
}

check_services() {
    local name
    for name in ai gateway callback; do
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
        (( code == 10 )) || fail "$label failed; inspect $RUN_DIR"
        (( SECONDS < deadline )) || fail "timeout after ${seconds}s: $label"
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
    curl --silent --fail --noproxy '*' --max-time 1 "$GW_BASE/health/live" > /dev/null || return 10
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

verify_snapshot() {
    local phase=$1 filename=$2
    http_request GET "$GW_BASE/metrics" "$RUN_DIR/$filename" 200 || return 1
    video_data "$phase"
}

main() {
    while (( $# )); do
        case "$1" in
            -h|--help) usage; return 0 ;;
            --output-video) OUTPUT_VIDEO=1; shift ;;
            --ai-mode|--ai-target|--model|--mediapipe-delegate|--bin-dir|--input|--pcap-tcore-stream|--pcap-taccess-stream|--background|--duration|--width|--height|--infer-size|--expect-per-leg|--timeout|--gw-port|--callback-port)
                (( $# >= 2 )) && [[ "$2" != --* ]] || fail "missing value for $1"
                case "$1" in
                    --ai-mode) AI_MODE=$2 ;; --ai-target) AI_TARGET=$2 ;; --model) MODEL=$2 ;;
                    --mediapipe-delegate) MEDIAPIPE_DELEGATE=$2 ;;
                    --pcap-tcore-stream) PCAP_TCORE=$2 ;; --pcap-taccess-stream) PCAP_TACCESS=$2 ;;
                    --background) BACKGROUND=$2 ;;
                    --bin-dir) BIN_DIR=$2 ;; --input) INPUT=$2 ;; --duration) DURATION=$2 ;;
                    --width) WIDTH=$2 ;; --height) HEIGHT=$2 ;; --infer-size) INFER_SIZE=$2 ;;
                    --expect-per-leg) EXPECT_PER_LEG=$2 ;; --timeout) TEST_TIMEOUT=$2 ;;
                    --gw-port) GW_PORT=$2 ;; --callback-port) CALLBACK_PORT=$2 ;;
                esac
                shift 2 ;;
            *) fail "unknown option: $1" ;;
        esac
    done
    [[ "$MEDIAPIPE_DELEGATE" == gpu || "$MEDIAPIPE_DELEGATE" == cpu ]] || fail 'MediaPipe delegate must be gpu or cpu'
    [[ -z "$BACKGROUND" || "$OUTPUT_VIDEO" == 1 ]] || fail '--background requires --output-video'
    command -v "$PYTHON_BIN" > /dev/null || fail "Python 3 is required"
    RUN_DIR=$(mktemp -d ./tmp/runs/video-e2e-XXXXXXXX)
    trap cleanup EXIT
    trap 'FAILURE=${FAILURE:-"command failed at line $LINENO"}' ERR
    trap 'FAILURE="interrupted"; exit 1' INT TERM
    info "Artifacts: $RUN_DIR"
    info 'Step 1 — Preflight'
    video_data init "$AI_MODE" "$AI_TARGET" "$MODEL" "$BIN_DIR" "$INPUT" "$DURATION" "$WIDTH" "$HEIGHT" \
        "$INFER_SIZE" "$EXPECT_PER_LEG" "$TEST_TIMEOUT" "$GW_PORT" "$CALLBACK_PORT"
    read -r GW_PORT CALLBACK_PORT EXPECT_PER_LEG TIMEOUT_SECONDS SENDER_TIMEOUT DURATION WIDTH HEIGHT < "$RUN_DIR/runtime.txt"
    AI_TARGET=$(video_data get options.json ai_target)
    INPUT=$(video_data get options.json input)
    IS_PCAP=$(video_data get options.json pcap)
    MODEL=$(video_data get options.json model)
    BACKGROUND=$(video_data background "$BACKGROUND")
    cd "$ROOT"
    GW_BASE="http://127.0.0.1:$GW_PORT"
    CALL_URL="$GW_BASE/v1/vonras/call-sessions/${RUN_DIR##*/}"
    local tool name
    for tool in curl setsid ffmpeg; do command -v "$tool" > /dev/null || fail "$tool is required"; done
    ffmpeg -hide_banner -encoders > "$RUN_DIR/ffmpeg-encoders.txt" 2>&1
    grep -q libx264 "$RUN_DIR/ffmpeg-encoders.txt" || fail 'FFmpeg must include libx264'
    local names=(media-ai-gateway mock-callback-server mock-video-rtp-sender)
    [[ "$AI_MODE" != mock ]] || names+=(mock-ai-worker)
    if (( OUTPUT_VIDEO )); then
        [[ -f "$ROOT/mock_mf/mock.py" ]] || fail "missing Python MF mock: $ROOT/mock_mf/mock.py"
        "$PYTHON_BIN" -c 'from PIL import Image' || fail 'Python MF mock requires Pillow: python3 -m pip install Pillow'
    fi
    if [[ -n "$BIN_DIR" ]]; then
        BIN_DIR=$(video_data get options.json bin_dir)
        info "Step 2 — Use binaries from $BIN_DIR"
        for name in "${names[@]}"; do [[ -x "$BIN_DIR/$name" ]] || fail "missing executable: $BIN_DIR/$name"; done
    else
        for tool in go pkg-config "${CXX:-c++}" "${CC:-cc}"; do
            command -v "$tool" > /dev/null || fail "$tool is required to build; alternatively use --bin-dir"
        done
        [[ "${CGO_ENABLED:-1}" == 1 ]] || fail 'Video gateway requires CGO_ENABLED=1'
        pkg-config --exists libavcodec libavutil libswscale
        BIN_DIR="$RUN_DIR/bin"
        mkdir -p "$BIN_DIR"
        info 'Step 2 — Build gateway and mock tools'
        for name in "${names[@]}"; do
            start build env CGO_ENABLED=1 go build -o "$BIN_DIR/$name" "./cmd/$name"
            if ! wait "${PIDS[build]}"; then cat "$RUN_DIR/build.log" >&2; fail "build $name failed"; fi
            unset 'PIDS[build]'
        done
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

    info 'Step 3 — Start AI and gateway'
    case "$AI_MODE" in
        mock) start ai "$BIN_DIR/mock-ai-worker" --addr "$AI_TARGET" --log-level debug ;;
        mediapipe) start ai "$PYTHON_BIN" "$ROOT/workers/mediapipe/worker.py" --addr "$AI_TARGET" --model "$MODEL" --delegate "$MEDIAPIPE_DELEGATE" ;;
    esac
    wait_for 'AI TCP ready' "$TIMEOUT_SECONDS" video_data tcp "$AI_TARGET"
    start gateway "$BIN_DIR/media-ai-gateway" --config "$RUN_DIR/gateway.yaml"
    wait_for 'gateway ready' "$TIMEOUT_SECONDS" health_ready

    info 'Step 4 — Metrics baseline'
    verify_snapshot baseline metrics-before.prom

    info 'Step 5 — Start callback daemon; POST ANSWER then ctrl-result directly'
    start callback "$BIN_DIR/mock-callback-server" --port "$CALLBACK_PORT" --expect-final 0
    wait_for 'callback ready' "$TIMEOUT_SECONDS" video_data callback-ready
    CALL_STARTED=1
    http_request POST "$CALL_URL/notify-event" "$RUN_DIR/answer.json" 201 "$RUN_DIR/notify.json"
    http_request POST "$CALL_URL/ctrl-result" "$RUN_DIR/ctrl-response.json" 200 "$RUN_DIR/ctrl-result.json"
    video_data signaling
    verify_snapshot active metrics-active.prom

    info "Step 6 — Send H264 RTP to both legs (${DURATION}s)"
    local tcore_port taccess_port
    read -r tcore_port taccess_port < "$RUN_DIR/video-ports.txt"
    local sender_args=(--width "$WIDTH" --height "$HEIGHT" --fps 10 --duration "${DURATION}s")
    [[ -z "$INPUT" ]] || sender_args+=(--input "$INPUT")
    local core_args=() access_args=()
    if [[ "$IS_PCAP" == 1 ]]; then
        core_args+=(--pcap-stream "$PCAP_TCORE")
        access_args+=(--pcap-stream "$PCAP_TACCESS")
    fi
    if (( OUTPUT_VIDEO )); then
        core_args+=(--frames-dir "$RUN_DIR/output/tcore")
        access_args+=(--frames-dir "$RUN_DIR/output/taccess")
    fi
    start tcore "$BIN_DIR/mock-video-rtp-sender" --target "127.0.0.1:$tcore_port" --ssrc 88001 "${sender_args[@]}" "${core_args[@]}"
    start taccess "$BIN_DIR/mock-video-rtp-sender" --target "127.0.0.1:$taccess_port" --ssrc 88002 "${sender_args[@]}" "${access_args[@]}"
    wait_for 'video senders' "$SENDER_TIMEOUT" senders_done
    video_data senders

    info "Step 7 — Verify masks and drain all frames (minimum $EXPECT_PER_LEG/leg)"
    wait_for 'all packets, JPEG frames, AI results and callbacks' "$TIMEOUT_SECONDS" verify_snapshot drained metrics-after.prom
    info 'Step 8 — Metrics OK; zero errors/drops'
    info 'Step 9 — DELETE; wait for sessions, pipelines, AI streams and RTP ports to be released'
    http_request DELETE "$CALL_URL" "$RUN_DIR/delete-response.txt" 204
    CALL_STARTED=0
    wait_for 'session and RTP port cleanup' "$TIMEOUT_SECONDS" verify_snapshot released metrics-cleanup.prom
    if (( OUTPUT_VIDEO )); then
        info 'Step 10 — MF matches callback masks to source RTP timestamps; export video'
        local leg dir mf_name encode_name
        for leg in tCore tAccess; do
            dir="$RUN_DIR/output/${leg,,}"
            mf_name="mf-${leg,,}"
            encode_name="encode-${leg,,}"
            start "$mf_name" "$PYTHON_BIN" "$ROOT/mock_mf/mock.py" --callbacks "$RUN_DIR/callbacks.json" \
                --frames-dir "$dir" --leg "$leg" --background "$BACKGROUND"
            if ! wait "${PIDS[$mf_name]}"; then cat "$RUN_DIR/$mf_name.log" >&2; fail "mf $leg failed"; fi
            unset 'PIDS[$mf_name]'
            start "$encode_name" ffmpeg -hide_banner -loglevel error -nostdin -y -f concat -safe 0 -i "$dir/output.ffconcat" \
                -vf 'pad=ceil(iw/2)*2:ceil(ih/2)*2' -vsync vfr -an -c:v libx264 -threads 1 -pix_fmt yuv420p \
                -movflags +faststart "$dir/output.mp4"
            if ! wait "${PIDS[$encode_name]}"; then cat "$RUN_DIR/$encode_name.log" >&2; fail "encode $leg failed"; fi
            unset 'PIDS[$encode_name]'
            info "Video $leg: $dir/output.mp4"
        done
        video_data output "$BACKGROUND"
    fi
    video_data summary
}

# Stateless data operations. No process launching, HTTP requests or polling here.
video_data() {
    "$PYTHON_BIN" - "$RUN_DIR" "$@" <<'PY_VIDEO_DATA'
import json
import math
import socket
import sys
from pathlib import Path

VIDEO = "media_ai_video_"


ERROR_METRICS = (
    VIDEO + "pool_dropped_total", VIDEO + "frames_dropped_total", VIDEO + "decode_errors_total",
    VIDEO + "ai_send_errors_total", VIDEO + "ai_recv_errors_total", VIDEO + "ai_result_errors_total",
    "media_ai_dispatcher_send_errors_total", "media_ai_result_queue_dropped_total",
)


def require(condition, message):
    # Explicit checks also run when PYTHONOPTIMIZE is set on the lab machine.
    if not condition:
        raise RuntimeError(message)


def free_port(kind=socket.SOCK_STREAM):
    with socket.socket(type=kind) as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def udp_range():
    for _ in range(100):
        start = free_port(socket.SOCK_DGRAM)
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


def check_errors(current, baseline):
    for key in ERROR_METRICS:
        require(delta(current, baseline, key) == 0, f"{key} increased by {delta(current, baseline, key)}")


def callback_log(path):
    """Read complete JSON lines; a concurrent write may leave the last line partial."""
    if not path.exists():
        return []
    events = []
    for line in path.read_bytes().splitlines(keepends=True):
        if not line.endswith(b"\n"):
            break
        event = json.loads(line)
        require(event.get("event") not in ("parse_error", "error", "timeout"),
                f"callback server error: {event}")
        events.append(event)
    return events


def validate(state, call_id, infer_size, minimum, duration):
    require(state["answer_status"] == 201, "notify-event ANSWER failed")
    require(state["ctrl_result_status"] == 200, "ctrl-result failed")
    answer = state["answer"]
    require(answer["rtp_ip"] == "127.0.0.1", "lab RTP IP must be loopback")
    for key in ("codec", "sample_rate", "tcore_rtp_port", "taccess_rtp_port",
                "tcore_local_non_dc_media", "taccess_local_non_dc_media"):
        require(not answer.get(key), f"video-only call unexpectedly allocated audio: {key}")
    ports = [answer["tcore_video_rtp_port"], answer["taccess_video_rtp_port"]]
    require(all(0 < p < 65536 for p in ports) and ports[0] != ports[1], "invalid video RTP ports")
    for leg, port in zip(("tcore", "taccess"), ports):
        sdp = answer[f"{leg}_local_non_dc_video_media"]
        require(sdp["sdpmLine"] == f"video {port} RTP/AVP 96", f"invalid {leg} video SDP")
        require("rtpmap:96 H264/90000" in sdp["sdpaLines"], f"invalid {leg} codec/clock")
    frames = {"tCore": {}, "tAccess": {}}
    for body in state["callbacks"]:
        path = body.get("path")
        require(path in ("/results/tCore", "/results/tAccess"), f"unexpected callback URL path: {path}")
        leg = path.rsplit("/", 1)[1]
        require(body["protocol"] == "HTTP/2.0", f"{leg}: callback must use HTTP/2")
        require(body["event_type"] == "video.mask.result" and body["is_final"] is True, f"{leg}: invalid event/final")
        require(body["kind"] == "video_mask", f"{leg}: invalid result kind")
        require(body["session_id"] == f"{call_id}-{leg.lower()}", f"{leg}: wrong session")
        require(body["stream_id"] == body["session_id"], f"{leg}: wrong stream")
        require(body["mediaResources"] == {leg: {"contextId": f"ctx-{leg}", "terminationId": f"term-{leg}"}},
                f"{leg}: wrong callback termination")
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
    complete = True
    for leg, masks in frames.items():
        ids = sorted(masks)
        if len(ids) < minimum or ids != list(range(1, len(ids) + 1)):
            complete = False
            continue
        # Wrap-safe RTP coverage rejects streams that only work at the beginning.
        span = ((masks[ids[-1]]["rtp_timestamp"] - masks[ids[0]]["rtp_timestamp"]) & 0xFFFFFFFF) / 90000
        if span < duration * 0.75:
            complete = False
    return complete, {leg: len(masks) for leg, masks in frames.items()}


def read_metrics(path):
    return {parts[0]: float(parts[1]) for line in path.read_text().splitlines()
            if line and not line.startswith('#') and len(parts := line.split()) == 2}


def data_main(folder, action, *args):
    run = Path(folder)
    def save(name, value):
        (run / name).write_text(json.dumps(value, indent=2))
    def load(name):
        return json.loads((run / name).read_text())
    if action == 'failure':
        save('result.json', {'result': 'FAIL', 'error': args[0]})
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
        duration, width, height, size, minimum, timeout, gw_port, cb_port = (
            convert(value) for convert, value in zip((float, int, int, int, int, float, int, int), args[5:]))
        require(mode in ('mock', 'external', 'mediapipe'), 'invalid --ai-mode')
        require(math.isfinite(duration) and 1 <= duration <= 300, 'duration must be in 1..300 seconds')
        require(math.isfinite(timeout) and timeout > 0 and minimum >= 0, 'timeout must be positive; expect-per-leg must be nonnegative')
        require(0 < width <= 1920 and 0 < height <= 1080 and width % 2 == height % 2 == 0, 'positive even dimensions, at most 1920x1080 required')
        require(1 <= size <= 1024, 'infer-size must be in 1..1024')
        require(all(0 <= p < 65536 for p in (gw_port, cb_port)), 'TCP ports must be in 0..65535')
        require(mode != 'external' or target, 'external mode requires --ai-target')
        require(mode != 'mediapipe' or Path(model).is_file(), 'mediapipe requires --model pointing to a local file')
        require(not input_file or Path(input_file).is_file(), f'input file not found: {input_file}')
        capture = False
        if input_file:
            with open(input_file, 'rb') as source:
                capture = source.read(4) in (bytes.fromhex(magic) for magic in
                          ('d4c3b2a1', 'a1b2c3d4', '4d3cb2a1', 'a1b23c4d', '0a0d0d0a'))
        target = target or f'127.0.0.1:{free_port()}'
        gw_port = gw_port or free_port()
        start, end = udp_range()
        minimum = minimum or max(2, int(duration * 5 * 0.6))
        save('options.json', dict(mode=mode, ai_target=target, bin_dir=str(Path(binary_dir).resolve()),
             input=str(Path(input_file).resolve()) if input_file else '', model=str(Path(model).resolve()) if model else '',
             duration=duration, infer_size=size, minimum=minimum, minimum_auto=int(args[9]) == 0, pcap=int(capture)))
        (run / 'runtime.txt').write_text(f'{gw_port} {cb_port} {minimum} {math.ceil(timeout)} {math.ceil(duration + 65)} {duration} {width} {height}\n')
        save('gateway.yaml', {
            'gateway': {'id': 'gw-video-test', 'raw_rtp_enabled': True, 'webrtc_enabled': False, 'mock_result_pump': False},
            'server': {'http_addr': f'127.0.0.1:{gw_port}', 'public_url': f'http://127.0.0.1:{gw_port}', 'shutdown_timeout_sec': 2},
            'rtp': {'listen_addr': '127.0.0.1:0', 'bind_ip': '127.0.0.1', 'public_ip': '127.0.0.1', 'port_start': start, 'port_end': end},
            'session': {'max_sessions': 10, 'per_session_packet_queue': 1024, 'idle_timeout_sec': int(duration + timeout + 60)},
            'pipeline': {'jitter_out_queue_size': 1024}, 'ai': {'grpc_target': ''},
            'video': {'enabled': True, 'worker_count': 2, 'job_queue_size': 1024, 'infer_width': size,
                      'infer_height': size, 'infer_fps': 5, 'frame_queue_size': 8, 'default_effect_type': 'bg_replace',
                      'ai': {'grpc_target': target, 'max_retry': 0, 'stream_timeout_sec': int(duration + timeout + 120)}},
            'log': {'level': 'debug'},
        })
        save('notify.json', {'callId': run.name, 'event': 'ANSWER', 'selectedService': 'change_background'})
        return 0
    if action == 'tcp':
        host, port = args[0].rsplit(':', 1)
        try:
            with socket.create_connection((host, int(port)), timeout=0.2):
                return 0
        except OSError:
            return 10
    if action == 'callback-ready':
        ready = next((event for event in callback_log(run / 'callback.log') if event.get('event') == 'ready'), None)
        if ready is None:
            return 10
        require(0 < ready['port'] < 65536, 'callback server must report its actual port; rebuild the binary')
        save('ctrl-result.json', {'callId': run.name, 'mediaResources': {
            leg: {'contextId': f'ctx-{leg}', 'termination': {'terminationId': f'term-{leg}'},
                  'callbackUrl': f"http://127.0.0.1:{ready['port']}/results/{leg}"} for leg in ('tCore', 'tAccess')}})
        return 0
    options = load('options.json')
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
                options['minimum'] = max(2, int(options['duration'] * 5 * 0.6))
            options['pcap_streams'] = {'tCore': core, 'tAccess': access}
            save('options.json', options)
        else:
            require(core == access == -1, 'PCAP stream selection requires a PCAP/PCAPNG input')
        (run / 'input-runtime.txt').write_text(f"{int(capture['pcap'])} {core} {access} {options['duration']} {options['minimum']}\n")
        return 0
    if action == 'signaling':
        state = dict(notify_event=load('notify.json'), answer_status=201, answer=load('answer.json'),
                     ctrl_result=load('ctrl-result.json'), ctrl_result_status=200, callbacks=[])
        validate(state, run.name, options['infer_size'], options['minimum'], options['duration'])
        save('signaling.json', state)
        (run / 'video-ports.txt').write_text(f"{state['answer']['tcore_video_rtp_port']} {state['answer']['taccess_video_rtp_port']}\n")
        return 0
    if action == 'senders':
        save('senders.json', {leg: json.loads((run / f'{leg}.log').read_text().strip().splitlines()[-1]) for leg in ('tcore', 'taccess')})
        return 0
    if action == 'summary':
        result = load('result.json')
        print(f"RESULT: PASS ({result['mode']}) — {result['packets']} packets; masks {result['masks_per_leg']}; metrics and RTP cleanup OK")
        return 0
    if action == 'output':
        result = load('result.json')
        result['output_videos'] = {leg: str(run / 'output' / leg.lower() / 'output.mp4') for leg in ('tCore', 'tAccess')}
        result['background'] = args[0] or 'black'
        for path in result['output_videos'].values():
            require(Path(path).is_file() and Path(path).stat().st_size > 0, f'output video missing: {path}')
        save('result.json', result)
        return 0
    baseline = read_metrics(run / 'metrics-before.prom')
    if action == 'baseline':
        check_errors(baseline, baseline)
        require(baseline['media_ai_sessions_active'] == 0, 'expected an isolated gateway')
        return 0
    filenames = {'active': 'metrics-active.prom', 'drained': 'metrics-after.prom', 'released': 'metrics-cleanup.prom'}
    require(action in filenames, f'unknown data operation: {action}')
    current = read_metrics(run / filenames[action])
    check_errors(current, baseline)
    if action == 'active':
        require(current['media_ai_rtp_ports_available'] == baseline['media_ai_rtp_ports_available'] - 2, 'expected exactly 2 video ports')
        require(current['media_ai_ai_streams_active'] == 0, 'unexpected audio AI stream')
        require(current[VIDEO + 'ai_streams_active'] == 2, 'expected 2 video AI streams')
        return 0
    counters = (VIDEO + 'pool_processed_total', VIDEO + 'frames_total', VIDEO + 'ai_frames_sent_total',
                VIDEO + 'ai_results_received_total', 'media_ai_dispatcher_sent_total')
    if action == 'drained':
        state = load('signaling.json')
        state['callbacks'] = [event for event in callback_log(run / 'callback.log') if event.get('event') == 'callback']
        save('callbacks.json', state)
        valid, counts = validate(state, run.name, options['infer_size'], options['minimum'], options['duration'])
        packets = sum(sender['packets'] for sender in load('senders.json').values())
        amounts = {key: delta(current, baseline, key) for key in counters}
        save('progress.json', {'packets_sent': packets, 'masks': counts, **amounts})
        complete = (valid and amounts[counters[0]] == packets and all(amounts[key] == sum(counts.values()) for key in counters[1:])
                    and delta(current, baseline, VIDEO + 'pool_submitted_total') == packets
                    and current[VIDEO + 'pool_queue_len'] == current['media_ai_dispatcher_queue_len'] == 0)
        return 0 if complete else 10
    complete = (current['media_ai_sessions_active'] == baseline['media_ai_sessions_active']
                and current['media_ai_rtp_ports_available'] == baseline['media_ai_rtp_ports_available']
                and current[VIDEO + 'pool_sessions_active'] == current[VIDEO + 'ai_streams_active'] == 0)
    if not complete:
        return 10
    after = read_metrics(run / 'metrics-after.prom')
    for key in counters:
        require(current[key] == after[key], f'counter changed after drain/DELETE: {key}')
    progress = load('progress.json')
    save('result.json', dict(result='PASS', mode=options['mode'], call_id=run.name, packets=progress['packets_sent'],
         masks_per_leg=progress['masks'], duration_sec=options['duration'], min_masks_per_leg=options['minimum'],
         ports_available=current['media_ai_rtp_ports_available']))
    return 0


if __name__ == '__main__':
    try:
        sys.exit(data_main(*sys.argv[1:]))
    except (OSError, ValueError, KeyError, TypeError, RuntimeError) as exc:
        print(f'[FAIL] {exc}', file=sys.stderr)
        sys.exit(1)
PY_VIDEO_DATA
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then main "$@"; fi
