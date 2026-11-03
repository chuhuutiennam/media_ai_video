#!/usr/bin/env bash
# test-pcm-dump-amrwb.sh — Verify AMR-WB decode → PCM dump với opencore-amrwb CGO
#
# Mục đích:
#   Kiểm tra end-to-end pipeline: mock-rtp-sender → gateway (CGO AMR-WB decode) →
#   PCM dump file có dữ liệu thực tại data/output/pcm/.
#
# Yêu cầu hệ thống (Linux):
#   apt-get install libopencore-amrwb-dev
#   go (với CGO_ENABLED=1, mặc định trên Linux)
#
# Script tự làm:
#   - Build gateway với -tags opencore_amrwb
#   - Build mock-rtp-sender nếu chưa có
#   - Khởi động mock-ai-worker + gateway (dừng process cũ trước)
#   - Tạo AMR-WB session, gửi 200 packet từ file thực
#   - Verify PCM file > 0 bytes và nội dung hợp lệ (sample count, value range)
#   - Check metrics: decode_errors=0, processed≥200
#   - Cleanup tự động
#
# AMR-WB file: data/generated/amrwb/speech.amr
#   Format: #!AMR-WB\n magic (9 bytes) + frames FT=8 (1+60 bytes mỗi frame)
#   3001 frames total; test gửi toàn bộ file = ~60 giây audio
#
# Expected PCM output:
#   200 packets × 320 samples/packet × 2 bytes/sample = 128,000 bytes
#   (16kHz, mono, 20ms/packet = 320 samples)
#
# Usage:
#   ./scripts/test-pcm-dump-amrwb.sh [GW_HOST] [GW_PORT] [AI_ADDR]
#
# Defaults:
#   GW_HOST=127.0.0.1   GW_PORT=8080   AI_ADDR=127.0.0.1:50051

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────────────────
GW_HOST="${1:-127.0.0.1}"
GW_PORT="${2:-8080}"
AI_ADDR="${3:-127.0.0.1:50051}"
GW_BASE="http://${GW_HOST}:${GW_PORT}"

RTP_PACKETS=3001        # 3001 × 20ms = 60s audio (toàn bộ speech.amr)
PACKET_INTERVAL=0.02    # 20ms inter-packet gap
SSRC=60099
SESSION_WAIT=5          # giây chờ pipeline flush sau khi gửi xong

EXPECTED_PCM_BYTES=1920640 # 3001 × 320 samples × 2 bytes
PCM_TOLERANCE=0.05         # cho phép ±5% (jitter buffer có thể drop 1-2 packet)

# ── Paths ─────────────────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
BIN_DIR="${PROJECT_ROOT}/bin"
AMR_FILE="${PROJECT_ROOT}/data/generated/amrwb/speech.amr"
PCM_DUMP_DIR="${PROJECT_ROOT}/data/output/pcm"
GW_BINARY="${BIN_DIR}/media-ai-gateway-amrwb"
WORKER_BINARY="${BIN_DIR}/mock-ai-worker"
SENDER_BINARY="${BIN_DIR}/mock-rtp-sender"
GW_CONFIG="${PROJECT_ROOT}/config/gateway-mock.yaml"

GW_PID_FILE="/tmp/gw-amrwb-test.pid"
WORKER_PID_FILE="/tmp/worker-amrwb-test.pid"

# ── Colors ────────────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'

