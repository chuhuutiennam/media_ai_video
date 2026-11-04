#!/usr/bin/env bash
# =============================================================================
# install.sh — Full installation of mediapipe-video on a GPU or CPU server
#
# Layout after install:
#   APP_DIR/
#     bin/                wrapper scripts (mediapipe-video)
#     etc/config.yaml     runtime config (patched from SRC_DIR/etc/)
#     libs/venv/          Python virtual environment
#     logs/               application + rotation logs
#     models/             TFLite model files (copy here manually)
#     scripts/            start.sh / stop.sh
#
# Usage:
#   ./scripts/install.sh [OPTIONS]
#
# Options:
#   --app-dir DIR     Application directory  [default: /u01/ims/mediapipe_video]
#   --src-dir DIR     Source directory       [default: parent of this script]
#   --venv DIR        Dùng venv có sẵn thay vì tạo mới  [default: APP_DIR/libs/venv]
#   --delegate MODE   gpu | cpu | auto       [default: auto-detect via nvidia-smi]
#   --offline         Use SRC_DIR/wheels/ only — no PyPI access required
#   --dry-run         Show what would be done without making any changes
#   --skip-check      Bỏ qua pre-flight checks (không khuyến nghị)
#   -h, --help
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
APP_DIR="/u01/ims/mediapipe_video"
VENV_DIR=""
DELEGATE="auto"
OFFLINE=false
DRY_RUN=false
SKIP_CHECK=false

# ── helpers ──────────────────────────────────────────────────────────────────
log() { printf '[%s] INFO  %s\n'  "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*"; }
die() { printf '[%s] ERROR %s\n'  "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*" >&2; exit 1; }

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
        --app-dir)   APP_DIR="$2";  shift 2 ;;
        --src-dir)   SRC_DIR="$2";  shift 2 ;;
        --venv)      VENV_DIR="$2"; shift 2 ;;
        --delegate)  DELEGATE="$2"; shift 2 ;;
        --offline)    OFFLINE=true;    shift   ;;
        --dry-run)    DRY_RUN=true;   shift   ;;
        --skip-check) SKIP_CHECK=true; shift  ;;
        -h|--help)    usage ;;
        *)           die "Unknown option: $1" ;;
    esac
done

# ── dry-run header ────────────────────────────────────────────────────────────
if [[ "$DRY_RUN" == true ]]; then
    printf '\n[DRY-RUN] ════════════════════════════════════════════\n'
    printf '[DRY-RUN] Dry-run mode — no changes will be made\n'
    printf '[DRY-RUN] Re-run without --dry-run to apply\n'
    printf '[DRY-RUN] ════════════════════════════════════════════\n\n'
fi

# ── validate source ───────────────────────────────────────────────────────────
[[ -f "$SRC_DIR/pyproject.toml" ]] || die "pyproject.toml not found in SRC_DIR=$SRC_DIR"
[[ -f "$SRC_DIR/etc/config.yaml" ]] || die "etc/config.yaml not found in SRC_DIR=$SRC_DIR"

log "SRC_DIR : $SRC_DIR"
log "APP_DIR : $APP_DIR"

# ── auto-detect delegate ──────────────────────────────────────────────────────
if [[ "$DELEGATE" == "auto" ]]; then
    if command -v nvidia-smi &>/dev/null && nvidia-smi &>/dev/null 2>&1; then
        GPU_NAME=$(nvidia-smi --query-gpu=name --format=csv,noheader 2>/dev/null | head -1 || echo "unknown")
        DELEGATE="gpu"
        log "GPU detected: $GPU_NAME → delegate=gpu"
    else
        DELEGATE="cpu"
        log "No GPU detected → delegate=cpu"
    fi
fi
[[ "$DELEGATE" == "gpu" || "$DELEGATE" == "cpu" ]] || die "--delegate must be gpu, cpu, or auto"

