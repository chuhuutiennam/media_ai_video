#!/usr/bin/env bash
# Standalone gesture stop; no shared shell helpers required.
set -Eeuo pipefail
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
ACTION=${0##*/}
SRC_DIR=$(cd -- "$SCRIPT_DIR/.." && pwd)
APP_DIR= VENV= MODEL= WHEELS_DIR=
PYTHON_BIN=${PYTHON:-python3}
DELEGATE=cpu ADDR=127.0.0.1:50053 METRICS_PORT=9102 MODE=runtime
VERBOSE=false
DRY_RUN=false OFFLINE=false FOREGROUND=false NO_RESTART=false FORCE=false
GRACE=10 TIMEOUT=60 SERVICE_LOG=

trap 'code=$?; emit ERROR "Command failed (exit=$code, line=$LINENO). Review the error above and logs under ${APP_DIR:-APP_DIR}/logs"; exit "$code"' ERR

emit() {
    local level=$1 line; shift
    line="[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] $level $*"
    if [[ "$level" == ERROR ]]; then printf '%s\n' "$line" >&2; else printf '%s\n' "$line"; fi
    if [[ "$DRY_RUN" == false && -n "$SERVICE_LOG" && -d "${SERVICE_LOG%/*}" ]]; then
        printf '%s\n' "$line" >> "$SERVICE_LOG" || printf '[WARN] Cannot append %s\n' "$SERVICE_LOG" >&2
    fi
}

info() { emit INFO "$@"; }

ok() { emit OK "$@"; }

warn() { emit WARN "$@"; }

detail() { if [[ "$VERBOSE" == true ]]; then emit INFO "$@"; fi; }

hint() {
    local label=$1 command; shift
    printf -v command '%q ' "$@"
    info "$label: ${command% }"
}

die() { emit ERROR "$@"; exit 1; }

usage() {
    cat <<HELP
Usage: bash $ACTION [options]
  --app-dir DIR     Deployment directory (source .app_dir or /u01/ims/mediapipe_gesture)
  --src-dir DIR     Gesture source tree (install/upgrade)
  --venv DIR        Override APP_DIR/.app_venv or APP_DIR/libs/venv
  --python FILE     Python 3.11+ for a new venv
  --model FILE      Checkpoint for a new deployment (existing model is preserved)
  --delegate MODE   cpu|gpu, new config only (default cpu)
  --addr HOST:PORT  gRPC bind for new config (default 127.0.0.1:50053)
  --metrics-port N  Metrics for new config; 0 disables HTTP (default 9102)
  --offline        Install only from wheelhouse; no index/build isolation
  --wheels-dir DIR Wheelhouse (default SRC_DIR/wheels)
  --verbose        Show config/model/venv paths and executed commands
  --dry-run        Print plan without installing, writing files or stopping processes
  --mode MODE      check.sh: install|upgrade|runtime|all (default runtime)
  --foreground     start/restart: exec worker for supervisor; no background PID
  --no-restart     upgrade: leave service stopped
  --grace SEC      Shutdown wait (positive integer, default 10)
  --timeout SEC    Startup wait (positive integer, default 60)
  --force          Allow SIGKILL after shutdown grace expires
  -h, --help
HELP
}

parse_args() {
    while (( $# )); do
        case "$1" in
            -h|--help) usage; exit 0 ;;
            --verbose) VERBOSE=true; shift ;;
            --dry-run) DRY_RUN=true; shift ;;
            --offline) OFFLINE=true; shift ;;
            --foreground) FOREGROUND=true; shift ;;
            --no-restart) NO_RESTART=true; shift ;;
            --force) FORCE=true; shift ;;
            --app-dir|--src-dir|--venv|--python|--model|--delegate|--addr|--metrics-port|--wheels-dir|--mode|--grace|--timeout)
                (( $# >= 2 )) && [[ -n "$2" && "$2" != --* ]] || die "Missing value for $1"
                case "$1" in
                    --app-dir) APP_DIR=$2 ;; --src-dir) SRC_DIR=$2 ;; --venv) VENV=$2 ;;
                    --python) PYTHON_BIN=$2 ;; --model) MODEL=$2 ;; --delegate) DELEGATE=$2 ;;
                    --addr) ADDR=$2 ;; --metrics-port) METRICS_PORT=$2 ;; --wheels-dir) WHEELS_DIR=$2 ;;
                    --mode) MODE=$2 ;; --grace) GRACE=$2 ;; --timeout) TIMEOUT=$2 ;;
                esac
                shift 2 ;;
            *) die "Unknown option: $1 (use --help)" ;;
        esac
    done
    [[ "$GRACE" =~ ^[1-9][0-9]{0,5}$ && "$TIMEOUT" =~ ^[1-9][0-9]{0,5}$ ]] || die '--grace/--timeout must be positive integer seconds'
    [[ "$DELEGATE" == cpu || "$DELEGATE" == gpu ]] || die '--delegate must be cpu or gpu'
    [[ "$MODE" =~ ^(install|upgrade|runtime|all)$ ]] || die 'Invalid --mode'
    SRC_DIR=$(realpath -m -- "$SRC_DIR")
    if [[ -z "$APP_DIR" ]]; then
        if [[ -f "$SCRIPT_DIR/../.app_venv" ]]; then APP_DIR=$(cd -- "$SCRIPT_DIR/.." && pwd)
        elif [[ -f "$SRC_DIR/.app_dir" ]]; then APP_DIR=$(read_marker "$SRC_DIR/.app_dir")
        else APP_DIR=/u01/ims/mediapipe_gesture; fi
    fi
    APP_DIR=$(realpath -m -- "$APP_DIR")
    if [[ -z "$VENV" ]]; then
        if [[ -f "$APP_DIR/.app_venv" ]]; then VENV=$(read_marker "$APP_DIR/.app_venv"); else VENV="$APP_DIR/libs/venv"; fi
    fi
    VENV=$(realpath -m -- "$VENV")
    WHEELS_DIR=$(realpath -m -- "${WHEELS_DIR:-$SRC_DIR/wheels}")
    CONFIG="$APP_DIR/etc/config.yaml"
    WORKER_BIN="$VENV/bin/mediapipe-gesture-server"
    PID_FILE="$APP_DIR/run/mediapipe-gesture.pid"
    START_FILE="$APP_DIR/run/mediapipe-gesture.start"
    ADDR_FILE="$APP_DIR/run/mediapipe-gesture.addr"
    STATE_FILE="$APP_DIR/run/mediapipe-gesture.json"
    STDOUT_LOG="$APP_DIR/logs/stdout.log"
    SERVICE_LOG="$APP_DIR/logs/service.log"
    detail "APP_DIR=$APP_DIR VENV=$VENV CONFIG=$CONFIG"
}

