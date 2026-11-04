#!/usr/bin/env bash
# =============================================================================
# upgrade.sh — Upgrade mediapipe-video to the latest source code
#
# Steps:
#   1. Stop the running instance (if any)
#   2. Reinstall package from SRC_DIR (or SRC_DIR/wheels/ in offline mode)
#   3. Sync start.sh / stop.sh to APP_DIR/scripts/
#   4. Restart (unless --no-restart)
#
# Usage:
#   ./scripts/upgrade.sh [OPTIONS]
#
# Options:
#   --app-dir DIR     Application directory  [default: read from .app_dir or /u01/ims/mediapipe_video]
#   --src-dir DIR     Source directory       [default: parent of this script]
#   --venv DIR        Override venv path     [default: read from .app_venv or APP_DIR/libs/venv]
#   --no-restart      Upgrade without restarting the service
#   --offline         Use SRC_DIR/wheels/ only — no PyPI access required
#   --dry-run         Show what would be done without making any changes
#   --skip-check      Bỏ qua pre-flight checks (không khuyến nghị)
#   -h, --help
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
RESTART=true
OFFLINE=false
DRY_RUN=false
SKIP_CHECK=false
VENV_DIR=""

# Read last installed APP_DIR (written by install.sh)
if [[ -f "$SRC_DIR/.app_dir" ]]; then
    APP_DIR=$(cat "$SRC_DIR/.app_dir")
else
    APP_DIR="/u01/ims/mediapipe_video"
fi

# ── helpers ──────────────────────────────────────────────────────────────────
log() { printf '[%s] INFO  %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*"; }
die() { printf '[%s] ERROR %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*" >&2; exit 1; }

ok() {
    if [[ "$DRY_RUN" == true ]]; then
        printf '[DRY-RUN] would: %s\n' "$*"
    else
        printf '[%s] OK    %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*"
    fi
}

# Run a command, or print it in dry-run mode.
run() {
    if [[ "$DRY_RUN" == true ]]; then
        printf '[DRY-RUN] %s\n' "$*"
    else
        "$@"
    fi
}

usage() {
    grep '^#' "$0" | grep -v '#!/' | sed 's/^# \?//'
    exit 0
}

# ── args ──────────────────────────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
    case $1 in
        --app-dir)    APP_DIR="$2";  shift 2 ;;
        --src-dir)    SRC_DIR="$2";  shift 2 ;;
        --venv)       VENV_DIR="$2"; shift 2 ;;
        --no-restart) RESTART=false;   shift   ;;
        --offline)    OFFLINE=true;    shift   ;;
        --dry-run)    DRY_RUN=true;    shift   ;;
        --skip-check) SKIP_CHECK=true; shift   ;;
        -h|--help)    usage ;;
        *)            die "Unknown option: $1" ;;
    esac
done

# ── dry-run header ────────────────────────────────────────────────────────────
if [[ "$DRY_RUN" == true ]]; then
    printf '\n[DRY-RUN] ════════════════════════════════════════════\n'
    printf '[DRY-RUN] Dry-run mode — no changes will be made\n'
    printf '[DRY-RUN] Re-run without --dry-run to apply\n'
    printf '[DRY-RUN] ════════════════════════════════════════════\n\n'
fi

# Xác định VENV: --venv > .app_venv > APP_DIR/libs/venv
if [[ -n "$VENV_DIR" ]]; then
    VENV="$VENV_DIR"
elif [[ -f "$SRC_DIR/.app_venv" ]]; then
    VENV=$(cat "$SRC_DIR/.app_venv")
else
    VENV="$APP_DIR/libs/venv"
fi
PID_FILE="$APP_DIR/logs/mediapipe-video.pid"

[[ -d "$VENV" ]] || die "venv not found: $VENV — run install.sh first"
[[ -f "$SRC_DIR/pyproject.toml" ]] || die "pyproject.toml not found in $SRC_DIR"

log "SRC_DIR : $SRC_DIR"
log "APP_DIR : $APP_DIR"

# ── pre-flight checks ─────────────────────────────────────────────────────────
if [[ "$SKIP_CHECK" == false ]]; then
    CHECK_ARGS=(--mode upgrade --app-dir "$APP_DIR" --src-dir "$SRC_DIR")
    [[ "$OFFLINE" == true ]] && CHECK_ARGS+=(--offline)
    bash "$SCRIPT_DIR/check.sh" "${CHECK_ARGS[@]}" \
        || die "Pre-flight checks thất bại — sửa các lỗi trên hoặc dùng --skip-check để bỏ qua"
fi

