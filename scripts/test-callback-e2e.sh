#!/usr/bin/env bash
# test-callback-e2e.sh — Self-contained E2E callback test (DCSF API).
#
# Pipeline được kiểm tra end-to-end:
#   DCSF notify-event ANSWER → gateway tạo 2 sessions (tcore + taccess)
#   DCSF ctrl-result → gateway set callbackUrl per-termination
#   mock-rtp-sender → gateway :tcore_rtp_port + :taccess_rtp_port (song song, PCMU decode)
#       → mock-ai-worker (gRPC) → gateway dispatcher
#       → HTTP/2 callback → mock-callback-server
#
# Script tự làm:
#   1. Build gateway + mock-ai-worker + mock-rtp-sender + mock-callback-server
#   2. Khởi động tất cả services
#   3. notify-event ANSWER → nhận tcore_rtp_port + taccess_rtp_port
#   4. ctrl-result → set callbackUrl per-termination (cả tcore + taccess)
#   5. Gửi 200 RTP packets vào tcore_rtp_port VÀ taccess_rtp_port song song (4s = 8 AudioChunks mỗi stream)
#   6. Chờ ≥2 final callback từ mock-callback-server (1 từ tcore + 1 từ taccess)
#   7. Verify: callback count, gateway metrics, 0 errors
#
# Chạy:
#   bash scripts/test-callback-e2e.sh

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────────────────
GW_PORT=8080
GW_BASE="http://127.0.0.1:${GW_PORT}"

CALLBACK_PORT=9999
CALLBACK_URL="http://127.0.0.1:${CALLBACK_PORT}"

RTP_PACKETS=200      # 200 × 20ms = 4s → 8 AudioChunks @ 500ms
EXPECT_FINAL=2       # mỗi stream (tcore + taccess) sinh ≥1 final → 2 total
CALLBACK_TIMEOUT=30  # giây

CALL_ID="cb-e2e-$(date +%s)"

GW_BIN="./bin/media-ai-gateway-cb"
WORKER_BIN="./bin/mock-ai-worker"
SENDER_BIN="./bin/mock-rtp-sender"
CALLBACK_BIN="./bin/mock-callback-server"
GW_CONFIG="config/gateway-mock.yaml"

GW_PID=""
WORKER_PID=""
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
    [[ -n "$CB_PID" ]]     && kill "$CB_PID"     2>/dev/null && wait "$CB_PID"     2>/dev/null || true
    [[ -n "$GW_PID" ]]     && kill "$GW_PID"     2>/dev/null && wait "$GW_PID"     2>/dev/null || true
    [[ -n "$WORKER_PID" ]] && kill "$WORKER_PID" 2>/dev/null && wait "$WORKER_PID" 2>/dev/null || true
    rm -f "$CALLBACK_LOG"
    [[ "${TEST_RESULT:-FAIL}" == "PASS" ]] && ok "Cleanup done" || warn "Cleanup done (test failed)"
}
trap cleanup EXIT

# ── Header ────────────────────────────────────────────────────────────────────
sep
echo -e "${BOLD}╔═══════════════════════════════════════════════════════╗${NC}"
echo -e "${BOLD}║  Callback E2E Test (RTP → decode → gRPC → callback)  ║${NC}"
echo -e "${BOLD}╚═══════════════════════════════════════════════════════╝${NC}"
sep

# ── Step 1: Preflight ─────────────────────────────────────────────────────────
info "Step 1 — Preflight"
command -v go      >/dev/null 2>&1 || fail "go not found"
command -v curl    >/dev/null 2>&1 || fail "curl not found"
command -v python3 >/dev/null 2>&1 || fail "python3 not found"
ok "  go $(go version | awk '{print $3}') ✓"
grep -q "pcm_dump_dir" "$GW_CONFIG" || fail "gateway-mock.yaml không tìm thấy"
ok "  $GW_CONFIG ✓"

# ── Step 2: Build ─────────────────────────────────────────────────────────────
sep
info "Step 2 — Build binaries"
mkdir -p bin

info "  Building gateway..."
go build -o "$GW_BIN" ./cmd/media-ai-gateway/ 2>&1 || fail "Build gateway thất bại"
ok "    $GW_BIN ✓"