read_marker() {
    local value; IFS= read -r value < "$1" || [[ -n "$value" ]] || die "Empty marker $1; pass --app-dir/--venv explicitly"
    [[ "$value" == /* ]] || die "Marker $1 must contain an absolute path; pass --app-dir/--venv explicitly"
    printf '%s\n' "$value"
}

lock_service() {
    command -v flock >/dev/null || die 'flock is required (install util-linux)'
    mkdir -p "$APP_DIR/run"
    exec 9> "$APP_DIR/run/service.lock"
    flock -x 9
}

proc_token() {
    local record rest; local -a fields
    [[ "$1" =~ ^[0-9]+$ && -r "/proc/$1/stat" ]] || return 1
    IFS= read -r record < "/proc/$1/stat" || return 1
    rest=${record##*) }; read -r -a fields <<< "$rest"
    [[ "${fields[0]}" != Z && "${fields[0]}" != X ]] || return 1
    printf '%s\n' "${fields[19]}"
}

running() {
    local token; PID= SAVED_START= SAVED_ADDR=
    [[ -f "$PID_FILE" ]] || return 1
    IFS= read -r PID < "$PID_FILE" || [[ -n "$PID" ]] || return 1
    [[ "$PID" =~ ^[0-9]+$ && "$PID" -gt 1 ]] || die "Invalid PID metadata: $PID_FILE"
    if [[ -f "$START_FILE" && -f "$ADDR_FILE" ]]; then
        IFS= read -r SAVED_START < "$START_FILE"
        IFS= read -r SAVED_ADDR < "$ADDR_FILE"
    elif [[ -f "$STATE_FILE" ]]; then
        # Read-only compatibility with metadata from the former Python launcher.
        local legacy
        legacy=$("$PYTHON_BIN" - "$STATE_FILE" "$PID" <<'PY_STATE'
import json,sys
s=json.load(open(sys.argv[1]))
if s.get('pid') != int(sys.argv[2]) or not str(s.get('start_time','')).isdigit():
    raise SystemExit('Invalid legacy gesture process metadata')
print(s['start_time']); print(s['addr'])
PY_STATE
        ) || die "Cannot read legacy metadata: $STATE_FILE"
        { IFS= read -r SAVED_START; IFS= read -r SAVED_ADDR; } <<< "$legacy"
    else
        die "Missing start-time metadata for PID $PID; refusing to signal an unverified process"
    fi
    [[ "$SAVED_START" =~ ^[0-9]+$ ]] || die "Invalid start-time metadata for PID $PID"
    token=$(proc_token "$PID") || return 1
    [[ "$token" == "$SAVED_START" ]]
}

clear_state() { rm -f -- "$PID_FILE" "$START_FILE" "$ADDR_FILE" "$STATE_FILE"; }

stop_service() {
    if ! running; then clear_state; info 'Not running; stale metadata cleared if present'; return; fi
    local stopped_pid=$PID deadline=$((SECONDS + GRACE))
    info "Sending SIGTERM PID=$PID grace=${GRACE}s"
    kill -TERM "$PID" 2>/dev/null || ! running || die "Cannot signal PID $PID"
    while running; do
        if (( SECONDS >= deadline )); then
            [[ "$FORCE" == true ]] || die "Shutdown timeout PID=$PID; metadata retained. Use stop.sh --force if required"
            warn "Grace expired; sending SIGKILL PID=$PID"
            kill -KILL "$PID" 2>/dev/null || ! running || die "Cannot kill PID $PID"
            deadline=$((SECONDS + 5))
            while running && (( SECONDS < deadline )); do sleep .1; done
            if running; then die "PID $PID still present; metadata retained"; fi
            break
        fi
        sleep .1
    done
    clear_state; ok "Stopped PID=$stopped_pid"
}

# Execute the requested operation.
parse_args "$@"
if [[ "$DRY_RUN" == true ]]; then info "DRY-RUN: stop instance at $APP_DIR"; exit 0; fi
if [[ ! -d "$APP_DIR" ]]; then info 'Not running'; exit 0; fi
lock_service
stop_service
