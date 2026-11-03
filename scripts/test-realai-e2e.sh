#!/usr/bin/env bash
# test-realai-e2e.sh — E2E test: G.711 60s + AI thật + HTTP/2 callback (DCSF API)
#
# Pipeline:
#   DCSF notify-event ANSWER → gateway tạo 2 sessions (tcore + taccess)
#   DCSF ctrl-result          → gateway set callbackUrl per-termination
#   mock-rtp-sender           → tcore_rtp_port + taccess_rtp_port (song song, G.711 60s)
#       → AI gRPC thật        → transcript
#       → HTTP/2 callback     → mock-callback-server (daemon, luôn mở suốt test)
#
# Yêu cầu:
#   - Binaries đã build sẵn: ./bin/media-ai-gateway, ./bin/mock-callback-server, ./bin/mock-rtp-sender
#   - AI gRPC server đang chạy và kết nối được từ máy này
#   - File RTP: data/generated/g711/speech.pcmu (3000 frames = 60s PCMU 8kHz 20ms)
#
# Tham số (env):
#   AI_ADDR=host:port     địa chỉ AI gRPC server        (default: 127.0.0.1:50051)
#   GW_PORT=8080          cổng gateway HTTP              (default: 8080)
#   CALLBACK_PORT=9999    cổng mock-callback-server      (default: 9999)
#   EXPECT_FINAL=2        số final tối thiểu để PASS     (default: 2)
#   TIMEOUT=120           timeout chờ callback (giây)    (default: 120)
#
# Chạy:
#   AI_ADDR=10.0.0.1:50051 bash scripts/test-realai-e2e.sh
#   AI_ADDR=10.0.0.1:50051 EXPECT_FINAL=10 TIMEOUT=180 bash scripts/test-realai-e2e.sh

set -euo pipefail

# ── Tham số ───────────────────────────────────────────────────────────────────
AI_ADDR="${AI_ADDR:-127.0.0.1:50051}"
GW_PORT="${GW_PORT:-8080}"
CALLBACK_PORT="${CALLBACK_PORT:-9999}"
EXPECT_FINAL="${EXPECT_FINAL:-2}"
TIMEOUT="${TIMEOUT:-120}"

GW_BASE="http://127.0.0.1:${GW_PORT}"
CALLBACK_URL="http://127.0.0.1:${CALLBACK_PORT}"
RTP_FILE="data/generated/g711/speech.pcmu"
RTP_FRAME_BYTES=160
CALL_ID="realai-e2e-$(date +%s)"

GW_BIN="./bin/media-ai-gateway"
CALLBACK_BIN="./bin/mock-callback-server"
SENDER_BIN="./bin/mock-rtp-sender"

GW_PID=""
CB_PID=""
SENDER_TCORE_PID=""
SENDER_TACCESS_PID=""
GW_CFG=$(mktemp)
CALLBACK_LOG=$(mktemp)
TEST_RESULT="FAIL"

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
    [[ -n "$SENDER_TCORE_PID" ]]   && kill "$SENDER_TCORE_PID"   2>/dev/null || true
    [[ -n "$SENDER_TACCESS_PID" ]] && kill "$SENDER_TACCESS_PID" 2>/dev/null || true
    [[ -n "$CB_PID" ]]  && kill "$CB_PID"  2>/dev/null && wait "$CB_PID"  2>/dev/null || true
    [[ -n "$GW_PID" ]]  && kill "$GW_PID"  2>/dev/null && wait "$GW_PID"  2>/dev/null || true
    rm -f "$GW_CFG" "$CALLBACK_LOG"
    [[ "${TEST_RESULT}" == "PASS" ]] \
        && ok "Cleanup done" \
        || warn "Cleanup done (test failed)"
}
trap cleanup EXIT

# ── Header ────────────────────────────────────────────────────────────────────
sep
echo -e "${BOLD}╔══════════════════════════════════════════════════════════════╗${NC}"
echo -e "${BOLD}║  Real AI E2E Test (G.711 60s → AI thật → HTTP/2 callback)   ║${NC}"
echo -e "${BOLD}╚══════════════════════════════════════════════════════════════╝${NC}"
sep