# ── pre-flight checks ─────────────────────────────────────────────────────────
if [[ "$SKIP_CHECK" == false ]]; then
    CHECK_ARGS=(--mode install --app-dir "$APP_DIR" --src-dir "$SRC_DIR" --delegate "$DELEGATE")
    [[ "$OFFLINE" == true ]] && CHECK_ARGS+=(--offline)
    bash "$SCRIPT_DIR/check.sh" "${CHECK_ARGS[@]}" \
        || die "Pre-flight checks thất bại — sửa các lỗi trên hoặc dùng --skip-check để bỏ qua"
fi

# ── find Python 3.10+ ─────────────────────────────────────────────────────────
PYTHON=""
for py in python3.12 python3.11 python3.10 python3; do
    if command -v "$py" &>/dev/null; then
        if "$py" -c 'import sys; sys.exit(0 if sys.version_info >= (3,10) else 1)' 2>/dev/null; then
            PYTHON=$(command -v "$py")
            break
        fi
    fi
done
[[ -n "$PYTHON" ]] || die "Python 3.10+ not found in PATH"
log "Python   : $PYTHON ($("$PYTHON" --version 2>&1))"

# ── create directory structure ────────────────────────────────────────────────
log "Creating directory layout under $APP_DIR ..."
run mkdir -p \
    "$APP_DIR/bin" \
    "$APP_DIR/etc" \
    "$APP_DIR/logs" \
    "$APP_DIR/scripts" \
    "$APP_DIR/models" \
    "$APP_DIR/libs"

# ── virtual environment ───────────────────────────────────────────────────────
if [[ -n "$VENV_DIR" ]]; then
    VENV="$VENV_DIR"
    [[ -d "$VENV" ]] || die "venv không tồn tại: $VENV"
    log "Using venv   : $VENV"
else
    VENV="$APP_DIR/libs/venv"
    if [[ -d "$VENV" ]]; then
        log "Reusing venv : $VENV"
    else
        log "Creating venv: $VENV"
        run "$PYTHON" -m venv "$VENV"
    fi
fi

# Dùng python -m pip thay vì bin/pip — hoạt động kể cả khi pip script không tồn tại
if [[ "$DRY_RUN" == false ]] && ! "$VENV/bin/python" -m pip --version &>/dev/null 2>&1; then
    log "Bootstrapping pip via ensurepip ..."
    "$VENV/bin/python" -m ensurepip --upgrade \
        || die "pip không khả dụng — cài python3.12-pip hoặc thêm pip wheel vào wheels/"
fi
run "$VENV/bin/python" -m pip install --upgrade pip --quiet

# ── install package ───────────────────────────────────────────────────────────
log "Installing mediapipe-video (offline=$OFFLINE) ..."
if [[ "$OFFLINE" == true ]]; then
    WHL_DIR="$SRC_DIR/wheels"
    [[ -d "$WHL_DIR" ]] || die "Offline mode requires SRC_DIR/wheels/ — build trước: python3.12 -m pip wheel . -w wheels/ --no-deps"
    # Bước 1: cài toàn bộ dependency wheels — không download
    readarray -t _wheels < <(find "$WHL_DIR" -name "*.whl" | sort)
    run "$VENV/bin/python" -m pip install \
        --no-index \
        --find-links="$WHL_DIR" \
        --no-deps \
        --no-build-isolation \
        "${_wheels[@]}"
    # Bước 2: cài package chính từ SRC_DIR — deps đã có trong venv, không cần network
    run "$VENV/bin/python" -m pip install \
        --no-deps \
        --no-build-isolation \
        "$SRC_DIR"
else
    run "$VENV/bin/python" -m pip install "$SRC_DIR" --quiet
fi
ok "Package installed"

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

# ── bin wrapper ───────────────────────────────────────────────────────────────
BIN_WRAPPER="$APP_DIR/bin/mediapipe-video"
if [[ "$DRY_RUN" == true ]]; then
    printf '[DRY-RUN] would write bin wrapper: %s\n' "$BIN_WRAPPER"
else
    cat > "$BIN_WRAPPER" <<WRAPPER
#!/usr/bin/env bash
exec "$VENV/bin/mediapipe-video" "\$@"
WRAPPER
    chmod +x "$BIN_WRAPPER"
