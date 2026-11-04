#!/usr/bin/env bash
# =============================================================================
# start.sh — Start mediapipe-video service
#
# Works from both SRC_DIR/scripts/ (source) and APP_DIR/scripts/ (deployed):
# APP_DIR is auto-detected from script location if the venv exists in ../libs/.
#
# Usage:
#   ./scripts/start.sh [OPTIONS]
#
# Options:
#   --app-dir DIR   Override application directory
#   --foreground    Run in foreground (no PID file; useful for systemd/docker)
#   -h, --help
# =============================================================================
set -euo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FOREGROUND=false

# ── helpers ──────────────────────────────────────────────────────────────────
log() { printf '[%s] INFO  %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*"; }
die() { printf '[%s] ERROR %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*" >&2; exit 1; }

usage() {
    grep '^#' "$0" | grep -v '#!/' | sed 's/^# \?//'
    exit 0
}

# ── args ──────────────────────────────────────────────────────────────────────
# Parse before auto-detect so --app-dir override takes effect
APP_DIR_OVERRIDE=""
while [[ $# -gt 0 ]]; do
    case $1 in
        --app-dir)    APP_DIR_OVERRIDE="$2"; shift 2 ;;
        --foreground) FOREGROUND=true;       shift   ;;
        -h|--help)    usage ;;
        *)            die "Unknown option: $1" ;;
    esac
done

# ── detect APP_DIR ────────────────────────────────────────────────────────────
if [[ -n "$APP_DIR_OVERRIDE" ]]; then
    APP_DIR="$APP_DIR_OVERRIDE"
elif [[ -f "$SELF_DIR/../libs/venv/bin/mediapipe-video" ]] || [[ -f "$SELF_DIR/../.app_venv" ]]; then
    # Running from APP_DIR/scripts/ — detect by venv or .app_venv marker
    APP_DIR="$(cd "$SELF_DIR/.." && pwd)"
else
    APP_DIR="/u01/ims/mediapipe_video"
fi

# Venv: prefer APP_DIR/.app_venv written by install.sh, else default
if [[ -f "$APP_DIR/.app_venv" ]]; then
    VENV=$(cat "$APP_DIR/.app_venv")
else
    VENV="$APP_DIR/libs/venv"
fi
CONFIG="$APP_DIR/etc/config.yaml"
PID_FILE="$APP_DIR/logs/mediapipe-video.pid"
STDOUT_LOG="$APP_DIR/logs/stdout.log"

[[ -d "$VENV" ]]   || die "venv not found: $VENV — run install.sh first"
[[ -f "$CONFIG" ]] || die "config not found: $CONFIG"

mkdir -p "$APP_DIR/logs"

# ── foreground mode (systemd / docker) ───────────────────────────────────────
if [[ "$FOREGROUND" == true ]]; then
    log "Starting in foreground (APP_DIR=$APP_DIR) ..."
    exec "$VENV/bin/mediapipe-video" --config "$CONFIG"
fi

# ── background mode ───────────────────────────────────────────────────────────
if [[ -f "$PID_FILE" ]]; then
    PID=$(cat "$PID_FILE")
    if kill -0 "$PID" 2>/dev/null; then
        log "Already running (PID $PID) — nothing to do"
        exit 0
    fi
    rm -f "$PID_FILE"
fi

log "Starting mediapipe-video (APP_DIR=$APP_DIR) ..."
nohup "$VENV/bin/mediapipe-video" \
    --config "$CONFIG" \
    >> "$STDOUT_LOG" 2>&1 &

PID=$!
echo "$PID" > "$PID_FILE"
log "Started (PID $PID)"
log "  config    : $CONFIG"
log "  stdout    : $STDOUT_LOG"
log "  json log  : $APP_DIR/logs/mediapipe-video.log"
log "  pid file  : $PID_FILE"