# ── Step 1: Preflight ─────────────────────────────────────────────────────────
info "Step 1 — Preflight"

[[ -x "$GW_BIN" ]]       || fail "Binary không tồn tại hoặc không executable: $GW_BIN"
[[ -x "$CALLBACK_BIN" ]] || fail "Binary không tồn tại hoặc không executable: $CALLBACK_BIN"
[[ -x "$SENDER_BIN" ]]   || fail "Binary không tồn tại hoặc không executable: $SENDER_BIN"
ok "  Binaries: gateway, mock-callback-server, mock-rtp-sender ✓"

[[ -f "$RTP_FILE" ]] || fail "RTP file không tồn tại: $RTP_FILE"
FILE_SIZE=$(wc -c < "$RTP_FILE" | tr -d ' ')
RTP_PACKETS=$(( FILE_SIZE / RTP_FRAME_BYTES ))
DURATION_SEC=$(( RTP_PACKETS * 20 / 1000 ))
ok "  RTP file: $RTP_FILE  (${FILE_SIZE} bytes → ${RTP_PACKETS} frames = ${DURATION_SEC}s)"

command -v curl    >/dev/null 2>&1 || fail "curl not found"
command -v python3 >/dev/null 2>&1 || fail "python3 not found"
ok "  Tools: curl, python3 ✓"

AI_HOST="${AI_ADDR%:*}"
AI_PORT="${AI_ADDR##*:}"
if python3 -c "
import socket, sys
try:
    s = socket.create_connection(('${AI_HOST}', ${AI_PORT}), timeout=3)
    s.close()
except Exception as e:
    sys.exit(1)
" 2>/dev/null; then
    ok "  AI gRPC: ${AI_ADDR} TCP reachable ✓"
else
    warn "  AI gRPC: ${AI_ADDR} TCP không phản hồi — gateway sẽ retry gRPC sau khi connect"
fi

# ── Step 2: Sinh gateway config ───────────────────────────────────────────────
sep
info "Step 2 — Sinh gateway config  (AI: ${AI_ADDR})"

SESSION_IDLE=$(( TIMEOUT + 120 ))

cat > "$GW_CFG" << YAML
gateway:
  name: "media-ai-gateway"
  id: "gw-realai-test"
  raw_rtp_enabled: true
  webrtc_enabled: false

server:
  http_addr: ":${GW_PORT}"
  shutdown_timeout_sec: 5

rtp:
  listen_addr: ":5004"
  public_ip: "127.0.0.1"
  port_start: 40000
  port_end:   40099

session:
  max_sessions: 10
  idle_timeout_sec: ${SESSION_IDLE}
  gc_interval_sec: 10
  per_session_packet_queue: 512
  per_session_audio_queue:  256
  per_session_result_queue: 128

pipeline:
  audio_worker_count: 4
  audio_job_queue_size: 4096
  jitter_buffer_ms: 60
  max_packet_late_ms: 120
  packet_time_ms: 20

audio:
  output_sample_rate: 16000
  output_channels: 1
  chunk_ms: 500

ai:
  grpc_target: "${AI_ADDR}"
  max_active_streams: 10
  per_stream_queue_size: 64
  send_timeout_ms: 2000
  stream_timeout_sec: ${SESSION_IDLE}
  max_retry: 3
  retry_backoff_ms: 1000
  keepalive_time_sec: 30
  keepalive_timeout_sec: 10

result:
  dispatcher_workers: 4
  queue_size: 1024
  drop_partial_when_full: true
  send_timeout_ms: 5000

callback:
  url: "${CALLBACK_URL}"
  timeout_ms: 5000
  max_retry: 3
  retry_backoff_ms: 200
  read_idle_timeout_ms: 30000
  ping_timeout_ms: 15000

log:
  level: "info"
  format: "text"
  monitor_interval_sec: 30
YAML

ok "  Config written → $GW_CFG"
info "  ai.grpc_target       = ${AI_ADDR}"
info "  session.idle_timeout = ${SESSION_IDLE}s"
info "  callback.url         = ${CALLBACK_URL}  (pre-connect warm-up)"

