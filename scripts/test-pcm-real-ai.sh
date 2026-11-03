#!/usr/bin/env bash
# test-pcm-real-ai.sh — Test file PCM thật (speech.pcmu) → gateway → AI thật → HTTP/2 callback.
#
# Pipeline:
#   data/generated/g711/speech.pcmu → mock-rtp-sender (RTP/PCMU)
#       → gateway (decode + resample) → AI worker thật (gRPC)
#       → gateway dispatcher → HTTP/2 callback → mock-callback-server
#
# Yêu cầu (khởi động trước khi chạy script):
#   Terminal 1: AI worker đang chạy (vd: ./bin/mock-ai-worker --addr :50051)
#   Terminal 2: gateway đang chạy   (vd: ./bin/media-ai-gateway --config /etc/gateway/gateway-mock.yaml)
#   Binaries đã build: mock-rtp-sender, mock-callback-server
#   curl, python3 có trên PATH
#
# Usage:
#   bash scripts/test-pcm-real-ai.sh
#   GW_PORT=8081 bash scripts/test-pcm-real-ai.sh
#   EXPECT_FINAL=20 CALLBACK_TIMEOUT=180 bash scripts/test-pcm-real-ai.sh
#
# Biến env có thể override:
#   GW_PORT          Gateway HTTP port (default: 8080)
#   CALLBACK_PORT    mock-callback-server port (default: 9999)
#   EXPECT_FINAL     Số final callbacks cần nhận để PASS (default: 1)
#   CALLBACK_TIMEOUT Giây chờ callback (default: 120)
#   PCM_FILE         Path tới file PCMU (default: <project>/data/generated/g711/speech.pcmu)
#   SENDER_BIN       Path tới mock-rtp-sender binary
#   CALLBACK_BIN     Path tới mock-callback-server binary

set -euo pipefail

# ── Resolve project root (scripts/ và data/ là siblings) ─────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"

# ── Config ────────────────────────────────────────────────────────────────────
GW_PORT="${GW_PORT:-8080}"
GW_BASE="http://127.0.0.1:${GW_PORT}"

CALLBACK_PORT="${CALLBACK_PORT:-9999}"
CALLBACK_URL="http://127.0.0.1:${CALLBACK_PORT}"

EXPECT_FINAL="${EXPECT_FINAL:-1}"
CALLBACK_TIMEOUT="${CALLBACK_TIMEOUT:-120}"

PCM_FILE="${PCM_FILE:-$PROJECT_ROOT/data/generated/g711/speech.pcmu}"
FRAME_SIZE=160     # PCMU 8kHz 20ms = 160 bytes/frame
PTIME_MS=20
SAMPLE_RATE=8000
SSRC=88001

SENDER_BIN="${SENDER_BIN:-$PROJECT_ROOT/bin/mock-rtp-sender}"
CALLBACK_BIN="${CALLBACK_BIN:-$PROJECT_ROOT/bin/mock-callback-server}"

CB_PID=""
CALLBACK_LOG=$(mktemp)

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
        | awk '{print $2}' || echo "0"
}

# ── Cleanup ───────────────────────────────────────────────────────────────────
cleanup() {
    sep
    info "Cleanup..."
    [[ -n "$CB_PID" ]] && kill "$CB_PID" 2>/dev/null && wait "$CB_PID" 2>/dev/null || true
    rm -f "$CALLBACK_LOG"
    [[ "${TEST_RESULT:-FAIL}" == "PASS" ]] && ok "Done (PASS)" || warn "Done (FAIL)"
}
trap cleanup EXIT

# ── Header ────────────────────────────────────────────────────────────────────
sep
echo -e "${BOLD}╔══════════════════════════════════════════════════════════╗${NC}"
echo -e "${BOLD}║  PCM Real AI Test  (speech.pcmu → AI thật → callback)   ║${NC}"
echo -e "${BOLD}╚══════════════════════════════════════════════════════════╝${NC}"
sep
info "GW_PORT         = ${GW_PORT}"
info "CALLBACK_PORT   = ${CALLBACK_PORT}"
info "PCM_FILE        = ${PCM_FILE}"
info "EXPECT_FINAL    = ${EXPECT_FINAL}"
info "CALLBACK_TIMEOUT= ${CALLBACK_TIMEOUT}s"
sep

# ── Step 1: Preflight ─────────────────────────────────────────────────────────
info "Step 1 — Preflight"
command -v curl    >/dev/null 2>&1 || fail "curl not found"
command -v python3 >/dev/null 2>&1 || fail "python3 not found"

[[ -f "$SENDER_BIN" ]]   || fail "mock-rtp-sender không tìm thấy: $SENDER_BIN"
[[ -f "$CALLBACK_BIN" ]] || fail "mock-callback-server không tìm thấy: $CALLBACK_BIN"
ok "  $SENDER_BIN ✓"
ok "  $CALLBACK_BIN ✓"