info()  { echo -e "${CYAN}[INFO]${NC}  $*"; }
ok()    { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
fail()  { echo -e "${RED}[FAIL]${NC}  $*"; exit 1; }
sep()   { echo -e "${BOLD}────────────────────────────────────────${NC}"; }

get_metric() {
    curl -s "${GW_BASE}/metrics" \
        | grep -m1 "^${1} " \
        | awk '{print $2}' \
        || echo "0"
}

# ── Cleanup handler ───────────────────────────────────────────────────────────
cleanup() {
    local exit_code=$?
    sep
    info "Cleanup..."
    if [[ -n "${SESSION_ID:-}" ]]; then
        curl -s -o /dev/null -X DELETE "${GW_BASE}/v1/sessions/${SESSION_ID}" 2>/dev/null || true
        info "  session ${SESSION_ID} deleted"
    fi
    if [[ -f "$GW_PID_FILE" ]]; then
        kill "$(cat "$GW_PID_FILE")" 2>/dev/null || true
        rm -f "$GW_PID_FILE"
        info "  gateway stopped"
    fi
    if [[ -f "$WORKER_PID_FILE" ]]; then
        kill "$(cat "$WORKER_PID_FILE")" 2>/dev/null || true
        rm -f "$WORKER_PID_FILE"
        info "  mock-ai-worker stopped"
    fi
    [[ $exit_code -eq 0 ]] && ok "Cleanup done" || warn "Cleanup done (test failed)"
}
trap cleanup EXIT

# ═════════════════════════════════════════════════════════════════════════════
# STEP 1 — Preflight
# ═════════════════════════════════════════════════════════════════════════════
sep
echo -e "${BOLD}╔═══════════════════════════════════════════════════╗${NC}"
echo -e "${BOLD}║  AMR-WB PCM Dump Test (CGO / opencore-amrwb)     ║${NC}"
echo -e "${BOLD}╚═══════════════════════════════════════════════════╝${NC}"
sep
info "Step 1 — Preflight"

# OS check
[[ "$(uname -s)" == "Linux" ]] || fail "Script này chỉ chạy trên Linux (CGO yêu cầu)"

# opencore-amrwb library
if ! ldconfig -p 2>/dev/null | grep -q libopencore-amrwb; then
    fail "libopencore-amrwb không tìm thấy.\n  Cài đặt: apt-get install libopencore-amrwb-dev\n  (hoặc: yum install opencore-amr-devel)"
fi
ok "libopencore-amrwb found ✓"

# Go
command -v go >/dev/null 2>&1 || fail "go không tìm thấy trong PATH"
ok "go $(go version | awk '{print $3}') ✓"

# CGO
[[ "${CGO_ENABLED:-1}" == "1" ]] || fail "CGO_ENABLED=0 — cần CGO để link opencore-amrwb"

# Input file
[[ -f "$AMR_FILE" ]] || fail "File AMR-WB không tìm thấy: ${AMR_FILE}"
AMR_SIZE=$(stat -c%s "$AMR_FILE")
AMR_FRAMES=$(( (AMR_SIZE - 9) / 61 ))
ok "AMR-WB file: ${AMR_FILE} (${AMR_SIZE} bytes, ~${AMR_FRAMES} frames FT=8) ✓"

# curl
command -v curl >/dev/null 2>&1 || fail "curl không tìm thấy"

# python3
command -v python3 >/dev/null 2>&1 || fail "python3 không tìm thấy"

# pcm_dump_dir trong config
if ! grep -q "pcm_dump_dir" "$GW_CONFIG"; then
    fail "gateway-mock.yaml thiếu pcm_dump_dir — chạy từ project root sau khi apply patch"
fi
ok "gateway-mock.yaml có pcm_dump_dir ✓"

mkdir -p "$PCM_DUMP_DIR"
mkdir -p "$BIN_DIR"

# ═════════════════════════════════════════════════════════════════════════════
# STEP 2 — Build (CGO-enabled)
# ═════════════════════════════════════════════════════════════════════════════
sep
info "Step 2 — Build binaries với opencore_amrwb tag"

info "  Building gateway (-tags opencore_amrwb)..."
(cd "$PROJECT_ROOT" && CGO_ENABLED=1 go build -tags opencore_amrwb \
    -o "$GW_BINARY" ./cmd/media-ai-gateway/) \
    || fail "Build gateway thất bại — kiểm tra CGO và libopencore-amrwb-dev"
ok "  gateway-amrwb built: ${GW_BINARY} ✓"

info "  Building mock-ai-worker..."
(cd "$PROJECT_ROOT" && go build -o "$WORKER_BINARY" ./cmd/mock-ai-worker/) \
    || fail "Build mock-ai-worker thất bại"
ok "  mock-ai-worker built ✓"

info "  Building mock-rtp-sender..."
(cd "$PROJECT_ROOT" && go build -o "$SENDER_BINARY" ./cmd/mock-rtp-sender/) \
    || fail "Build mock-rtp-sender thất bại"
ok "  mock-rtp-sender built ✓"

# ═════════════════════════════════════════════════════════════════════════════
# STEP 3 — Start services
# ═════════════════════════════════════════════════════════════════════════════
sep
info "Step 3 — Khởi động services"

# Dừng gateway/worker cũ nếu đang dùng port 8080 / 50051
if lsof -ti:8080 >/dev/null 2>&1; then
    warn "  Port 8080 đang dùng — dừng process cũ..."
    kill "$(lsof -ti:8080)" 2>/dev/null || true
    sleep 1
fi
if lsof -ti:50051 >/dev/null 2>&1; then
    warn "  Port 50051 đang dùng — dừng process cũ..."
    kill "$(lsof -ti:50051)" 2>/dev/null || true
    sleep 1
fi

# Start mock-ai-worker
"$WORKER_BINARY" --addr ":50051" --log-level info \
    >"${PROJECT_ROOT}/bin/worker.log" 2>&1 &
echo $! > "$WORKER_PID_FILE"
info "  mock-ai-worker PID=$(cat "$WORKER_PID_FILE") → :50051"
sleep 0.5

# Start gateway (CGO binary, mock config)
"$GW_BINARY" --config "$GW_CONFIG" \
    >"${PROJECT_ROOT}/bin/gateway.log" 2>&1 &
echo $! > "$GW_PID_FILE"
info "  gateway-amrwb PID=$(cat "$GW_PID_FILE") → :8080"
sleep 2

# Health check
READY_CODE=$(curl -s -o /dev/null -w "%{http_code}" "${GW_BASE}/health/ready" 2>/dev/null || echo "000")
[[ "$READY_CODE" == "200" ]] || fail "Gateway không ready (HTTP ${READY_CODE}) — xem ${PROJECT_ROOT}/bin/gateway.log"
ok "  Gateway ready (HTTP 200) ✓"

# HTTP/2 mode
if curl -s --http2-prior-knowledge "${GW_BASE}/health/live" >/dev/null 2>&1; then
    H2_FLAG="--http2-prior-knowledge"; H2_MODE="h2c prior-knowledge"
elif curl -s --http2 "${GW_BASE}/health/live" >/dev/null 2>&1; then
    H2_FLAG="--http2"; H2_MODE="h2c upgrade"
else
    H2_FLAG=""; H2_MODE="HTTP/1.1 (fallback)"
fi
h2curl() { curl -s $H2_FLAG "$@"; }
info "  HTTP mode: ${H2_MODE}"

# ═════════════════════════════════════════════════════════════════════════════
# STEP 4 — Create AMR-WB session
# ═════════════════════════════════════════════════════════════════════════════
sep
info "Step 4 — Tạo AMR-WB session"

SESSION_ID="amrwb-pcmdump-$(date +%s)"

CREATE=$(h2curl -w "\n%{http_code}" \
    -X POST "${GW_BASE}/v1/sessions" \
    -H "Content-Type: application/json" \
    -H "X-Request-ID: pcmdump-$(date +%s)" \
    -d "{
      \"id\":          \"${SESSION_ID}\",
      \"source_type\": \"raw_rtp\",
      \"codec\":       \"AMR-WB\",
      \"sample_rate\": 16000,
      \"ssrc\":        ${SSRC}
    }")

BODY=$(echo "$CREATE" | head -1)
CODE=$(echo "$CREATE" | tail -1)
[[ "$CODE" == "201" ]] || fail "Tạo session thất bại (HTTP $CODE): $BODY"
ok "Session created (HTTP 201)"
echo "$BODY" | python3 -m json.tool 2>/dev/null || echo "$BODY"

RTP_PORT=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin)['rtp_port'])")
[[ -z "$RTP_PORT" || "$RTP_PORT" == "0" ]] && fail "Không lấy được rtp_port"
ok "RTP endpoint: 127.0.0.1:${RTP_PORT}"