# ── Step 3: Dựng mock-callback-server (daemon) ────────────────────────────────
sep
info "Step 3 — Dựng mock-callback-server  port=${CALLBACK_PORT}  (daemon mode)"
info "  Server chạy liên tục suốt test — gateway luôn có endpoint để POST callback"
info "  Script tự poll log để đếm final, kill server sau khi xong"

pkill -9 -f "${CALLBACK_BIN##*/}" 2>/dev/null || true
sleep 0.2

# --expect-final 0: daemon mode — không tự thoát, chờ SIGTERM từ cleanup
"$CALLBACK_BIN" \
    --port "$CALLBACK_PORT" \
    --expect-final 0 \
    > "$CALLBACK_LOG" 2>&1 &
CB_PID=$!

for i in $(seq 1 30); do
    grep -q '"event":"ready"' "$CALLBACK_LOG" 2>/dev/null && break
    sleep 0.1
done
grep -q '"event":"ready"' "$CALLBACK_LOG" 2>/dev/null \
    || { cat "$CALLBACK_LOG" >&2; fail "mock-callback-server không khởi động"; }
ok "  mock-callback-server PID=${CB_PID} → :${CALLBACK_PORT} ✓"

# ── Step 4: Start gateway ─────────────────────────────────────────────────────
sep
info "Step 4 — Start gateway"

pkill -9 -f "${GW_BIN##*/}" 2>/dev/null || true
sleep 0.3

"$GW_BIN" --config "$GW_CFG" > /tmp/gateway-realai.log 2>&1 &
GW_PID=$!
info "  gateway PID=${GW_PID} → :${GW_PORT}"

for i in $(seq 1 30); do
    curl -sf "${GW_BASE}/health/live" >/dev/null 2>&1 && break
    sleep 0.3
done
curl -sf "${GW_BASE}/health/live" >/dev/null 2>&1 || {
    tail -20 /tmp/gateway-realai.log >&2
    fail "Gateway không khởi động"
}
ok "  Gateway ready ✓"

if curl -s --http2-prior-knowledge "${GW_BASE}/health/live" >/dev/null 2>&1; then
    H2_FLAG="--http2-prior-knowledge"; H2_MODE="h2c prior-knowledge"
elif curl -s --http2 "${GW_BASE}/health/live" >/dev/null 2>&1; then
    H2_FLAG="--http2"; H2_MODE="h2 upgrade"
else
    H2_FLAG=""; H2_MODE="http/1.1"
fi
info "  HTTP mode: ${H2_MODE}"
h2curl() { curl -s $H2_FLAG "$@"; }

# ── Step 5: Metrics baseline ──────────────────────────────────────────────────
sep
info "Step 5 — Metrics baseline"

CB_SENT_BEFORE=$(get_metric "media_ai_dispatcher_sent_total")
CB_ERR_BEFORE=$(get_metric "media_ai_dispatcher_send_errors_total")
PROC_BEFORE=$(get_metric "media_ai_pool_processed_total")
AI_SEND_ERR_BEFORE=$(get_metric "media_ai_ai_send_errors_total")
AI_RECV_ERR_BEFORE=$(get_metric "media_ai_ai_recv_errors_total")

info "  dispatcher_sent_total  = ${CB_SENT_BEFORE}"
info "  pool_processed_total   = ${PROC_BEFORE}"
info "  ai_send_errors_total   = ${AI_SEND_ERR_BEFORE}"
info "  ai_recv_errors_total   = ${AI_RECV_ERR_BEFORE}"

# ── Step 6: notify-event ANSWER ───────────────────────────────────────────────
sep
info "Step 6 — notify-event ANSWER  callId=${CALL_ID}"

NOTIFY=$(h2curl -w "\n%{http_code}" \
    -X POST "${GW_BASE}/v1/vonras/call-sessions/${CALL_ID}/notify-event" \
    -H "Content-Type: application/json" \
    -d "{
      \"callId\": \"${CALL_ID}\",
      \"event\": \"ANSWER\",
      \"selectedService\": \"speech_to_text\",
      \"direction\": \"MT\",
      \"role\": \"terminator\",
      \"bearerCapability\": \"AUDIO\"
    }")