# ── show current version ──────────────────────────────────────────────────────
CURRENT_VER=$("$VENV/bin/python" -m pip show mediapipe-video 2>/dev/null | awk '/^Version:/{print $2}' || echo "unknown")
log "Installed version : $CURRENT_VER"

NEW_VER=$(grep '^version' "$SRC_DIR/pyproject.toml" | head -1 | sed 's/.*= *"\(.*\)"/\1/' || echo "unknown")
log "Source version    : $NEW_VER"

# ── check running instance ────────────────────────────────────────────────────
was_running=false
if [[ -f "$PID_FILE" ]]; then
    PID=$(cat "$PID_FILE")
    if kill -0 "$PID" 2>/dev/null; then
        log "Service is running (PID $PID) — will stop before upgrade"
        was_running=true
    fi
else
    log "Service is not running"
fi

# ── stop running instance ─────────────────────────────────────────────────────
if [[ "$was_running" == true ]]; then
    log "Stopping service ..."
    run bash "$APP_DIR/scripts/stop.sh"
fi

# ── upgrade package ───────────────────────────────────────────────────────────
log "Upgrading mediapipe-video (offline=$OFFLINE) ..."
if [[ "$OFFLINE" == true ]]; then
    WHL_DIR="$SRC_DIR/wheels"
    [[ -d "$WHL_DIR" ]] || die "Offline mode requires SRC_DIR/wheels/"
    # Bước 1: cập nhật dependency wheels — không download
    readarray -t _wheels < <(find "$WHL_DIR" -name "*.whl" | sort)
    run "$VENV/bin/python" -m pip install \
        --no-index \
        --find-links="$WHL_DIR" \
        --no-deps \
        --no-build-isolation \
        "${_wheels[@]}"
    # Bước 2: cài lại package chính từ SRC_DIR — không cần network
    run "$VENV/bin/python" -m pip install \
        --no-deps \
        --no-build-isolation \
        --upgrade \
        "$SRC_DIR"
else
    run "$VENV/bin/python" -m pip install --upgrade "$SRC_DIR" --quiet
fi
ok "Package upgraded ($CURRENT_VER → $NEW_VER)"

# ── ensure video.proto is in installed package (package-data fallback) ────────
PROTO_SRC="$SRC_DIR/src/proto/video.proto"
if [[ -f "$SRC_DIR/../../internal/ai/proto/video.proto" ]]; then
    PROTO_SRC="$SRC_DIR/../../internal/ai/proto/video.proto"
fi
if [[ -f "$PROTO_SRC" ]]; then
    if [[ "$DRY_RUN" == false ]]; then
        PKG_DIR=$("$VENV/bin/python" -c \
            "import mediapipe_video, os; print(os.path.dirname(mediapipe_video.__file__))")
        if [[ -n "$PKG_DIR" ]]; then
            cp -f "$PROTO_SRC" "$PKG_DIR/video.proto"
            ok "video.proto → $PKG_DIR/"
        fi
    else
        printf '[DRY-RUN] would copy video.proto → <venv>/site-packages/mediapipe_video/\n'
    fi
else
    log "video.proto không tìm thấy tại $PROTO_SRC — bỏ qua"
fi

# ── sync management scripts ───────────────────────────────────────────────────
log "Syncing management scripts ..."
for script in start.sh stop.sh; do
    run install -m 0755 "$SCRIPT_DIR/$script" "$APP_DIR/scripts/$script"
done
ok "Scripts synced"

# ── save APP_DIR / VENV ──────────────────────────────────────────────────────
if [[ "$DRY_RUN" == true ]]; then
    printf '[DRY-RUN] would write .app_dir / .app_venv files\n'
else
    echo "$APP_DIR" > "$SRC_DIR/.app_dir"
    echo "$VENV"    > "$SRC_DIR/.app_venv"
    echo "$VENV"    > "$APP_DIR/.app_venv"
fi

# ── restart ───────────────────────────────────────────────────────────────────
if [[ "$RESTART" == true && "$was_running" == true ]]; then
    log "Restarting service ..."
    run bash "$APP_DIR/scripts/start.sh"
elif [[ "$RESTART" == true && "$was_running" == false ]]; then
    log "Service was not running — skipping restart (use start.sh to start manually)"
fi

# ── summary ───────────────────────────────────────────────────────────────────
if [[ "$DRY_RUN" == true ]]; then
    printf '\n[DRY-RUN] ════════════════════════════════════════════\n'
    printf '[DRY-RUN] Dry-run complete — nothing was changed\n'
    printf '[DRY-RUN] Re-run without --dry-run to apply\n'
    printf '[DRY-RUN] ════════════════════════════════════════════\n\n'
else
    ok "Upgrade complete"
fi
