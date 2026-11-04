#!/usr/bin/env bash
# =============================================================================
# check.sh — Pre-flight checks for mediapipe-video
#
# Kiểm tra các điều kiện cần thiết trước khi cài đặt, nâng cấp, hoặc chạy ứng dụng.
# Thoát với code 0 nếu tất cả PASS/WARN, code 1 nếu có bất kỳ FAIL nào.
#
# Usage:
#   ./scripts/check.sh [OPTIONS]
#
# Options:
#   --mode MODE       install | upgrade | runtime | all  [default: all]
#   --app-dir DIR     Application directory  [default: /u01/ims/mediapipe_video]
#   --src-dir DIR     Source directory       [default: parent of this script]
#   --delegate MODE   gpu | cpu              [default: gpu]
#   --offline         Kiểm tra thêm wheels/ directory và tính tương thích
#   -h, --help
#
# Modes:
#   install   Kiểm tra môi trường trước khi chạy install.sh
#   upgrade   Kiểm tra trước khi chạy upgrade.sh
#   runtime   Kiểm tra ứng dụng có thể khởi động được không
#   all       Chạy tất cả các nhóm kiểm tra phù hợp
# =============================================================================

# Cần bash 4+ cho declare -A — kiểm tra trước set -e
if (( BASH_VERSINFO[0] < 4 )); then
    printf '[FAIL]  Bash %s.%s — cần bash ≥ 4.0 (có declare -A)\n' \
        "${BASH_VERSINFO[0]}" "${BASH_VERSINFO[1]}"
    printf '        Cài: sudo yum install bash  /  sudo apt install bash\n'
    exit 1
fi

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
APP_DIR="/u01/ims/mediapipe_video"
DELEGATE="gpu"
OFFLINE=false
MODE="all"

# ── helpers ───────────────────────────────────────────────────────────────────
PASS_COUNT=0; WARN_COUNT=0; FAIL_COUNT=0

pass() { printf '[PASS]  %s\n' "$*"; PASS_COUNT=$((PASS_COUNT + 1)); }
warn() { printf '[WARN]  %s\n' "$*"; WARN_COUNT=$((WARN_COUNT + 1)); }
fail() { printf '[FAIL]  %s\n' "$*"; FAIL_COUNT=$((FAIL_COUNT + 1)); }
info() { printf '[INFO]  %s\n' "$*"; }
sep()  { printf -- '──────────────────────────────────────────────────\n'; }

usage() {
    grep '^#' "$0" | grep -v '#!/' | sed 's/^# \?//'
    exit 0
}

# ── args ──────────────────────────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
    case $1 in
        --mode)      MODE="$2";     shift 2 ;;
        --app-dir)   APP_DIR="$2";  shift 2 ;;
        --src-dir)   SRC_DIR="$2";  shift 2 ;;
        --delegate)  DELEGATE="$2"; shift 2 ;;
        --offline)   OFFLINE=true;  shift   ;;
        -h|--help)   usage ;;
        *)           printf '[ERROR] Unknown option: %s\n' "$1" >&2; exit 1 ;;
    esac
done

[[ "$MODE" =~ ^(install|upgrade|runtime|all)$ ]] \
    || { printf '[ERROR] --mode must be install|upgrade|runtime|all\n' >&2; exit 1; }

# ── G1: Hệ điều hành & kiến trúc ─────────────────────────────────────────────
# Lưu lại để dùng ở các check khác
SYS_ARCH=$(uname -m)                                        # x86_64 | aarch64 | ...
SYS_OS=$(. /etc/os-release 2>/dev/null && echo "$ID $VERSION_ID" || uname -s)
GLIBC_VER=$(ldd --version 2>/dev/null | head -1 \
    | awk '{print $NF}' | grep -E '^[0-9]+\.[0-9]+$' || echo "0.0")
GLIBC_MINOR=$(echo "$GLIBC_VER" | cut -d'.' -f2)