BODY=$(echo "$NOTIFY" | head -1)
CODE=$(echo "$NOTIFY" | tail -1)
[[ "$CODE" == "201" ]] || fail "notify-event ANSWER thất bại (HTTP $CODE): $BODY"
ok "  Sessions created (HTTP 201)"
echo "$BODY" | python3 -m json.tool 2>/dev/null || echo "$BODY"

TCORE_PORT=$(echo "$BODY"   | python3 -c "import sys,json; print(json.load(sys.stdin).get('tcore_rtp_port',0))")
TACCESS_PORT=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('taccess_rtp_port',0))")
[[ "$TCORE_PORT"   -gt 0 ]] || fail "tcore_rtp_port không có trong response"
[[ "$TACCESS_PORT" -gt 0 ]] || fail "taccess_rtp_port không có trong response"
ok "  tcore_rtp_port   : ${TCORE_PORT}"
ok "  taccess_rtp_port : ${TACCESS_PORT}"

# ── Step 7: ctrl-result ───────────────────────────────────────────────────────
sep
info "Step 7 — ctrl-result  callId=${CALL_ID}"
info "  tCore   callbackUrl = ${CALLBACK_URL}"
info "  tAccess callbackUrl = ${CALLBACK_URL}"

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

CTRL_CODE=$(echo "$CTRL" | tail -1)
CTRL_BODY=$(echo "$CTRL" | head -1)
[[ "$CTRL_CODE" == "200" ]] || fail "ctrl-result thất bại (HTTP $CTRL_CODE): $CTRL_BODY"
ok "  ctrl-result OK — callbackUrl set cho tcore + taccess ✓"

sleep 0.5   # cho gateway hoàn tất H/2 pre-connect tới callback server

# ── Step 8: Gửi RTP song song ─────────────────────────────────────────────────
sep
info "Step 8 — Gửi RTP song song từ $RTP_FILE"
info "  ${RTP_PACKETS} packets × 20ms = ${DURATION_SEC}s — cả 2 stream cùng bắt đầu"
info "  tCore   → 127.0.0.1:${TCORE_PORT}   (SSRC=77001)"
info "  tAccess → 127.0.0.1:${TACCESS_PORT}  (SSRC=77002)"

"$SENDER_BIN" \
    --codec PCMU --pt 0 --ssrc 77001 \
    --ptime 20 --sample-rate 8000 \
    --file-format raw --frame-size "${RTP_FRAME_BYTES}" \
    --count "${RTP_PACKETS}" \
    --target "127.0.0.1:${TCORE_PORT}" \
    --file "$RTP_FILE" \
    > /tmp/rtp-tcore-realai.log 2>&1 &
SENDER_TCORE_PID=$!

"$SENDER_BIN" \
    --codec PCMU --pt 0 --ssrc 77002 \
    --ptime 20 --sample-rate 8000 \
    --file-format raw --frame-size "${RTP_FRAME_BYTES}" \
    --count "${RTP_PACKETS}" \
    --target "127.0.0.1:${TACCESS_PORT}" \
    --file "$RTP_FILE" \
    > /tmp/rtp-taccess-realai.log 2>&1 &
SENDER_TACCESS_PID=$!

info "  tCore   sender PID=${SENDER_TCORE_PID}"
info "  tAccess sender PID=${SENDER_TACCESS_PID}"

wait "$SENDER_TCORE_PID";   SENDER_TCORE_PID="";   ok "  tCore   sender done ✓"
wait "$SENDER_TACCESS_PID"; SENDER_TACCESS_PID=""; ok "  tAccess sender done ✓"

# ── Step 9: Chờ callback ──────────────────────────────────────────────────────
sep
info "Step 9 — Chờ ${EXPECT_FINAL} final callback (timeout ${TIMEOUT}s)"
info "  Poll callback log mỗi giây — server vẫn mở để gateway POST tiếp"

WAIT_START=$(date +%s)
CALLBACK_OK=0
while true; do
    ELAPSED=$(( $(date +%s) - WAIT_START ))
    CUR_FINAL=$(grep '"event":"callback"' "$CALLBACK_LOG" 2>/dev/null \
                | grep -c '"is_final":true' || echo "0")
    if [[ "$CUR_FINAL" -ge "$EXPECT_FINAL" ]]; then
        ok "  Nhận đủ ${CUR_FINAL} final callback sau ${ELAPSED}s ✓"
        CALLBACK_OK=1
        break
    fi
    if [[ "$ELAPSED" -ge "$TIMEOUT" ]]; then
        warn "  Timeout ${TIMEOUT}s — nhận được ${CUR_FINAL}/${EXPECT_FINAL} final"
        break
    fi
    sleep 1