[[ -f "$PCM_FILE" ]] || fail "PCM file không tìm thấy: $PCM_FILE"
PCM_SIZE=$(wc -c < "$PCM_FILE")
PCM_FRAMES=$(( PCM_SIZE / FRAME_SIZE ))
PCM_DURATION_S=$(( PCM_FRAMES * PTIME_MS / 1000 ))
ok "  $PCM_FILE  ${PCM_SIZE} bytes = ${PCM_FRAMES} frames = ${PCM_DURATION_S}s ✓"

curl -sf "${GW_BASE}/health/live" >/dev/null 2>&1 \
    || fail "Gateway không reachable tại ${GW_BASE} — khởi động gateway trước"
ok "  Gateway reachable ✓"

# Phát hiện HTTP/2 support của curl
if curl -s --http2-prior-knowledge "${GW_BASE}/health/live" >/dev/null 2>&1; then
    H2_FLAG="--http2-prior-knowledge"; H2_MODE="h2c prior-knowledge"
elif curl -s --http2 "${GW_BASE}/health/live" >/dev/null 2>&1; then
    H2_FLAG="--http2"; H2_MODE="h2 upgrade"
else
    H2_FLAG=""; H2_MODE="http/1.1"
fi
info "  HTTP mode: ${H2_MODE}"
h2curl() { curl -s $H2_FLAG "$@"; }

# ── Step 2: Start mock-callback-server ───────────────────────────────────────
sep
info "Step 2 — Start mock-callback-server :${CALLBACK_PORT}  expect-final=${EXPECT_FINAL}  timeout=${CALLBACK_TIMEOUT}s"

"$CALLBACK_BIN" \
    --port "$CALLBACK_PORT" \
    --expect-final "$EXPECT_FINAL" \
    --timeout "${CALLBACK_TIMEOUT}s" \
    > "$CALLBACK_LOG" 2>&1 &
CB_PID=$!

for i in $(seq 1 30); do
    grep -q '"event":"ready"' "$CALLBACK_LOG" 2>/dev/null && break
    sleep 0.1
done
grep -q '"event":"ready"' "$CALLBACK_LOG" 2>/dev/null \
    || { cat "$CALLBACK_LOG" >&2; fail "mock-callback-server không khởi động"; }
ok "  mock-callback-server PID=${CB_PID} ✓"

# ── Step 3: Metrics baseline ─────────────────────────────────────────────────
sep
info "Step 3 — Metrics baseline"
SENT_BEFORE=$(get_metric "media_ai_dispatcher_sent_total")
ERR_BEFORE=$(get_metric "media_ai_dispatcher_send_errors_total")
PROC_BEFORE=$(get_metric "media_ai_pool_processed_total")
AI_ERR_BEFORE=$(get_metric "media_ai_ai_recv_errors_total")
info "  dispatcher_sent_total = ${SENT_BEFORE}"
info "  pool_processed_total  = ${PROC_BEFORE}"
info "  ai_recv_errors_total  = ${AI_ERR_BEFORE}"

# ── Step 4: Tạo session ──────────────────────────────────────────────────────
sep
info "Step 4 — Tạo PCMU session  callback_url=${CALLBACK_URL}"

SESSION_ID="pcm-real-$(date +%s)"

CREATE=$(h2curl -w "\n%{http_code}" \
    -X POST "${GW_BASE}/v1/sessions" \
    -H "Content-Type: application/json" \
    -d "{
      \"id\":           \"${SESSION_ID}\",
      \"source_type\":  \"raw_rtp\",
      \"codec\":        \"PCMU\",
      \"sample_rate\":  ${SAMPLE_RATE},
      \"ssrc\":         ${SSRC},
      \"language\":     \"vi\",
      \"task\":         \"transcribe\",
      \"callback_url\": \"${CALLBACK_URL}\"
    }")

BODY=$(echo "$CREATE" | head -1)
CODE=$(echo "$CREATE" | tail -1)
[[ "$CODE" == "201" ]] || fail "Create session thất bại (HTTP $CODE): $BODY"
ok "  Session created (HTTP 201): ${SESSION_ID}"
echo "$BODY" | python3 -m json.tool 2>/dev/null || echo "$BODY"

RTP_PORT=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin)['rtp_port'])")
ok "  RTP endpoint: 127.0.0.1:${RTP_PORT}"

sleep 0.5

# ── Step 5: Gửi speech.pcmu ──────────────────────────────────────────────────
sep
info "Step 5 — Gửi ${PCM_FRAMES} PCMU frames (${PCM_DURATION_S}s) → 127.0.0.1:${RTP_PORT}"
info "  File: ${PCM_FILE}  ${PCM_SIZE} bytes  ${FRAME_SIZE}B/frame  ${PTIME_MS}ms ptime"