info "  Building mock-ai-worker..."
go build -o "$WORKER_BIN" ./cmd/mock-ai-worker/ 2>&1 || fail "Build mock-ai-worker thất bại"
ok "    $WORKER_BIN ✓"

info "  Building mock-rtp-sender..."
go build -o "$SENDER_BIN" ./cmd/mock-rtp-sender/ 2>&1 || fail "Build mock-rtp-sender thất bại"
ok "    $SENDER_BIN ✓"

info "  Building mock-callback-server..."
go build -o "$CALLBACK_BIN" ./cmd/mock-callback-server/ 2>&1 || fail "Build mock-callback-server thất bại"
ok "    $CALLBACK_BIN ✓"

# ── Step 3: Start services ────────────────────────────────────────────────────
sep
info "Step 3 — Khởi động services"

pkill -9 -f "${GW_BIN##*/}"       2>/dev/null || true
pkill -9 -f "${WORKER_BIN##*/}"   2>/dev/null || true
pkill -9 -f "${CALLBACK_BIN##*/}" 2>/dev/null || true
sleep 0.3

"$WORKER_BIN" --addr :50051 --log-level debug \
    > /tmp/mock-ai-worker-cb.log 2>&1 &
WORKER_PID=$!
info "  mock-ai-worker PID=${WORKER_PID} → :50051"

"$GW_BIN" --config "$GW_CONFIG" \
    > /tmp/gateway-cb.log 2>&1 &
GW_PID=$!
info "  gateway PID=${GW_PID} → :${GW_PORT}"

for i in $(seq 1 20); do
    curl -sf "${GW_BASE}/health/live" >/dev/null 2>&1 && break
    sleep 0.3
done
curl -sf "${GW_BASE}/health/live" >/dev/null 2>&1 || fail "Gateway không khởi động"
ok "  Gateway ready ✓"

if curl -s --http2-prior-knowledge "${GW_BASE}/health/live" >/dev/null 2>&1; then
    H2_FLAG="--http2-prior-knowledge"; H2_MODE="h2c prior-knowledge"
elif curl -s --http2 "${GW_BASE}/health/live" >/dev/null 2>&1; then
    H2_FLAG="--http2"; H2_MODE="h2 upgrade"
else
    H2_FLAG=""; H2_MODE="http/1.1 (no h2 in curl)"
fi
info "  HTTP mode: ${H2_MODE}"
h2curl() { curl -s $H2_FLAG "$@"; }

# ── Step 4: Start mock-callback-server ───────────────────────────────────────
sep
info "Step 4 — Start mock-callback-server trên port ${CALLBACK_PORT}"
info "  expect-final=${EXPECT_FINAL}  timeout=${CALLBACK_TIMEOUT}s"

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
ok "  mock-callback-server PID=${CB_PID} → :${CALLBACK_PORT} ✓"

# ── Step 5: Metrics baseline ──────────────────────────────────────────────────
sep
info "Step 5 — Metrics baseline"

CB_SENT_BEFORE=$(get_metric "media_ai_dispatcher_sent_total")
CB_ERR_BEFORE=$(get_metric "media_ai_dispatcher_send_errors_total")
PROC_BEFORE=$(get_metric "media_ai_pool_processed_total")
AI_RECV_ERR_BEFORE=$(get_metric "media_ai_ai_recv_errors_total")

info "  result_sent_total    = ${CB_SENT_BEFORE}"
info "  result_errors_total  = ${CB_ERR_BEFORE}"
info "  pool_processed_total = ${PROC_BEFORE}"
info "  ai_recv_errors_total = ${AI_RECV_ERR_BEFORE}"

# ── Step 6: notify-event ANSWER → tạo 2 sessions ─────────────────────────────
sep
info "Step 6 — notify-event ANSWER  callId=${CALL_ID}"

NOTIFY=$(h2curl -w "\n%{http_code}" \
    -X POST "${GW_BASE}/v1/vonras/call-sessions/${CALL_ID}/notify-event" \
    -H "Content-Type: application/json" \
    -d "{
      \"callId\": \"${CALL_ID}\",
      \"event\":          \"ANSWER\",
      \"selectedService\": \"speech_to_text\",
      \"direction\":       \"MT\",
      \"role\":            \"terminator\",
      \"bearerCapability\": \"AUDIO\"
    }")