# Python được detect bởi check_python, dùng lại ở check_wheel_compat
PYTHON_BIN=""
PYTHON_CP_TAG=""   # cp310 | cp311 | cp312

check_system() {
    info "Kiểm tra hệ điều hành & kiến trúc ..."
    pass "OS: $SYS_OS | arch: $SYS_ARCH"
    pass "Bash ${BASH_VERSINFO[0]}.${BASH_VERSINFO[1]}"

    # glibc — manylinux2014 cần ≥ 2.17, manylinux_2_28 cần ≥ 2.28
    if [[ "$GLIBC_VER" == "0.0" ]]; then
        warn "Không đọc được phiên bản glibc"
    elif (( GLIBC_MINOR < 17 )); then
        fail "glibc $GLIBC_VER — cần ≥ 2.17 (manylinux2014). Nâng cấp OS hoặc dùng container"
    elif (( GLIBC_MINOR < 28 )); then
        pass "glibc $GLIBC_VER (tương thích manylinux2014; một số wheel mới cần ≥ 2.28)"
    else
        pass "glibc $GLIBC_VER"
    fi
}

# ── G2: Python ────────────────────────────────────────────────────────────────
check_python() {
    info "Kiểm tra Python ..."
    for py in python3.12 python3.11 python3.10 python3; do
        if command -v "$py" &>/dev/null; then
            if "$py" -c 'import sys; sys.exit(0 if sys.version_info >= (3,10) else 1)' 2>/dev/null; then
                PYTHON_BIN=$(command -v "$py")
                local ver
                ver=$("$PYTHON_BIN" --version 2>&1)
                local minor
                minor=$("$PYTHON_BIN" -c 'import sys; print(sys.version_info.minor)')
                PYTHON_CP_TAG="cp3${minor}"
                pass "Python $ver → $PYTHON_BIN"
                return
            fi
        fi
    done
    fail "Python 3.10+ không tìm thấy trong PATH"
}

check_python_venv() {
    info "Kiểm tra python venv module ..."
    [[ -z "$PYTHON_BIN" ]] && { warn "Bỏ qua (Python chưa tìm thấy)"; return; }

    local tmp_venv
    tmp_venv=$(mktemp -d)
    if "$PYTHON_BIN" -m venv "$tmp_venv/probe" &>/dev/null 2>&1; then
        pass "python -m venv: OK"
    else
        local minor
        minor=$("$PYTHON_BIN" -c 'import sys; print(sys.version_info.minor)')
        fail "python -m venv thất bại — thiếu venv module"
        if command -v dnf &>/dev/null || command -v yum &>/dev/null; then
            fail "  RHEL/CentOS: dnf install python3.${minor}  (venv bundled với AppStream Python)"
        else
            fail "  Ubuntu/Debian: apt install python3.${minor}-venv"
        fi
    fi
    rm -rf "$tmp_venv"
}