done

# Tắt daemon sau khi poll xong
kill "$CB_PID" 2>/dev/null && wait "$CB_PID" 2>/dev/null || true
CB_PID=""

# ── Step 10: Phân tích callback log ──────────────────────────────────────────
sep
info "Step 10 — Phân tích callback log"

TOTAL_CB=$(grep -c '"event":"callback"'   "$CALLBACK_LOG" 2>/dev/null || echo "0")
FINAL_CB=$(grep '"event":"callback"'      "$CALLBACK_LOG" 2>/dev/null \
           | grep -c '"is_final":true'    || echo "0")
PARTIAL_CB=$(grep '"event":"callback"'    "$CALLBACK_LOG" 2>/dev/null \
             | grep -c '"is_final":false' || echo "0")

info "  Total callbacks : ${TOTAL_CB}"
info "  Final           : ${FINAL_CB}  (expect ≥${EXPECT_FINAL})"
info "  Partial         : ${PARTIAL_CB}"

sep
info "  Callback events:"
grep '"event":"callback"' "$CALLBACK_LOG" 2>/dev/null \
    | python3 -c "
import sys, json
for line in sys.stdin:
    line = line.strip()
    if not line: continue
    try:
        d = json.loads(line)
        kind = 'FINAL  ' if d.get('is_final') else 'partial'
        sid  = d.get('session_id', '?')
        seq  = d.get('seq', '?')
        text = d.get('text', '')
        mr   = d.get('mediaResources')
        print(f'    [{kind}] seq={seq}  sid={sid}')
        if text:
            print(f'      text           = {json.dumps(text, ensure_ascii=False)}')
        if mr:
            print(f'      mediaResources = {json.dumps(mr, ensure_ascii=False)}')
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

CB_SENT_AFTER=$(get_metric    "media_ai_dispatcher_sent_total")
CB_ERR_AFTER=$(get_metric     "media_ai_dispatcher_send_errors_total")
PROC_AFTER=$(get_metric       "media_ai_pool_processed_total")
AI_SEND_ERR_AFTER=$(get_metric "media_ai_ai_send_errors_total")
AI_RECV_ERR_AFTER=$(get_metric "media_ai_ai_recv_errors_total")

CB_SENT_DELTA=$(( ${CB_SENT_AFTER:-0}     - ${CB_SENT_BEFORE:-0} ))
CB_ERR_DELTA=$(( ${CB_ERR_AFTER:-0}       - ${CB_ERR_BEFORE:-0} ))
PROC_DELTA=$(( ${PROC_AFTER:-0}           - ${PROC_BEFORE:-0} ))
AI_SEND_ERR_DELTA=$(( ${AI_SEND_ERR_AFTER:-0} - ${AI_SEND_ERR_BEFORE:-0} ))
AI_RECV_ERR_DELTA=$(( ${AI_RECV_ERR_AFTER:-0} - ${AI_RECV_ERR_BEFORE:-0} ))

TOTAL_PACKETS=$(( RTP_PACKETS * 2 ))
PROC_EXPECT=$(( TOTAL_PACKETS * 95 / 100 ))

info "  pool_processed_total     : +${PROC_DELTA}  (expect ≥${PROC_EXPECT} / ${TOTAL_PACKETS})"
info "  dispatcher_sent_total    : +${CB_SENT_DELTA}  (expect >0)"
info "  dispatcher_send_errors   : +${CB_ERR_DELTA}  (expect 0)"
info "  ai_send_errors_total     : +${AI_SEND_ERR_DELTA}  (expect 0)"
info "  ai_recv_errors_total     : +${AI_RECV_ERR_DELTA}  (expect 0)"

[[ "$PROC_DELTA"        -ge $PROC_EXPECT ]] \
    && ok "  Pipeline: +${PROC_DELTA}/${TOTAL_PACKETS} jobs ✓" \
    || warn "  Pipeline thấp: ${PROC_DELTA}/${TOTAL_PACKETS}"