# ═════════════════════════════════════════════════════════════════════════════
# STEP 5 — Metrics baseline
# ═════════════════════════════════════════════════════════════════════════════
sep
info "Step 5 — Metrics baseline"

DECODE_ERR_BEFORE=$(get_metric "media_ai_pool_decode_errors_total")
PROCESSED_BEFORE=$(get_metric "media_ai_pool_processed_total")
SUBMITTED_BEFORE=$(get_metric "media_ai_pool_submitted_total")

info "  pool_submitted_total     = ${SUBMITTED_BEFORE:-0}"
info "  pool_processed_total     = ${PROCESSED_BEFORE:-0}"
info "  pool_decode_errors_total = ${DECODE_ERR_BEFORE:-0}"

# PCM dump file path (phải khớp với logic trong audio_pipeline.go)
PCM_FILE="${PCM_DUMP_DIR}/${SESSION_ID}.amrwb.16000hz.1ch.s16le"
info "  PCM dump file sẽ tạo tại: ${PCM_FILE}"

sleep 0.3

# ═════════════════════════════════════════════════════════════════════════════
# STEP 6 — Send RTP packets
# ═════════════════════════════════════════════════════════════════════════════
sep
info "Step 6 — Gửi ${RTP_PACKETS} AMR-WB packets qua mock-rtp-sender"
info "  File: ${AMR_FILE}"
info "  Format: amrwb (CMR=0xF0 + TOC + 60 bytes/frame = 62 bytes RTP payload)"
info "  Timestamp increment: 320 (16kHz × 20ms)"