# ── G3: System libraries ──────────────────────────────────────────────────────
check_system_libs() {
    info "Kiểm tra system libraries ..."

    # Hàm trợ giúp: tìm lib trong ldconfig hoặc /usr/lib
    has_lib() {
        ldconfig -p 2>/dev/null | grep -qi "$1" \
            || find /usr/lib /usr/lib64 /lib /lib64 -maxdepth 3 -name "$1*" 2>/dev/null | grep -q .
    }

    # libstdc++.so.6 — C++ runtime, cần bởi mediapipe, grpcio, numpy
    if has_lib 'libstdc++\.so\.6'; then
        pass "libstdc++.so.6 (C++ runtime)"
    else
        fail "libstdc++.so.6 không tìm thấy — apt install libstdc++6  /  yum install libstdc++"
    fi

    # libgomp.so.1 — OpenMP, cần bởi numpy và mediapipe
    if has_lib 'libgomp\.so\.1'; then
        pass "libgomp.so.1 (OpenMP)"
    else
        warn "libgomp.so.1 không tìm thấy — apt install libgomp1  /  yum install libgomp"
    fi

    # libGL.so.1 — OpenGL, cần bởi OpenCV (bundled trong mediapipe)
    if has_lib 'libGL\.so\.1'; then
        pass "libGL.so.1 (OpenGL)"
    elif [[ "$DELEGATE" == "gpu" ]]; then
        fail "libGL.so.1 không tìm thấy — apt install libgl1  /  yum install mesa-libGL"
    else
        warn "libGL.so.1 không tìm thấy — cần nếu đổi sang delegate=gpu sau này"
    fi

    # libglib-2.0.so.0 — GLib, cần bởi một số OpenCV dependencies
    if has_lib 'libglib-2\.0\.so\.0'; then
        pass "libglib-2.0.so.0"
    else
        warn "libglib-2.0.so.0 không tìm thấy — apt install libglib2.0-0  /  yum install glib2"
    fi

    # libEGL.so — cần cho MediaPipe GPU delegate
    if has_lib 'libEGL\.so'; then
        if [[ "$DELEGATE" == "gpu" ]]; then
            pass "libEGL.so (EGL — cần cho GPU delegate)"
        else
            pass "libEGL.so (EGL)"
        fi
    else
        if [[ "$DELEGATE" == "gpu" ]]; then
            fail "libEGL.so không tìm thấy — MediaPipe GPU delegate cần EGL"
            fail "     apt install libegl1  /  yum install mesa-libEGL"
        else
            warn "libEGL.so không tìm thấy — cần nếu đổi sang delegate=gpu"
        fi
    fi

    # libxcb.so.1 — X11 XCB, cần bởi opencv-python (cv2)
    if has_lib 'libxcb\.so\.1'; then
        pass "libxcb.so.1 (X11 XCB)"
    else
        fail "libxcb.so.1 không tìm thấy — dnf install libxcb  /  apt install libxcb1"
    fi

    # libX11.so — cần bởi cv2 kể cả trên headless server (dlopen khi import, không phải khi dùng GUI)
    if has_lib 'libX11\.so'; then
        pass "libX11.so"
    else
        fail "libX11.so không tìm thấy — dnf install libX11  /  apt install libx11-6"
    fi

    # libXext.so — X11 extension, cần bởi cv2
    if has_lib 'libXext\.so'; then
        pass "libXext.so"
    else
        warn "libXext.so không tìm thấy — dnf install libXext  /  apt install libxext6"
    fi

    # libSM.so / libXrender.so — cần bởi cv2
    if has_lib 'libSM\.so'; then
        pass "libSM.so"
    else
        warn "libSM.so không tìm thấy — dnf install libSM  /  apt install libsm6"
    fi

    if has_lib 'libXrender\.so'; then
        pass "libXrender.so"
    else
        warn "libXrender.so không tìm thấy — dnf install libXrender  /  apt install libxrender1"
    fi

    # libz.so — zlib, cần bởi nhiều package
    if has_lib 'libz\.so'; then
        pass "libz.so (zlib)"
    else
        warn "libz.so không tìm thấy — apt install zlib1g  /  yum install zlib"
    fi
}