[[ "$CB_SENT_DELTA"     -gt 0  ]] \
    && ok "  Dispatcher sent: +${CB_SENT_DELTA} ✓" \
    || warn "  Dispatcher không gửi callback nào"
[[ "$CB_ERR_DELTA"      -eq 0  ]] \
    && ok "  Callback errors: 0 ✓" \
    || warn "  Callback errors: +${CB_ERR_DELTA}"
[[ "$AI_SEND_ERR_DELTA" -eq 0  ]] \
    && ok "  AI send errors: 0 ✓" \
    || warn "  AI send errors: +${AI_SEND_ERR_DELTA}"
[[ "$AI_RECV_ERR_DELTA" -eq 0  ]] \
    && ok "  AI recv errors: 0 ✓" \
    || warn "  AI recv errors: +${AI_RECV_ERR_DELTA}"

# ── Summary ───────────────────────────────────────────────────────────────────
sep
echo -e "${BOLD}═══════════════════ Real AI E2E Test Summary ═══════════════════${NC}"
echo ""
echo -e "  Call ID          : ${CALL_ID}"
echo -e "  AI gRPC          : ${AI_ADDR}"
echo -e "  Sessions         : ${CALL_ID}-tcore   (RTP port ${TCORE_PORT})"
echo -e "                     ${CALL_ID}-taccess  (RTP port ${TACCESS_PORT})"
echo -e "  RTP file         : ${RTP_FILE}"
echo -e "  RTP streams      : tcore   → ${TCORE_PORT}  (${RTP_PACKETS} packets = ${DURATION_SEC}s)"
echo -e "                     taccess  → ${TACCESS_PORT}  (${RTP_PACKETS} packets = ${DURATION_SEC}s)"
echo -e "  Callback URL     : ${CALLBACK_URL}  (tcore + taccess)"
echo ""
echo -e "  Callbacks received:"
echo -e "    Total   : ${TOTAL_CB}"
echo -e "    Final   : ${FINAL_CB}  (expect ≥${EXPECT_FINAL})"
echo -e "    Partial : ${PARTIAL_CB}"
echo ""
echo -e "  Gateway:"
echo -e "    Pipeline jobs   : +${PROC_DELTA}/${TOTAL_PACKETS}"
echo -e "    Callback sent   : +${CB_SENT_DELTA}"
echo -e "    Callback errors : +${CB_ERR_DELTA}"
echo -e "    AI send errors  : +${AI_SEND_ERR_DELTA}"
echo -e "    AI recv errors  : +${AI_RECV_ERR_DELTA}"
sep

ERRORS=0
[[ "$FINAL_CB"          -ge "$EXPECT_FINAL" ]] || ERRORS=$(( ERRORS + 1 ))
[[ "$CB_ERR_DELTA"      -eq 0               ]] || ERRORS=$(( ERRORS + 1 ))
[[ "$PROC_DELTA"        -ge $PROC_EXPECT    ]] || ERRORS=$(( ERRORS + 1 ))
[[ "$AI_SEND_ERR_DELTA" -eq 0               ]] || ERRORS=$(( ERRORS + 1 ))
[[ "$AI_RECV_ERR_DELTA" -eq 0               ]] || ERRORS=$(( ERRORS + 1 ))

if [[ "$ERRORS" -eq 0 ]]; then
    echo -e "  ${GREEN}${BOLD}RESULT: PASS${NC}"
    echo -e "  G.711 ${DURATION_SEC}s → AI thật → HTTP/2 callback pipeline hoạt động đúng ✓"
    TEST_RESULT="PASS"
else
    echo -e "  ${RED}${BOLD}RESULT: FAIL${NC} (${ERRORS} check(s) failed)"
    echo ""
    echo -e "  Debug logs:"
    echo -e "    Gateway     : /tmp/gateway-realai.log"
    echo -e "    RTP tCore   : /tmp/rtp-tcore-realai.log"
    echo -e "    RTP tAccess : /tmp/rtp-taccess-realai.log"
    TEST_RESULT="FAIL"
fi
sep