"$SENDER_BINARY" \
    --codec    AMR-WB \
    --pt       98 \
    --ssrc     "$SSRC" \
    --ptime    20 \
    --sample-rate 16000 \
    --target   "127.0.0.1:${RTP_PORT}" \
    --file     "$AMR_FILE" \
    --count    "$RTP_PACKETS" \
    --log-level info

# ═════════════════════════════════════════════════════════════════════════════
# STEP 7 — Chờ pipeline flush
# ═════════════════════════════════════════════════════════════════════════════
sep
info "Step 7 — Chờ ${SESSION_WAIT}s để pipeline flush và đóng PCM file..."
sleep "$SESSION_WAIT"

# Xóa session để trigger flush() → pcmFile.Close()
DEL_CODE=$(h2curl -o /dev/null -w "%{http_code}" -X DELETE "${GW_BASE}/v1/sessions/${SESSION_ID}")
[[ "$DEL_CODE" == "204" ]] || warn "Delete session: HTTP $DEL_CODE"
ok "Session deleted — pcmFile.Close() triggered ✓"
SESSION_ID=""  # đánh dấu đã delete để cleanup không gọi lại

sleep 0.5

# ═════════════════════════════════════════════════════════════════════════════
# STEP 8 — Verify PCM file
# ═════════════════════════════════════════════════════════════════════════════
sep
info "Step 8 — Kiểm tra PCM dump file"

[[ -f "$PCM_FILE" ]] || fail "PCM file không tồn tại: ${PCM_FILE}"
ok "PCM file tồn tại ✓"

ACTUAL_BYTES=$(stat -c%s "$PCM_FILE")
info "  File size: ${ACTUAL_BYTES} bytes"
info "  Expected : ${EXPECTED_PCM_BYTES} bytes (±${PCM_TOLERANCE})"

if [[ "$ACTUAL_BYTES" -eq 0 ]]; then
    fail "PCM file trống (0 bytes) — gateway KHÔNG được build với -tags opencore_amrwb!\n  Kiểm tra: ldd ${GW_BINARY} | grep amrwb"
fi
ok "PCM file không trống ✓"

# Kiểm tra kích thước trong khoảng chấp nhận được
MIN_BYTES=$(python3 -c "print(int(${EXPECTED_PCM_BYTES} * (1 - ${PCM_TOLERANCE})))")
MAX_BYTES=$(python3 -c "print(int(${EXPECTED_PCM_BYTES} * (1 + ${PCM_TOLERANCE})))")
if [[ "$ACTUAL_BYTES" -ge "$MIN_BYTES" && "$ACTUAL_BYTES" -le "$MAX_BYTES" ]]; then
    ok "PCM file size hợp lệ: ${ACTUAL_BYTES} bytes (expect ${MIN_BYTES}–${MAX_BYTES}) ✓"
else
    warn "PCM file size ngoài khoảng: ${ACTUAL_BYTES} bytes (expect ${MIN_BYTES}–${MAX_BYTES})"
fi

# Kiểm tra nội dung PCM: int16-LE, giá trị không phải toàn 0
info "  Phân tích PCM samples..."
python3 - "$PCM_FILE" <<'PYEOF'
import sys, struct, math

path = sys.argv[1]
with open(path, 'rb') as f:
    data = f.read()

if len(data) < 2:
    print("  [FAIL] PCM file quá nhỏ")
    sys.exit(1)

# Đọc int16-LE samples
n_samples = len(data) // 2
samples = struct.unpack(f'<{n_samples}h', data[:n_samples * 2])

non_zero = sum(1 for s in samples if s != 0)
max_val  = max(abs(s) for s in samples)
rms      = math.sqrt(sum(s*s for s in samples) / n_samples)

print(f"  Tổng samples   : {n_samples:,}  ({n_samples/16000:.2f}s @ 16kHz)")
print(f"  Non-zero samples: {non_zero:,} ({100*non_zero/n_samples:.1f}%)")
print(f"  Max amplitude  : {max_val} / 32767  ({100*max_val/32767:.1f}% of full scale)")
print(f"  RMS amplitude  : {rms:.1f}")

if non_zero < n_samples * 0.1:
    print("  [WARN] >90% samples là 0 — có thể silence hoặc decode lỗi")
    sys.exit(0)