# ── G4: GPU ───────────────────────────────────────────────────────────────────
check_gpu() {
    info "Kiểm tra GPU (delegate=$DELEGATE) ..."

    if [[ "$DELEGATE" != "gpu" ]]; then
        pass "Delegate=cpu — không cần GPU"
        return
    fi

    if ! command -v nvidia-smi &>/dev/null; then
        fail "nvidia-smi không tìm thấy — cài NVIDIA driver hoặc dùng --delegate cpu"
        return
    fi

    if ! nvidia-smi &>/dev/null 2>&1; then
        fail "nvidia-smi lỗi — driver có vấn đề hoặc không có GPU"
        return
    fi

    local gpu_name driver_ver vram_total
    gpu_name=$(nvidia-smi  --query-gpu=name           --format=csv,noheader 2>/dev/null | head -1)
    driver_ver=$(nvidia-smi --query-gpu=driver_version --format=csv,noheader 2>/dev/null | head -1)
    vram_total=$(nvidia-smi  --query-gpu=memory.total   --format=csv,noheader,nounits 2>/dev/null | head -1 || echo "?")
    pass "GPU: $gpu_name | driver: $driver_ver | VRAM: ${vram_total} MiB"

    # Driver version ≥ 450 — yêu cầu tối thiểu cho MediaPipe
    local driver_major
    driver_major=$(echo "$driver_ver" | cut -d'.' -f1)
    if (( driver_major < 450 )); then
        warn "Driver $driver_ver < 450 — MediaPipe GPU delegate yêu cầu driver ≥ 450"
    fi
}

# ── G5: Disk & permissions ────────────────────────────────────────────────────
check_disk_and_permissions() {
    info "Kiểm tra disk và quyền truy cập ..."

    local check_dir="$APP_DIR"
    while [[ ! -d "$check_dir" ]]; do
        check_dir="$(dirname "$check_dir")"
    done

    local avail_kb avail_gb
    avail_kb=$(df -k "$check_dir" 2>/dev/null | tail -1 | awk '{print $4}' || echo 0)
    avail_gb=$(( avail_kb / 1024 / 1024 ))
    if (( avail_gb < 2 )); then
        fail "Disk space: ${avail_gb} GB tại $check_dir (cần ≥ 2 GB)"
    else
        pass "Disk space: ${avail_gb} GB tại $check_dir"
    fi

    if [[ -d "$APP_DIR" ]]; then
        [[ -w "$APP_DIR" ]] \
            && pass "Ghi được vào APP_DIR: $APP_DIR" \
            || fail "Không có quyền ghi vào APP_DIR: $APP_DIR"
    else
        [[ -w "$check_dir" ]] \
            && pass "Ghi được vào $check_dir (APP_DIR sẽ được tạo)" \
            || fail "Không có quyền ghi vào $check_dir — không thể tạo $APP_DIR"
    fi
}

# ── G6: Source files ──────────────────────────────────────────────────────────
check_source_files() {
    info "Kiểm tra source files tại $SRC_DIR ..."
    for f in pyproject.toml etc/config.yaml; do
        [[ -f "$SRC_DIR/$f" ]] \
            && pass "Source: $f" \
            || fail "Source không tìm thấy: $SRC_DIR/$f"
    done
    for d in src scripts; do
        [[ -d "$SRC_DIR/$d" ]] \
            && pass "Source: $d/" \
            || fail "Source không tìm thấy: $SRC_DIR/$d/"
    done
}

# ── G7: Wheels (offline) ──────────────────────────────────────────────────────
# Cập nhật khi thay đổi dependencies trong pyproject.toml
declare -A REQUIRED_WHEELS=(
    ["mediapipe"]="0.10.35"
    ["grpcio"]="1.81.1"
    ["grpcio_tools"]="1.81.1"
    ["protobuf"]="6.33.6"
    ["numpy"]="2.4.6"
    ["opencv_contrib_python"]="4."
    ["pyyaml"]="6."
    ["ruamel_yaml"]="0.19"
)

