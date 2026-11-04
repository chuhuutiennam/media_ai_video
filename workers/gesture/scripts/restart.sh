#!/usr/bin/env bash
# Standalone gesture restart; no shared shell helpers required.
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

service_hints() {
    hint 'Status' bash "$APP_DIR/scripts/status.sh"
    hint 'Follow log' tail -f "$STDOUT_LOG"
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

read_runtime_config() {
    info "Checking configuration, model and dependencies ..."
    [[ -x "$VENV/bin/python" && -x "$WORKER_BIN" ]] || die "Worker/venv missing at $VENV; run install.sh first"
    [[ -f "$CONFIG" ]] || die "Config missing: $CONFIG"
    local data
    data=$("$VENV/bin/python" - "$CONFIG" "$APP_DIR" <<'PY_CONFIG'
import importlib.util,sys
from pathlib import Path
from mediapipe_gesture.config import load_config
cfg=load_config(Path(sys.argv[1]))
for name in ('grpc','grpc_tools','google.protobuf','mediapipe','cv2','numpy','prometheus_client'):
    if importlib.util.find_spec(name) is None: raise SystemExit('Missing dependency: '+name)
if not cfg.gesture.enabled or not cfg.gesture.model or not cfg.gesture.model.is_file():
    raise SystemExit('Set gesture.enabled=true and gesture.model to an existing .task checkpoint in '+sys.argv[1])
host,port=cfg.server.addr.rsplit(':',1)
allowed='abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.-:%[]'
if not host or any(c not in allowed for c in host) or not 1<=int(port)<=65535: raise SystemExit('Invalid server.addr')
m=cfg.metrics.port if cfg.metrics.enabled else 0
if type(m) is not int or not 0<=m<=65535 or m==int(port): raise SystemExit('Invalid or conflicting metrics port')
if type(cfg.server.max_streams) is not int or cfg.server.max_streams<1: raise SystemExit('server.max_streams must be positive')
f=getattr(cfg.logging,'file',None)
f=str((Path(sys.argv[2])/f).absolute()) if f else 'disabled (stdout only)'
values=(cfg.server.addr,m,cfg.gesture.model,cfg.gesture.delegate,cfg.server.max_streams,
        cfg.gesture.max_concurrent_recognizers,f)
for value in values:
    if '\n' in str(value) or '\r' in str(value): raise SystemExit('Config values cannot contain newlines')
    print(value)
PY_CONFIG
    ) || die "Runtime check failed; review $CONFIG and install package dependencies"
    { IFS= read -r RUNTIME_ADDR; IFS= read -r RUNTIME_METRICS; IFS= read -r RUNTIME_MODEL;
      IFS= read -r RUNTIME_DELEGATE; IFS= read -r RUNTIME_RPC_LIMIT; IFS= read -r RUNTIME_RECOGNIZERS;
      IFS= read -r JSON_LOG; } <<< "$data"
    ok "Configuration valid: grpc=$RUNTIME_ADDR delegate=$RUNTIME_DELEGATE"
    detail "Model=$RUNTIME_MODEL metrics=$RUNTIME_METRICS RPC=$RUNTIME_RPC_LIMIT recognizers=$RUNTIME_RECOGNIZERS"
    detail "JSON log=$JSON_LOG"
}

port_probe() {
    if [[ "$1" == ready ]]; then
        local host=${2%:*} port=${2##*:}
        host=${host#[}; host=${host%]}
        case "$host" in 0.0.0.0) host=127.0.0.1 ;; ::) host=::1 ;; esac
        timeout 1 bash -c 'exec 3<>"/dev/tcp/$1/$2"' -- "$host" "$port" 2>/dev/null
        return $?
    fi
    "$VENV/bin/python" - "$1" "$2" <<'PY_PORT'
import socket,sys
host,port=sys.argv[2].rsplit(':',1); host=host.strip('[]'); port=int(port)
if sys.argv[1]=='free':
    family,kind,proto,_,address=socket.getaddrinfo(host,port,type=socket.SOCK_STREAM)[0]
    try:
        with socket.socket(family,kind,proto) as s:
            s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
            s.bind(address)
    except OSError as exc: raise SystemExit('Port unavailable '+sys.argv[2]+': '+str(exc))
else:
    host={'0.0.0.0':'127.0.0.1','::':'::1'}.get(host,host)
    try:
        with socket.create_connection((host,port),timeout=.2): pass
    except OSError: sys.exit(1)
PY_PORT
}

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

start_service() {
    if running; then
        port_probe ready "$SAVED_ADDR" || die "PID=$PID exists but endpoint $SAVED_ADDR is not ready; inspect $STDOUT_LOG"
        ok "Already running PID=$PID grpc=$SAVED_ADDR"; service_hints; return
    fi
    clear_state
    read_runtime_config
    info "Checking available ports ..."
    port_probe free "$RUNTIME_ADDR" || die "gRPC port occupied: $RUNTIME_ADDR"
    if (( RUNTIME_METRICS > 0 )); then port_probe free "0.0.0.0:$RUNTIME_METRICS" || die "Metrics port occupied: $RUNTIME_METRICS"; fi
    mkdir -p "$APP_DIR/logs" "$APP_DIR/run/matplotlib"
    export MPLCONFIGDIR=${MPLCONFIGDIR:-$APP_DIR/run/matplotlib}
    export PYTHONUNBUFFERED=1
    info "Starting gesture worker ..."
    detail "Config=$CONFIG executable=$WORKER_BIN"
    detail "Logs: stdout=$STDOUT_LOG json=$JSON_LOG service=$SERVICE_LOG"
    if [[ "$FOREGROUND" == true ]]; then
        info 'Foreground: supervisor owns lifecycle (no background PID metadata)'
        flock -u 9; exec 9>&-
        cd -- "$APP_DIR"
        exec "$WORKER_BIN" --config "$CONFIG"
    fi
    (cd -- "$APP_DIR"; exec nohup "$WORKER_BIN" --config "$CONFIG" </dev/null >> "$STDOUT_LOG" 2>&1 9>&-) &
    local child=$! token deadline=$((SECONDS + TIMEOUT)) next_progress=$((SECONDS + 5))
    info "Waiting for gRPC at $RUNTIME_ADDR (timeout ${TIMEOUT}s) ..."
    token=$(proc_token "$child") || { wait "$child" || true; die "Worker exited during startup; see $STDOUT_LOG"; }
    # Publish PID last; all companion metadata is present before another reader sees it.
    if ! { printf '%s\n' "$token" > "$START_FILE" &&
           printf '%s\n' "$RUNTIME_ADDR" > "$ADDR_FILE" &&
           printf '{"pid":%s,"start_time":"%s","addr":"%s"}\n' "$child" "$token" "$RUNTIME_ADDR" > "$STATE_FILE" &&
           printf '%s\n' "$child" > "$PID_FILE.tmp" && mv -f -- "$PID_FILE.tmp" "$PID_FILE"; }; then
        kill -TERM "$child" 2>/dev/null || true
        sleep .1; kill -KILL "$child" 2>/dev/null || true
        wait "$child" 2>/dev/null || true
        clear_state
        die "Cannot write process metadata in $APP_DIR/run; startup process stopped"
    fi
    while running; do
        if port_probe ready "$RUNTIME_ADDR"; then
            ok "Ready PID=$child grpc=$RUNTIME_ADDR"
            if (( RUNTIME_METRICS > 0 )); then info "Metrics: port $RUNTIME_METRICS (/metrics)"; fi
            service_hints
            return
        fi
        if (( SECONDS >= deadline )); then
            warn "Startup timeout after ${TIMEOUT}s; stopping PID=$child"
            local FORCE=true GRACE=5; stop_service
            tail -n 20 "$STDOUT_LOG" >&2 || true
            die "Startup failed; inspect $STDOUT_LOG and $JSON_LOG"
        fi
        if (( SECONDS >= next_progress )); then
            info "Still initializing model; waiting for gRPC (${TIMEOUT}s timeout) ..."
            next_progress=$((SECONDS + 5))
        fi
        sleep .2
    done
    local code=0; wait "$child" || code=$?
    clear_state; tail -n 20 "$STDOUT_LOG" >&2 || true
    die "Worker exited before readiness (exit=$code); inspect config/model/delegate and logs"
}

# Execute the requested operation.
parse_args "$@"
if [[ "$DRY_RUN" == true ]]; then info "DRY-RUN: stop/start instance at $APP_DIR"; exit 0; fi
lock_service
stop_service
start_service