BODY=$(echo "$NOTIFY" | head -1)
CODE=$(echo "$NOTIFY" | tail -1)
[[ "$CODE" == "201" ]] || fail "notify-event ANSWER thất bại (HTTP $CODE): $BODY"
ok "  Sessions created (HTTP 201) — tcore + taccess"
echo "$BODY" | python3 -m json.tool 2>/dev/null || echo "$BODY"

TCORE_PORT=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('tcore_rtp_port',0))")
TACCESS_PORT=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('taccess_rtp_port',0))")
[[ "$TCORE_PORT" -gt 0 ]] || fail "tcore_rtp_port không có trong response"
ok "  tcore_rtp_port   : ${TCORE_PORT}"
ok "  taccess_rtp_port : ${TACCESS_PORT}"

# ── Step 7: ctrl-result → set callbackUrl per-termination ────────────────────
sep
info "Step 7 — ctrl-result  callId=${CALL_ID}"

CTRL=$(h2curl -w "\n%{http_code}" \
    -X POST "${GW_BASE}/v1/vonras/call-sessions/${CALL_ID}/ctrl-result" \
    -H "Content-Type: application/json" \
    -d "{
      \"callId\": \"${CALL_ID}\",
      \"mediaResources\": {
        \"tCore\": {
          \"contextId\": \"ctx-tcore-001\",
          \"termination\": { \"terminationId\": \"term-core-001\" },
          \"callbackUrl\": \"${CALLBACK_URL}\"
        },
        \"tAccess\": {
          \"contextId\": \"ctx-taccess-001\",
          \"termination\": { \"terminationId\": \"term-access-001\" },
          \"callbackUrl\": \"${CALLBACK_URL}\"
        }
      }
    }")

CTRL_BODY=$(echo "$CTRL" | head -1)
CTRL_CODE=$(echo "$CTRL" | tail -1)
[[ "$CTRL_CODE" == "200" ]] || fail "ctrl-result thất bại (HTTP $CTRL_CODE): $CTRL_BODY"
ok "  ctrl-result OK — callbackUrl set cho tcore + taccess ✓"

sleep 0.3

# ── Step 8: Gửi RTP packets vào cả tcore_rtp_port và taccess_rtp_port ────────
sep
info "Step 8 — Gửi ${RTP_PACKETS} PCMU packets → tCore:${TCORE_PORT} và tAccess:${TACCESS_PORT} (song song)"
info "  200 packets × 20ms = 4s → 8 AudioChunks @ 500ms mỗi stream"
info "  mock-ai-worker: partial mỗi 3 chunks, final mỗi 6 chunks → ≥1 final/stream"

PCMU_TMP=$(mktemp)
python3 -c "import sys; sys.stdout.buffer.write(bytes([0x7F]*160)*${RTP_PACKETS})" > "$PCMU_TMP"

"$SENDER_BIN" \
    --codec PCMU --pt 0 --ssrc 77001 \
    --ptime 20 --sample-rate 8000 \
    --file-format raw --frame-size 160 \
    --count $RTP_PACKETS \
    --target "127.0.0.1:${TCORE_PORT}" \
    --file "$PCMU_TMP" \
    > /tmp/rtp-tcore.log 2>&1 &
SENDER_TCORE_PID=$!

"$SENDER_BIN" \
    --codec PCMU --pt 0 --ssrc 77002 \
    --ptime 20 --sample-rate 8000 \
    --file-format raw --frame-size 160 \
    --count $RTP_PACKETS \
    --target "127.0.0.1:${TACCESS_PORT}" \
    --file "$PCMU_TMP" \
    > /tmp/rtp-taccess.log 2>&1 &
SENDER_TACCESS_PID=$!

wait "$SENDER_TCORE_PID"
info "  tCore sender done"
wait "$SENDER_TACCESS_PID"
info "  tAccess sender done"

rm -f "$PCMU_TMP"