check_wheels() {
    local WHL_DIR="$SRC_DIR/wheels"
    info "Kiểm tra offline wheels tại $WHL_DIR ..."

    if [[ ! -d "$WHL_DIR" ]]; then
        fail "Thư mục wheels/ không tồn tại: $WHL_DIR"
        fail "  Chạy trên máy có internet:"
        fail "    pip download -d wheels/ mediapipe==0.10.35 grpcio==1.81.1 grpcio-tools==1.81.1 \\"
        fail "        protobuf==6.33.6 numpy==2.4.6 'opencv-contrib-python>=4.5' 'PyYAML>=6.0' 'ruamel.yaml>=0.19'"
        fail "    pip wheel . -w wheels/ --no-deps"
        return
    fi

    local whl_count
    whl_count=$(find "$WHL_DIR" -maxdepth 1 -name "*.whl" | wc -l)
    pass "Wheels directory tồn tại ($whl_count files)"

    local norm lower lower_norm whl_dir="$WHL_DIR"
    for pkg in "${!REQUIRED_WHEELS[@]}"; do
        local ver="${REQUIRED_WHEELS[$pkg]}"
        norm="${pkg//[-.]/_}"          # hyphen/dot → underscore (pip normalize)
        lower="${pkg,,}"               # lowercase (bash 4+)
        lower_norm="${lower//[-.]/_}"  # lowercase + hyphen/dot → underscore
        if compgen -G "$whl_dir/${pkg}-${ver}*.whl"       > /dev/null 2>&1 || \
           compgen -G "$whl_dir/${norm}-${ver}*.whl"      > /dev/null 2>&1 || \
           compgen -G "$whl_dir/${lower}-${ver}*.whl"     > /dev/null 2>&1 || \
           compgen -G "$whl_dir/${lower_norm}-${ver}*.whl" > /dev/null 2>&1; then
            pass "Wheel hiện diện: $pkg ${ver}*"
        else
            fail "Wheel thiếu: $pkg ${ver}* (cần $whl_dir/${pkg}-${ver}*.whl)"
        fi
    done
}

# ── G8: Wheel compatibility ───────────────────────────────────────────────────
check_wheel_compat() {
    local WHL_DIR="$SRC_DIR/wheels"
    [[ -d "$WHL_DIR" ]] || return
    [[ -z "$PYTHON_CP_TAG" ]] && { warn "Bỏ qua wheel compat (Python chưa detect)"; return; }

    info "Kiểm tra tính tương thích wheel (Python=$PYTHON_CP_TAG, arch=$SYS_ARCH, glibc=$GLIBC_VER) ..."

    local compat_errors=0
    while IFS= read -r whl; do
        local fname base pytag _abi plat _tmp
        fname=$(basename "$whl")
        base="${fname%.whl}"

        # Tách 3 trường cuối: python-abi-platform (dùng bash, không cần rev)
        plat="${base##*-}"
        _tmp="${base%-*}"
        _abi="${_tmp##*-}"
        _tmp="${_tmp%-*}"
        pytag="${_tmp##*-}"

        # Pure-Python wheels — bỏ qua
        [[ "$plat" == "any" ]] && continue

        # ── Kiểm tra kiến trúc ────────────────────────────────────────────────
        if [[ "$plat" != *"$SYS_ARCH"* ]]; then
            fail "Wheel arch không khớp: $fname"
            fail "  Wheel: $plat  |  Server: $SYS_ARCH"
            compat_errors=$(( compat_errors + 1 ))
            continue
        fi

        # ── Kiểm tra Python tag (cp310, cp311, cp312) ─────────────────────────
        # abi3 wheels (cp39-abi3, cp311-abi3, ...) tương thích với mọi Python ≥ tag version
        if [[ "$_abi" == "abi3" ]]; then
            : # bỏ qua — stable ABI wheel, compatible với Python ≥ tag
        elif [[ "$pytag" == cp3* && "$pytag" != "$PYTHON_CP_TAG" ]]; then
            fail "Wheel Python không khớp: $fname"
            fail "  Wheel: $pytag  |  Server Python: $PYTHON_CP_TAG"
            fail "  Tải lại wheel với đúng Python version hoặc đổi Python trên server"
            compat_errors=$(( compat_errors + 1 ))
            continue
        fi

        # ── Kiểm tra glibc qua manylinux tag ─────────────────────────────────
        if [[ "$plat" == manylinux_*_* ]]; then
            # manylinux_2_17_x86_64 → cần glibc >= 2.17
            local req_minor
            req_minor=$(echo "$plat" | sed 's/.*manylinux_[0-9]*_\([0-9]*\).*/\1/' || echo 0)
            if (( GLIBC_MINOR < req_minor )); then
                fail "glibc không đủ cho: $fname"
                fail "  Cần glibc ≥ 2.$req_minor  |  Server có $GLIBC_VER"
                compat_errors=$(( compat_errors + 1 ))
                continue
            fi
        elif [[ "$plat" == manylinux2014_* ]]; then
            if (( GLIBC_MINOR < 17 )); then
                fail "glibc không đủ cho: $fname (manylinux2014 cần ≥ 2.17)"
                compat_errors=$(( compat_errors + 1 ))
                continue
            fi
        elif [[ "$plat" == manylinux1_* ]]; then
            if (( GLIBC_MINOR < 5 )); then
                fail "glibc không đủ cho: $fname (manylinux1 cần ≥ 2.5)"
                compat_errors=$(( compat_errors + 1 ))
                continue
            fi
        fi

        pass "Wheel tương thích: $fname"

    done < <(find "$WHL_DIR" -maxdepth 1 -name "*.whl" | sort)

    [[ $compat_errors -eq 0 ]] && pass "Tất cả compiled wheels tương thích với server"
}

