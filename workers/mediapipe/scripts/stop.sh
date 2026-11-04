#!/usr/bin/env bash
# =============================================================================
# stop.sh — Stop mediapipe-video service
#
# Sends SIGTERM and waits up to --grace seconds; falls back to SIGKILL.
#
# Usage:
#   ./scripts/stop.sh [OPTIONS]
#
# Options:
#   --app-dir DIR   Override application directory
#   --grace SECS    Graceful shutdown timeout  [default: 5]
#   -h, --help
# =============================================================================
set -euo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GRACE_SECS=5

# ── helpers ──────────────────────────────────────────────────────────────────
log() { printf '[%s] INFO  %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*"; }
die() { printf '[%s] ERROR %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*" >&2; exit 1; }

usage() {
    grep '^#' "$0" | grep -v '#!/' | sed 's/^# \?//'
    exit 0
}

# ── args ──────────────────────────────────────────────────────────────────────
APP_DIR_OVERRIDE=""
while [[ $# -gt 0 ]]; do
    case $1 in
        --app-dir) APP_DIR_OVERRIDE="$2"; shift 2 ;;
        --grace)   GRACE_SECS="$2";       shift 2 ;;
        -h|--help) usage ;;
        *)         die "Unknown option: $1" ;;
    esac
done

# ── detect APP_DIR ────────────────────────────────────────────────────────────
if [[ -n "$APP_DIR_OVERRIDE" ]]; then
    APP_DIR="$APP_DIR_OVERRIDE"
elif [[ -f "$SELF_DIR/../libs/venv/bin/mediapipe-video" ]]; then
    APP_DIR="$(cd "$SELF_DIR/.." && pwd)"
else
    APP_DIR="/u01/ims/mediapipe_video"
fi

PID_FILE="$APP_DIR/logs/mediapipe-video.pid"

# ── check running ─────────────────────────────────────────────────────────────
if [[ ! -f "$PID_FILE" ]]; then
    log "Not running (no PID file at $PID_FILE)"
    exit 0
fi

PID=$(cat "$PID_FILE")
if ! kill -0 "$PID" 2>/dev/null; then
    log "Not running (stale PID $PID — cleaning up)"
    rm -f "$PID_FILE"
    exit 0
fi

# ── graceful stop ─────────────────────────────────────────────────────────────
log "Sending SIGTERM to PID $PID (grace=${GRACE_SECS}s) ..."
kill -TERM "$PID"

elapsed=0
while kill -0 "$PID" 2>/dev/null; do
    sleep 1
    elapsed=$((elapsed + 1))
    if [[ $elapsed -ge $GRACE_SECS ]]; then
        log "Grace period expired — sending SIGKILL to PID $PID ..."
        kill -KILL "$PID" 2>/dev/null || true
        break
    fi
done

# Wait for final exit
sleep 0.5
rm -f "$PID_FILE"
log "Stopped (elapsed ${elapsed}s)"