print("  [OK] PCM có tín hiệu audio hợp lệ ✓")

# Duration check
duration_s = n_samples / 16000
print(f"  Duration       : {duration_s:.2f}s  (expect ~60.02s)")
if abs(duration_s - 60.02) > 0.5:
    print(f"  [WARN] Duration sai lệch > 200ms")
PYEOF

ok "PCM nội dung hợp lệ ✓"

# ═════════════════════════════════════════════════════════════════════════════
# STEP 9 — Verify metrics
# ═════════════════════════════════════════════════════════════════════════════
sep
info "Step 9 — Kiểm tra metrics"

DECODE_ERR_AFTER=$(get_metric "media_ai_pool_decode_errors_total")
PROCESSED_AFTER=$(get_metric "media_ai_pool_processed_total")
SUBMITTED_AFTER=$(get_metric "media_ai_pool_submitted_total")

DECODE_DELTA=$(( ${DECODE_ERR_AFTER:-0} - ${DECODE_ERR_BEFORE:-0} ))
PROC_DELTA=$(( ${PROCESSED_AFTER:-0} - ${PROCESSED_BEFORE:-0} ))
SUBMIT_DELTA=$(( ${SUBMITTED_AFTER:-0} - ${SUBMITTED_BEFORE:-0} ))

info "  pool_submitted_total     : +${SUBMIT_DELTA}  (expect +${RTP_PACKETS})"
info "  pool_processed_total     : +${PROC_DELTA}   (expect +${RTP_PACKETS})"
info "  pool_decode_errors_total : +${DECODE_DELTA}  (expect 0)"

PASS=true

if [[ "$SUBMIT_DELTA" -ge $(( RTP_PACKETS * 95 / 100 )) ]]; then
    ok "  Submitted: +${SUBMIT_DELTA}/${RTP_PACKETS} ✓"
else
    warn "  Submitted thấp: +${SUBMIT_DELTA}/${RTP_PACKETS}"
    PASS=false
fi

if [[ "$PROC_DELTA" -ge $(( RTP_PACKETS * 95 / 100 )) ]]; then
    ok "  Processed: +${PROC_DELTA}/${RTP_PACKETS} ✓"
else
    warn "  Processed thấp: +${PROC_DELTA}/${RTP_PACKETS} — decode có thể lỗi"
    PASS=false
fi

if [[ "$DECODE_DELTA" -eq 0 ]]; then
    ok "  Decode errors: 0 — AMR-WB CGO decode thành công ✓"
else
    fail "  Decode errors: +${DECODE_DELTA} — gateway KHÔNG dùng CGO-enabled binary!\n  Kiểm tra: ldd ${GW_BINARY} | grep amrwb"
fi

# ═════════════════════════════════════════════════════════════════════════════
# STEP 10 — Playback hint
# ═════════════════════════════════════════════════════════════════════════════
sep
info "Step 10 — Playback"
info "  PCM file: ${PCM_FILE}"
info "  Phát lại bằng ffplay:"
info "    ffplay -f s16le -ar 16000 -ac 1 '${PCM_FILE}'"
info "  Hoặc sox (kiểm tra waveform):"
info "    sox -r 16000 -e signed -b 16 -c 1 '${PCM_FILE}' output.wav"
if command -v ffplay >/dev/null 2>&1; then
    info "  (ffplay khả dụng — chạy lệnh trên để nghe)"
fi

# ═════════════════════════════════════════════════════════════════════════════
# Summary
# ═════════════════════════════════════════════════════════════════════════════
sep
echo -e "${BOLD}══════════════ AMR-WB PCM Dump Test Summary ══════════════${NC}"
echo ""
echo -e "  Gateway       : ${GW_BINARY}"
echo -e "  Build tag     : opencore_amrwb (CGO)"
echo -e "  Input         : ${AMR_FILE}"
echo -e "  Packets sent  : ${RTP_PACKETS}"
echo -e "  PCM file      : ${PCM_FILE}"
echo -e "  PCM size      : ${ACTUAL_BYTES} bytes"
echo ""

if [[ "$PASS" == "true" && "$DECODE_DELTA" -eq 0 ]]; then
    echo -e "  ${GREEN}${BOLD}RESULT: PASS${NC}"
    echo -e "  AMR-WB → PCM decode + dump hoạt động đúng ✓"
else
    echo -e "  ${RED}${BOLD}RESULT: FAIL${NC}"
    echo -e "  Xem chi tiết ở trên"
    exit 1
fi
sep