# ── Step 9: Chờ callback ──────────────────────────────────────────────────────
sep
info "Step 9 — Chờ mock-callback-server nhận đủ ${EXPECT_FINAL} final callback (timeout ${CALLBACK_TIMEOUT}s)..."

CALLBACK_OK=0
WAIT_START=$(date +%s)
if wait "$CB_PID" 2>/dev/null; then
    ELAPSED=$(( $(date +%s) - WAIT_START ))
    ok "  Callback(s) nhận thành công sau ${ELAPSED}s ✓"
    CALLBACK_OK=1
else
    CB_EXIT=$?
    ELAPSED=$(( $(date +%s) - WAIT_START ))
    if [[ "$CB_EXIT" -eq 1 ]]; then
        warn "  mock-callback-server timeout sau ${ELAPSED}s (exit 1)"
    else
        warn "  mock-callback-server thoát với code=${CB_EXIT} sau ${ELAPSED}s"
    fi
fi
CB_PID=""

# ── Step 10: Phân tích callback log ──────────────────────────────────────────
sep
info "Step 10 — Phân tích callback log"

TOTAL_CB=$(grep -c '"event":"callback"' "$CALLBACK_LOG" 2>/dev/null || echo "0")
FINAL_CB=$(grep '"event":"callback"' "$CALLBACK_LOG" 2>/dev/null | grep -c '"is_final":true' || echo "0")
PARTIAL_CB=$(grep '"event":"callback"' "$CALLBACK_LOG" 2>/dev/null | grep -c '"is_final":false' || echo "0")
SUMMARY=$(grep '"event":"summary"\|"event":"timeout"' "$CALLBACK_LOG" 2>/dev/null | tail -1 || echo "")

info "  Total callbacks : ${TOTAL_CB}"
info "  Final           : ${FINAL_CB}  (expect ≥${EXPECT_FINAL})"
info "  Partial         : ${PARTIAL_CB}"
[[ -n "$SUMMARY" ]] && info "  Server summary  : ${SUMMARY}"

sep
info "  Callback events (full payload):"
grep '"event":"callback"' "$CALLBACK_LOG" 2>/dev/null \
    | python3 -c "
import sys, json
for line in sys.stdin:
    line = line.strip()
    if not line: continue
    try:
        d = json.loads(line)
        kind = 'FINAL  ' if d.get('is_final') else 'partial'
        print(f'    [{kind}]')
        for k, v in d.items():
            if k == 'event': continue
            print(f'      {k:<14} = {json.dumps(v, ensure_ascii=False)}')
        print()
    except Exception:
        print('   ', line)
" || true

# ── Step 11: Xoá session ─────────────────────────────────────────────────────
sep
info "Step 11 — DELETE call-session ${CALL_ID}"
DEL=$(h2curl -o /dev/null -w "%{http_code}" \
    -X DELETE "${GW_BASE}/v1/vonras/call-sessions/${CALL_ID}")
[[ "$DEL" == "204" ]] && ok "  Sessions deleted ✓" || warn "  DELETE HTTP $DEL"
sleep 1

# ── Step 12: Gateway metrics ──────────────────────────────────────────────────
sep
info "Step 12 — Gateway metrics"

CB_SENT_AFTER=$(get_metric "media_ai_dispatcher_sent_total")
CB_ERR_AFTER=$(get_metric "media_ai_dispatcher_send_errors_total")
PROC_AFTER=$(get_metric "media_ai_pool_processed_total")
AI_SEND_ERR=$(get_metric "media_ai_ai_send_errors_total")
AI_RECV_ERR_AFTER=$(get_metric "media_ai_ai_recv_errors_total")

CB_SENT_DELTA=$(( ${CB_SENT_AFTER:-0} - ${CB_SENT_BEFORE:-0} ))
CB_ERR_DELTA=$(( ${CB_ERR_AFTER:-0} - ${CB_ERR_BEFORE:-0} ))
PROC_DELTA=$(( ${PROC_AFTER:-0} - ${PROC_BEFORE:-0} ))
AI_RECV_ERR_DELTA=$(( ${AI_RECV_ERR_AFTER:-0} - ${AI_RECV_ERR_BEFORE:-0} ))