fi

# ── copy model files ──────────────────────────────────────────────────────────
CHECKPOINT_DIR="$SRC_DIR/checkpoint"
if [[ ! -d "$CHECKPOINT_DIR" && -d "$SRC_DIR/../checkpoint" ]]; then
    CHECKPOINT_DIR="$SRC_DIR/../checkpoint"
fi
FIRST_MODEL=""
if [[ -d "$CHECKPOINT_DIR" ]]; then
    for _model in "$CHECKPOINT_DIR"/*.tflite; do
        [[ -e "$_model" ]] || continue
        _dest="$APP_DIR/models/$(basename "$_model")"
        if [[ -f "$_dest" ]]; then
            log "Model already exists — skipping: $(basename "$_model")"
        else
            run cp "$_model" "$_dest"
            ok "Model copied: $(basename "$_model") → $APP_DIR/models/"
        fi
        [[ -z "$FIRST_MODEL" ]] && FIRST_MODEL="$APP_DIR/models/$(basename "$_model")"
    done
else
    log "checkpoint/ not found in SRC_DIR or sibling directory — copy model manually to $APP_DIR/models/"
fi

# ── copy config ───────────────────────────────────────────────────────────────
DEST_CONFIG="$APP_DIR/etc/config.yaml"
if [[ -f "$DEST_CONFIG" ]]; then
    log "Config already exists — skipping (delete $DEST_CONFIG to reset)"
else
    log "Writing config: $DEST_CONFIG"
    run cp "$SRC_DIR/etc/config.yaml" "$DEST_CONFIG"
    run sed -i "s|delegate: .*|delegate: \"$DELEGATE\"|"                                      "$DEST_CONFIG"
    run sed -i "s|file: .*mediapipe-video.log.*|file: \"$APP_DIR/logs/mediapipe-video.log\"|" "$DEST_CONFIG"
    if [[ -n "$FIRST_MODEL" ]]; then
        run sed -i "s|model: .*|model: \"$FIRST_MODEL\"|"                                     "$DEST_CONFIG"
        ok "Config written (delegate=$DELEGATE, model=$(basename "$FIRST_MODEL"))"
    else
        ok "Config written (delegate=$DELEGATE) — set mediapipe.model manually"
    fi
fi

# ── install management scripts ────────────────────────────────────────────────
log "Installing management scripts ..."
for script in start.sh stop.sh; do
    run install -m 0755 "$SCRIPT_DIR/$script" "$APP_DIR/scripts/$script"
done
ok "Scripts installed: $APP_DIR/scripts/"

# ── remember APP_DIR for upgrade script ───────────────────────────────────────
if [[ "$DRY_RUN" == true ]]; then
    printf '[DRY-RUN] would write .app_dir / .app_venv files\n'
else
    echo "$APP_DIR" > "$SRC_DIR/.app_dir"
    echo "$VENV"    > "$SRC_DIR/.app_venv"
    echo "$VENV"    > "$APP_DIR/.app_venv"
fi

# ── summary ───────────────────────────────────────────────────────────────────
if [[ "$DRY_RUN" == true ]]; then
    printf '\n[DRY-RUN] ════════════════════════════════════════════\n'
    printf '[DRY-RUN] Dry-run complete — nothing was changed\n'
    printf '[DRY-RUN] Re-run without --dry-run to apply\n'
    printf '[DRY-RUN] ════════════════════════════════════════════\n\n'
else
    log "═══════════════════════════════════════════"
    log "Installation complete"
    log "  APP_DIR  : $APP_DIR"
    log "  delegate : $DELEGATE"
    log "  venv     : $VENV"
    log ""
    log "Next steps:"
    log "  1. Copy model   →  $APP_DIR/models/<name>.tflite"
    log "  2. Edit config  →  $DEST_CONFIG"
    log "       mediapipe.model: $APP_DIR/models/<name>.tflite"
    log "  3. Start        →  $APP_DIR/scripts/start.sh"
    log "═══════════════════════════════════════════"
fi