"$SENDER_BIN" \
    --codec        PCMU \
    --pt           0 \
    --ssrc         "$SSRC" \
    --ptime        "$PTIME_MS" \
    --sample-rate  "$SAMPLE_RATE" \
    --file-format  raw \
    --frame-size   "$FRAME_SIZE" \
    --target       "127.0.0.1:${RTP_PORT}" \
    --file         "$PCM_FILE" \
    --log-level    info \
    2>&1
ok "  RTP sender hoàn thành ✓"

# ── Step 6: Chờ callback ─────────────────────────────────────────────────────
sep
info "Step 6 — Chờ ${EXPECT_FINAL} final callback(s) (tối đa ${CALLBACK_TIMEOUT}s)"

wait "$CB_PID" && CB_EXIT=0 || CB_EXIT=$?
info "  mock-callback-server exit code: ${CB_EXIT}"

# ── Step 7: Kết quả ──────────────────────────────────────────────────────────
sep
info "Step 7 — Kết quả callbacks"
info "  --- callback log ---"
cat "$CALLBACK_LOG"
info "  ---"

TIMEOUT_EV=$(grep '"event":"timeout"' "$CALLBACK_LOG" | tail -1 || echo "")
[[ -n "$TIMEOUT_EV" ]] && warn "mock-callback-server timeout — chưa nhận đủ ${EXPECT_FINAL} final"

FINAL_COUNT=$(grep '"event":"callback"' "$CALLBACK_LOG" | python3 -c "
import sys, json
count = 0
for line in sys.stdin:
    line = line.strip()
    if not line: continue
    try:
        obj = json.loads(line)
        if obj.get('is_final'): count += 1
    except: pass
print(count)
" 2>/dev/null || echo "0")

PARTIAL_COUNT=$(grep '"event":"callback"' "$CALLBACK_LOG" | python3 -c "
import sys, json
count = 0
for line in sys.stdin:
    line = line.strip()
    if not line: continue
    try:
        obj = json.loads(line)
        if not obj.get('is_final'): count += 1
    except: pass
print(count)
" 2>/dev/null || echo "0")

# ── Step 8: Metrics delta ───────────────────────────────────────────────────
sep
info "Step 8 — Metrics delta"

SENT_AFTER=$(get_metric "media_ai_dispatcher_sent_total")
ERR_AFTER=$(get_metric "media_ai_dispatcher_send_errors_total")
PROC_AFTER=$(get_metric "media_ai_pool_processed_total")
AI_ERR_AFTER=$(get_metric "media_ai_ai_recv_errors_total")

SENT_DELTA=$(( ${SENT_AFTER:-0}     - ${SENT_BEFORE:-0} ))
ERR_DELTA=$(( ${ERR_AFTER:-0}       - ${ERR_BEFORE:-0}  ))
PROC_DELTA=$(( ${PROC_AFTER:-0}     - ${PROC_BEFORE:-0} ))
AI_ERR_DELTA=$(( ${AI_ERR_AFTER:-0} - ${AI_ERR_BEFORE:-0} ))

info "  pool_processed  : +${PROC_DELTA}  (kỳ vọng ≥ ${PCM_FRAMES})"
info "  dispatcher_sent : +${SENT_DELTA}  (= callbacks delivered)"
info "  dispatcher_err  : +${ERR_DELTA}"
info "  ai_recv_errors  : +${AI_ERR_DELTA}"

# ── Step 9: PASS / FAIL ─────────────────────────────────────────────────────
sep
info "Step 9 — Kiểm tra kết quả"

FAIL_REASON=""

[[ "$FINAL_COUNT" -ge "$EXPECT_FINAL" ]] \
    && ok "  final callbacks    : ${FINAL_COUNT} ✓ (≥ ${EXPECT_FINAL})" \
    || { warn "  final callbacks    : ${FINAL_COUNT} ✗ (cần ≥ ${EXPECT_FINAL})"; FAIL_REASON+=" final_count"; }

[[ "$AI_ERR_DELTA" -eq 0 ]] \
    && ok "  ai_recv_errors     : 0 ✓" \
    || warn "  ai_recv_errors     : +${AI_ERR_DELTA}"

[[ "$ERR_DELTA" -eq 0 ]] \
    && ok "  dispatcher_errors  : 0 ✓" \
    || warn "  dispatcher_errors  : +${ERR_DELTA}"

info "  partial callbacks  : ${PARTIAL_COUNT}"
info "  pool_processed     : +${PROC_DELTA}"

sep
if [[ -z "$FAIL_REASON" ]]; then
    TEST_RESULT=PASS
    echo -e "${GREEN}${BOLD}✓ PASS${NC} — ${FINAL_COUNT} final callback(s) nhận được từ AI thật"
else
    TEST_RESULT=FAIL
    echo -e "${RED}${BOLD}✗ FAIL${NC} — ${FAIL_REASON}"
    echo ""
    info "Debug hints:"
    info "  curl -s ${GW_BASE}/metrics | grep media_ai"
    exit 1
fi