TOTAL_PACKETS=$(( RTP_PACKETS * 2 ))
info "  pool_processed_total     : +${PROC_DELTA}  (expect +${TOTAL_PACKETS})"
info "  result_sent_total        : +${CB_SENT_DELTA}  (expect >0)"
info "  result_send_errors_total : +${CB_ERR_DELTA}  (expect 0)"
info "  ai_send_errors_total     : ${AI_SEND_ERR}"
info "  ai_recv_errors_total     : +${AI_RECV_ERR_DELTA}  (expect 0)"

PROC_EXPECT=$(( TOTAL_PACKETS * 95 / 100 ))  # cho phép drop ≤5%
[[ "$PROC_DELTA" -ge $PROC_EXPECT ]] && ok "  Pipeline: +${PROC_DELTA}/${TOTAL_PACKETS} jobs ✓" || warn "  Pipeline thấp: ${PROC_DELTA}/${TOTAL_PACKETS}"
[[ "$CB_SENT_DELTA"      -gt 0   ]] && ok "  Dispatcher sent: +${CB_SENT_DELTA} ✓"             || warn "  Dispatcher không gửi callback"
[[ "$CB_ERR_DELTA"       -eq 0   ]] && ok "  Callback errors: 0 ✓"                               || warn "  Callback errors: +${CB_ERR_DELTA}"
[[ "$AI_RECV_ERR_DELTA"  -eq 0   ]] && ok "  AI recv errors: 0 ✓"                                || warn "  AI recv errors: +${AI_RECV_ERR_DELTA}"

# ── Summary ───────────────────────────────────────────────────────────────────
sep
echo -e "${BOLD}══════════════ Callback E2E Test Summary ══════════════${NC}"
echo ""
echo -e "  Call ID          : ${CALL_ID}"
echo -e "  Sessions         : ${CALL_ID}-tcore  (port ${TCORE_PORT})"
echo -e "                     ${CALL_ID}-taccess (port ${TACCESS_PORT})"
echo -e "  Callback URL     : ${CALLBACK_URL}  (tcore + taccess)"
echo -e "  RTP streams      : tcore  → ${TCORE_PORT}  (${RTP_PACKETS} packets = 4s)"
echo -e "                     taccess → ${TACCESS_PORT}  (${RTP_PACKETS} packets = 4s)"
echo ""
echo -e "  Callbacks received:"
echo -e "    Total   : ${TOTAL_CB}"
echo -e "    Final   : ${FINAL_CB}  (expect ≥${EXPECT_FINAL})"
echo -e "    Partial : ${PARTIAL_CB}"
echo ""
echo -e "  Gateway:"
echo -e "    Pipeline jobs  : +${PROC_DELTA}/${TOTAL_PACKETS}"
echo -e "    Callback sent  : +${CB_SENT_DELTA}"
echo -e "    Callback errors: +${CB_ERR_DELTA}"
echo -e "    AI send errors : ${AI_SEND_ERR}"
echo -e "    AI recv errors : +${AI_RECV_ERR_DELTA}"
sep

ERRORS=0
[[ "$FINAL_CB"          -ge "$EXPECT_FINAL" ]] || ERRORS=$(( ERRORS + 1 ))
[[ "$CB_ERR_DELTA"      -eq 0               ]] || ERRORS=$(( ERRORS + 1 ))
[[ "$PROC_DELTA"        -ge $PROC_EXPECT     ]] || ERRORS=$(( ERRORS + 1 ))
[[ "$AI_RECV_ERR_DELTA" -eq 0               ]] || ERRORS=$(( ERRORS + 1 ))

if [[ "$ERRORS" -eq 0 ]]; then
    echo -e "  ${GREEN}${BOLD}RESULT: PASS${NC}"
    echo -e "  notify-event → ctrl-result → RTP → decode → gRPC → callback pipeline hoạt động đúng ✓"
    TEST_RESULT="PASS"
else
    echo -e "  ${RED}${BOLD}RESULT: FAIL${NC} (${ERRORS} check(s) failed)"
    TEST_RESULT="FAIL"
fi
sep