# ── G9: Existing install (pre-upgrade) ───────────────────────────────────────
check_existing_install() {
    info "Kiểm tra cài đặt hiện tại tại $APP_DIR ..."

    local VENV="$APP_DIR/libs/venv"
    if [[ ! -d "$VENV" ]]; then
        fail "Virtual environment không tồn tại: $VENV — chạy install.sh trước"
        return
    fi
    pass "Virtual environment: $VENV"

    local BIN="$VENV/bin/mediapipe-video"
    if [[ ! -x "$BIN" ]]; then
        fail "Executable không tồn tại: $BIN"
        return
    fi

    local installed_ver source_ver
    installed_ver=$("$VENV/bin/pip" show mediapipe-video 2>/dev/null \
        | awk '/^Version:/{print $2}' || echo "unknown")
    source_ver=$(grep '^version' "$SRC_DIR/pyproject.toml" 2>/dev/null \
        | head -1 | sed 's/.*= *"\(.*\)"/\1/' || echo "unknown")
    pass "Installed: mediapipe-video $installed_ver"

    if [[ "$installed_ver" == "$source_ver" ]]; then
        warn "Phiên bản đã cài ($installed_ver) = source ($source_ver) — không có thay đổi code"
    else
        pass "Nâng cấp: $installed_ver → $source_ver"
    fi
}

