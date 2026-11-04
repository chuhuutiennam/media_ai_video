#!/usr/bin/env bash
# Standalone gesture install; no shared shell helpers required.
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

run() {
    local quoted; printf -v quoted '%q ' "$@"
    if [[ "$DRY_RUN" == true ]]; then info "DRY-RUN: $quoted"; else detail "Run: $quoted"; "$@"; fi
}

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

find_model() {
    if [[ -z "$MODEL" ]]; then
        if [[ -f "$SRC_DIR/../checkpoint/gesture_recognizer.task" ]]; then MODEL="$SRC_DIR/../checkpoint/gesture_recognizer.task"
        else MODEL="$SRC_DIR/checkpoint/gesture_recognizer.task"; fi
    fi
    MODEL=$(realpath -m -- "$MODEL")
    [[ -s "$MODEL" && -r "$MODEL" ]] || die "Checkpoint missing/unreadable: $MODEL; use --model FILE"
}

check_source() {
    info "Checking system, Python and checkpoint ..."
    detail "OS=$(uname -s) architecture=$(uname -m) source=$SRC_DIR"
    local name
    for name in realpath flock nohup timeout; do command -v "$name" >/dev/null || die "Required command missing: $name"; done
    command -v "$PYTHON_BIN" >/dev/null || die "Python not found: $PYTHON_BIN; use --python python3.12"
    "$PYTHON_BIN" - "$ADDR" "$METRICS_PORT" <<'PY_ARGS' || die 'Invalid install options; Python 3.11+ and distinct valid ports are required'
import sys
if sys.version_info < (3,11): raise SystemExit('Python 3.11+ required')
host,port=sys.argv[1].rsplit(':',1)
allowed='abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.-:%[]'
if not host or any(c not in allowed for c in host) or not 1<=int(port)<=65535: raise SystemExit('Invalid --addr')
if not 0<=int(sys.argv[2])<=65535 or int(port)==int(sys.argv[2]): raise SystemExit('Invalid/conflicting --metrics-port')
PY_ARGS
    for name in pyproject.toml etc/config.yaml scripts/install.sh scripts/check.sh scripts/start.sh scripts/stop.sh scripts/status.sh scripts/restart.sh scripts/upgrade.sh; do [[ -f "$SRC_DIR/$name" ]] || die "Source missing $name in $SRC_DIR"; done
    [[ -f "$SRC_DIR/../../internal/ai/proto/video.proto" || -f "$SRC_DIR/src/proto/video.proto" ]] || die 'Shared video.proto missing; run scripts/generate-video-proto.sh'
    find_model
    [[ "$OFFLINE" == false || -d "$WHEELS_DIR" ]] || die "Offline wheelhouse missing: $WHEELS_DIR"
    ok "Preflight passed (delegate=$DELEGATE offline=$OFFLINE)"
    detail "Python=$PYTHON_BIN checkpoint=$MODEL"
}

install_payload() {
    local dest_model="$APP_DIR/models/gesture_recognizer.task" script
    if [[ "$DRY_RUN" == false ]]; then mkdir -p "$APP_DIR"/{bin,etc,libs,logs,models,run,scripts}; fi
    if [[ ! -x "$VENV/bin/python" ]]; then info "Creating Python environment ..."; run "$PYTHON_BIN" -m venv "$VENV"; fi
    local -a pip=("$VENV/bin/python" -m pip --disable-pip-version-check install)
    info "Installing gesture package and dependencies ..."
    local -a flags=()
    if [[ "$VERBOSE" == false ]]; then flags+=(--quiet); fi
    if [[ "$OFFLINE" == true ]]; then
        flags+=(--no-index "--find-links=$WHEELS_DIR" --no-build-isolation)
        run "${pip[@]}" "${flags[@]}" 'setuptools>=65' wheel
    fi
    run "${pip[@]}" "${flags[@]}" --upgrade "$SRC_DIR"
    info "Preparing model and configuration ..."
    if [[ ! -e "$dest_model" ]]; then run cp -- "$MODEL" "$dest_model"; else detail "Preserving model $dest_model"; fi
    if [[ "$DRY_RUN" == true ]]; then info "DRY-RUN: preserve existing model/config, install Bash scripts, write absolute venv/model/log paths"; return; fi
    if [[ ! -f "$CONFIG" ]]; then
        "$VENV/bin/python" - "$SRC_DIR/etc/config.yaml" "$CONFIG" "$dest_model" "$DELEGATE" "$ADDR" "$METRICS_PORT" "$APP_DIR/logs/mediapipe-gesture.log" <<'PY_INSTALL'
import sys,yaml
from pathlib import Path
source,target,model,delegate,addr,port,log=sys.argv[1:]
cfg=yaml.safe_load(Path(source).read_text())
host,p=addr.rsplit(':',1)
if not host or not 1<=int(p)<=65535 or not 0<=int(port)<=65535 or int(p)==int(port): raise SystemExit('Invalid/conflicting gRPC/metrics ports')
cfg['gesture'].update(enabled=True,model=model,delegate=delegate)
cfg['server']['addr']=addr
cfg['metrics'].update(enabled=int(port)!=0,port=int(port))
cfg['logging'].update(file=log,backup_days=30)
cfg['overlay']['assets_dir']=None
Path(target).write_text(yaml.safe_dump(cfg,sort_keys=False))
PY_INSTALL
    else detail "Preserving config $CONFIG and existing model; edit YAML then restart to change runtime settings"; fi
    info "Updating management scripts ..."
    for script in install.sh check.sh start.sh stop.sh status.sh restart.sh upgrade.sh; do
        if [[ "$SRC_DIR/scripts/$script" != "$APP_DIR/scripts/$script" ]]; then install -m 0755 "$SRC_DIR/scripts/$script" "$APP_DIR/scripts/$script"; fi
    done
    rm -f -- "$APP_DIR/scripts/manage.py" "$APP_DIR/scripts/_common.sh"
    # A shell wrapper calls the installed worker entry point, as segmentation does.
    printf '#!/usr/bin/env bash\nexec %q "$@"\n' "$WORKER_BIN" > "$APP_DIR/bin/mediapipe-gesture-server"
    chmod +x "$APP_DIR/bin/mediapipe-gesture-server"
    printf '%s\n' "$VENV" > "$APP_DIR/.app_venv"
    if ! printf '%s\n' "$APP_DIR" > "$SRC_DIR/.app_dir"; then warn 'Cannot save source .app_dir; use deployed scripts or --app-dir'; fi
    read_runtime_config
    ok "Package and scripts installed"
}

# Execute the requested operation.
parse_args "$@"
check_source
if [[ "$DRY_RUN" == true ]]; then install_payload; exit 0; fi
lock_service
if running; then die "Instance is running PID=$PID; use upgrade.sh instead"; fi
install_payload

ok "Installation complete"
hint 'Start' bash "$APP_DIR/scripts/start.sh"