# ── G10: Runtime readiness ────────────────────────────────────────────────────
check_runtime() {
    info "Kiểm tra runtime tại $APP_DIR ..."

    local CONFIG="$APP_DIR/etc/config.yaml"
    if [[ ! -f "$CONFIG" ]]; then
        fail "Config không tồn tại: $CONFIG"
        return
    fi
    pass "Config: $CONFIG"

    # Model path
    local model_val
    model_val=$(grep -E '^\s*model\s*:' "$CONFIG" \
        | grep -v '#' | head -1 \
        | sed 's/.*model\s*:\s*//;s/#.*//;s/[[:space:]]*$//;s/^["'"'"']//;s/["'"'"']$//')
    if [[ -z "$model_val" || "$model_val" == "null" ]]; then
        fail "mediapipe.model chưa cấu hình — sửa: $CONFIG"
        fail "  mediapipe.model: $APP_DIR/models/selfie_segmenter.tflite"
    elif [[ ! -f "$model_val" ]]; then
        fail "Model file không tồn tại: $model_val"
        fail "  Copy file .tflite vào: $APP_DIR/models/"
    elif [[ "$model_val" != *.tflite ]]; then
        warn "Model không có đuôi .tflite: $model_val"
    else
        local size_kb
        size_kb=$(du -k "$model_val" 2>/dev/null | awk '{print $1}')
        pass "Model: $model_val (${size_kb} KB)"
    fi

    # Executable
    local BIN="$APP_DIR/libs/venv/bin/mediapipe-video"
    [[ -x "$BIN" ]] \
        && pass "Executable: $BIN" \
        || fail "Executable không tồn tại: $BIN"

    # Port
    local addr_val port
    addr_val=$(grep -E '^\s*addr\s*:' "$CONFIG" \
        | grep -v '#' | head -1 \
        | sed 's/.*addr\s*:\s*//;s/#.*//;s/[[:space:]]*$//;s/^["'"'"']//;s/["'"'"']$//')
    port="${addr_val##*:}"
    if [[ -n "$port" ]]; then
        local port_in_use=false
        if command -v ss &>/dev/null; then
            ss -tlnp 2>/dev/null | grep -q ":${port} " && port_in_use=true
        elif command -v netstat &>/dev/null; then
            netstat -tlnp 2>/dev/null | grep -q ":${port} " && port_in_use=true
        else
            warn "Không tìm thấy ss/netstat — bỏ qua kiểm tra port $port"
        fi
        if [[ "$port_in_use" == true ]]; then
            warn "Port $port đang bị chiếm dụng (addr=$addr_val)"
        else
            pass "Port $port khả dụng (addr=$addr_val)"
        fi
    fi

    # PID file
    local PID_FILE="$APP_DIR/logs/mediapipe-video.pid"
    if [[ -f "$PID_FILE" ]]; then
        local pid; pid=$(cat "$PID_FILE")
        if kill -0 "$pid" 2>/dev/null; then
            warn "Ứng dụng đang chạy (PID $pid) — stop trước khi upgrade"
        else
            warn "PID file tồn tại nhưng process $pid không còn — có thể crash"
        fi
    fi
}

# ─────────────────────────────────────────────────────────────────────────────
# MAIN
# ─────────────────────────────────────────────────────────────────────────────
printf '\n'
printf '[%s] Bắt đầu kiểm tra (mode=%s, delegate=%s, offline=%s)\n' \
    "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$MODE" "$DELEGATE" "$OFFLINE"
sep

case "$MODE" in
    install)
        check_system
        check_python
        check_python_venv
        check_system_libs
        check_disk_and_permissions
        check_source_files
        check_gpu
        if [[ "$OFFLINE" == true ]]; then
            check_wheels
            check_wheel_compat
        fi
        ;;
    upgrade)
        check_system
        check_python
        check_python_venv
        check_source_files
        check_existing_install
        if [[ "$OFFLINE" == true ]]; then
            check_wheels
            check_wheel_compat
        fi
        ;;
    runtime)
        check_runtime
        ;;
    all)
        check_system
        check_python
        check_python_venv
        check_system_libs
        check_disk_and_permissions
        check_source_files
        check_gpu
        if [[ "$OFFLINE" == true ]]; then
            check_wheels
            check_wheel_compat
        fi
        if [[ -d "$APP_DIR/libs/venv" ]]; then
            check_existing_install
            check_runtime
        else
            warn "APP_DIR chưa được cài đặt ($APP_DIR/libs/venv không tồn tại)"
        fi
        ;;
esac

sep
printf 'Kết quả: %d passed  %d warning  %d failed\n' \
    "$PASS_COUNT" "$WARN_COUNT" "$FAIL_COUNT"

if (( FAIL_COUNT > 0 )); then
    printf '[KẾT QUẢ] KHÔNG ĐẠT — sửa các mục FAIL ở trên trước khi tiếp tục\n\n'
    exit 1
elif (( WARN_COUNT > 0 )); then
    printf '[KẾT QUẢ] ĐẠT (có cảnh báo) — kiểm tra các mục WARN ở trên\n\n'
    exit 0
else
    printf '[KẾT QUẢ] ĐẠT — sẵn sàng\n\n'
    exit 0
fi
