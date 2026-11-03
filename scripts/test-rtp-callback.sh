#!/usr/bin/env bash
# test-rtp-callback.sh — E2E test: RTP → decode → gRPC (mock-ai-worker) → HTTP/2 callback
#
# Script này kiểm tra toàn bộ pipeline từ đầu đến cuối, bao gồm chiều trả về:
#
#   RTP packets
#       ↓  (UDP ingress)
#   media-ai-gateway  — decode → resample → 500ms AudioChunk
#       ↓  (gRPC bidirectional stream)
#   mock-ai-worker    — trả về partial mỗi 3 chunks, final mỗi 6 chunks
#       ↓  (HTTP/2 POST callback)
#   mock-callback-server  ← script này kiểm tra ở đây
#
# Yêu cầu:
#   - media-ai-gateway đang chạy (localhost:8080)
#   - mock-ai-worker đang chạy (gRPC worker)
#   - Go (để auto-build mock-callback-server nếu chưa có)
#   - curl (với nghttp2), python3
#
# Usage:
#   ./scripts/test-rtp-callback.sh [HOST] [PORT] [METRICS_PORT]
#
#   Defaults: HOST=127.0.0.1  PORT=8080  METRICS_PORT=<PORT>
#
# Environment vars:
#   CALLBACK_PORT    Port cho mock-callback-server   (default: 9999)
#   CALLBACK_HOST    Host mà gateway gọi callback về  (default: 127.0.0.1)
#   RTP_PACKETS      Số RTP packet gửi                (default: 200, ≈4s = 8 chunks)
#   EXPECT_FINAL     Số final callback cần nhận        (default: 1)
#   CALLBACK_TIMEOUT Giây chờ callback                (default: 30)
#
# Expected results:
#   200 packets × 20ms = 4s audio → 8 AudioChunk @ 500ms
#   mock-ai-worker: partial mỗi 3 chunks, final mỗi 6 chunks
#   → 2 partial + 1 final per 6 chunks → ít nhất 1 final trong 8 chunks

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────────────────
GW_HOST="${1:-127.0.0.1}"
GW_PORT="${2:-8080}"
METRICS_PORT="${3:-${2:-8080}}"
GW_BASE="http://${GW_HOST}:${GW_PORT}"
METRICS_BASE="http://${GW_HOST}:${METRICS_PORT}"

CALLBACK_PORT="${CALLBACK_PORT:-9999}"
CALLBACK_HOST="${CALLBACK_HOST:-127.0.0.1}"
CALLBACK_URL="http://${CALLBACK_HOST}:${CALLBACK_PORT}"

RTP_PACKETS="${RTP_PACKETS:-200}"
PACKET_INTERVAL="0.02"
TIMESTAMP_INCR=160

EXPECT_FINAL="${EXPECT_FINAL:-1}"
CALLBACK_TIMEOUT="${CALLBACK_TIMEOUT:-30}"

SESSION_ID="cb-$(date +%s)"
SSRC=77001
CODEC="PCMU"

# Temp files
CALLBACK_LOG=$(mktemp)
CALLBACK_PID=""
trap _cleanup EXIT

# ── Colors ────────────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'

info()  { echo -e "${CYAN}[INFO]${NC}  $*"; }
ok()    { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
fail()  { echo -e "${RED}[FAIL]${NC}  $*"; exit 1; }
sep()   { echo -e "${BOLD}────────────────────────────────────────${NC}"; }

_cleanup() {
    if [[ -n "$CALLBACK_PID" ]] && kill -0 "$CALLBACK_PID" 2>/dev/null; then
        kill "$CALLBACK_PID" 2>/dev/null || true
        wait "$CALLBACK_PID" 2>/dev/null || true
    fi
    rm -f "$CALLBACK_LOG"
}

get_metric() {
    curl -s "${METRICS_BASE}/metrics" \
        | grep -m1 "^${1} " \
        | awk '{print $2}' \
        || echo "0"
}

# ── Preflight ─────────────────────────────────────────────────────────────────
sep
info "Preflight checks"

command -v curl    >/dev/null 2>&1 || fail "curl not found"
command -v python3 >/dev/null 2>&1 || fail "python3 not found"

# HTTP/2 mode detection
if ! curl --version 2>&1 | grep -qi "HTTP2\|nghttp2"; then
    warn "curl không có HTTP/2 — fallback HTTP/1.1"
    H2_FLAG=""; H2_MODE="HTTP/1.1 (fallback)"
elif curl -s --http2-prior-knowledge "${GW_BASE}/health/live" >/dev/null 2>&1; then
    H2_FLAG="--http2-prior-knowledge"; H2_MODE="h2c prior-knowledge"
else
    H2_FLAG="--http2"; H2_MODE="h2c upgrade"
fi
info "HTTP/2 mode: ${H2_MODE}"

h2curl() { curl -s $H2_FLAG "$@"; }

# Tìm hoặc auto-build mock-callback-server (Go binary)
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
CALLBACK_SERVER="${PROJECT_ROOT}/mock-callback-server"
[[ "$(uname -s)" == MINGW* || "$(uname -s)" == CYGWIN* ]] && CALLBACK_SERVER="${CALLBACK_SERVER}.exe"

if [[ ! -x "$CALLBACK_SERVER" ]]; then
    command -v go >/dev/null 2>&1 || fail "mock-callback-server chưa build và Go không có trong PATH"
    info "Building mock-callback-server..."
    (cd "$PROJECT_ROOT" && go build -o mock-callback-server ./cmd/mock-callback-server/) \
        || fail "Build thất bại — chạy thủ công: cd ${PROJECT_ROOT} && go build ./cmd/mock-callback-server/"
fi
ok "mock-callback-server ready ✓"

# ── Step 1: Health check ──────────────────────────────────────────────────────
sep
info "Step 1 — Health check: ${GW_BASE}/health/ready"

RESP=$(h2curl -w "\n%{http_code}" "${GW_BASE}/health/ready")
CODE=$(echo "$RESP" | tail -1)
[[ "$CODE" == "200" ]] || fail "Gateway not ready (HTTP $CODE)"
ok "Gateway ready ✓"

# ── Step 2: Metrics baseline ──────────────────────────────────────────────────
sep
info "Step 2 — Metrics baseline"

CB_SENT_BEFORE=$(get_metric "media_ai_result_sent_total")
CB_ERR_BEFORE=$(get_metric "media_ai_result_send_errors_total")
PROC_BEFORE=$(get_metric "media_ai_pool_processed_total")

info "  result_sent_total        = ${CB_SENT_BEFORE:-0}"
info "  result_send_errors_total = ${CB_ERR_BEFORE:-0}"
info "  pool_processed_total     = ${PROC_BEFORE:-0}"

# ── Step 3: Start mock callback server ───────────────────────────────────────
sep
info "Step 3 — Start mock H2C callback server trên port ${CALLBACK_PORT}"
info "  Chờ ${EXPECT_FINAL} final result trong ${CALLBACK_TIMEOUT}s"
info "  Log: ${CALLBACK_LOG}"

"$CALLBACK_SERVER" \
    --port "$CALLBACK_PORT" \
    --expect-final "$EXPECT_FINAL" \
    --timeout "${CALLBACK_TIMEOUT}s" \
    > "$CALLBACK_LOG" 2>&1 &
CALLBACK_PID=$!

# Đợi server ready
READY_WAIT=0
while ! grep -q '"event":"ready"' "$CALLBACK_LOG" 2>/dev/null; do
    sleep 0.1
    READY_WAIT=$(( READY_WAIT + 1 ))
    if [[ $READY_WAIT -ge 30 ]]; then
        cat "$CALLBACK_LOG" >&2
        fail "mock-callback-server không khởi động sau 3s (PID=${CALLBACK_PID})"
    fi
done
ok "mock-callback-server ready trên port ${CALLBACK_PORT} (PID=${CALLBACK_PID}) ✓"

# ── Step 4: Tạo session với callback_url ─────────────────────────────────────
sep
info "Step 4 — Tạo session với callback_url=${CALLBACK_URL}"
info "  session_id=${SESSION_ID}  codec=${CODEC}  ssrc=${SSRC}"

CREATE=$(h2curl -w "\n%{http_code}\n%{http_version}" \
    -X POST "${GW_BASE}/v1/sessions" \
    -H "Content-Type: application/json" \
    -H "X-Request-ID: cb-$(date +%s)" \
    -d "{
      \"id\":           \"${SESSION_ID}\",
      \"source_type\":  \"raw_rtp\",
      \"codec\":        \"${CODEC}\",
      \"sample_rate\":  8000,
      \"ssrc\":         ${SSRC},
      \"callback_url\": \"${CALLBACK_URL}\"
    }")

BODY=$(echo "$CREATE" | head -1)
CODE=$(echo "$CREATE" | tail -2 | head -1)
HTTP_VER=$(echo "$CREATE" | tail -1)

[[ "$CODE" == "201" ]] || fail "Create session thất bại (HTTP $CODE): $BODY"
ok "Session created (HTTP ${CODE}, HTTP/${HTTP_VER}) ✓"
echo "$BODY" | python3 -m json.tool 2>/dev/null || echo "$BODY"

RTP_IP=$(echo "$BODY"   | python3 -c "import sys,json; print(json.load(sys.stdin).get('rtp_ip','127.0.0.1'))")
RTP_PORT=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('rtp_port','0'))")
[[ -z "$RTP_PORT" || "$RTP_PORT" == "0" ]] && fail "Không lấy được rtp_port từ response"
ok "RTP endpoint: ${RTP_IP}:${RTP_PORT}"

# Verify callback_url được ghi nhận trong response (nếu có)
CB_IN_RESP=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('callback_url',''))" 2>/dev/null || echo "")
if [[ -n "$CB_IN_RESP" ]]; then
    info "  callback_url in response: ${CB_IN_RESP}"
fi

sleep 0.3

# ── Step 5: Gửi RTP packets ───────────────────────────────────────────────────
sep
info "Step 5 — Gửi ${RTP_PACKETS} PCMU RTP packets → ${RTP_IP}:${RTP_PORT}"
info "  payload: 160 bytes × 0x7F (µ-law silence)"
info "  interval: ${PACKET_INTERVAL}s = 20ms"

python3 - <<PYEOF
import socket, struct, time, sys

DEST     = ("${RTP_IP}", ${RTP_PORT})
SSRC     = ${SSRC}
N        = ${RTP_PACKETS}
INTERVAL = ${PACKET_INTERVAL}
TS_INCR  = ${TIMESTAMP_INCR}
PAYLOAD  = bytes([0x7F] * 160)   # µ-law silence

sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
errors = 0

print(f"  Sending {N} PCMU packets to {DEST[0]}:{DEST[1]} ...")

for i in range(N):
    seq = (i + 1) & 0xFFFF
    ts  = (i * TS_INCR) & 0xFFFFFFFF
    # RTP header: V=2, P=0, X=0, CC=0, M=0, PT=0 (PCMU)
    hdr = struct.pack('!BBHII', 0x80, 0x00, seq, ts, SSRC)
    try:
        sock.sendto(hdr + PAYLOAD, DEST)
    except Exception as e:
        errors += 1
        print(f"  [!] packet {i+1} error: {e}", file=sys.stderr)
    if (i + 1) % 50 == 0:
        print(f"  Sent {i+1}/{N} ...")
    time.sleep(INTERVAL)

sock.close()
print(f"  Done: {N - errors}/{N} sent  errors={errors}")
PYEOF

# ── Step 6: Chờ mock-ai-worker xử lý và gateway gửi callback ─────────────────
sep
info "Step 6 — Chờ pipeline xử lý + callback nhận từ gateway"
info "  200 packets → 8 AudioChunks @ 500ms"
info "  mock-ai-worker: partial mỗi 3 chunks, final mỗi 6 chunks → ≥1 final"
info "  Timeout: ${CALLBACK_TIMEOUT}s"

# Đợi mock-callback-server thoát (exit 0 = nhận đủ, exit 1 = timeout)
WAIT_START=$(date +%s)
if wait "$CALLBACK_PID"; then
    WAIT_END=$(date +%s)
    ELAPSED=$(( WAIT_END - WAIT_START ))
    ok "Callback(s) nhận thành công sau ${ELAPSED}s ✓"
    CALLBACK_OK=1
else
    CALLBACK_OK=0
    warn "mock-callback-server thoát với lỗi — có thể timeout hoặc không nhận được final"
fi
CALLBACK_PID=""  # đã thoát, clear để trap không kill lại

# ── Step 7: Phân tích callback log ───────────────────────────────────────────
sep
info "Step 7 — Phân tích callback log"

TOTAL_CB=$(grep -c '"event":"callback"' "$CALLBACK_LOG" 2>/dev/null || echo "0")
FINAL_CB=$(grep '"event":"callback"' "$CALLBACK_LOG" 2>/dev/null \
    | grep -c '"is_final":true' || echo "0")
PARTIAL_CB=$(grep '"event":"callback"' "$CALLBACK_LOG" 2>/dev/null \
    | grep -c '"is_final":false' || echo "0")

# Lấy summary từ server
SUMMARY=$(grep '"event":"summary"' "$CALLBACK_LOG" 2>/dev/null | tail -1 || echo "")
if [[ -z "$SUMMARY" ]]; then
    SUMMARY=$(grep '"event":"timeout"' "$CALLBACK_LOG" 2>/dev/null | tail -1 || echo "")
fi

info "  Total callbacks : ${TOTAL_CB}"
info "  Final           : ${FINAL_CB}"
info "  Partial         : ${PARTIAL_CB}"
[[ -n "$SUMMARY" ]] && info "  Summary         : ${SUMMARY}"

echo ""
info "--- Callback log ---"
grep '"event":"callback"' "$CALLBACK_LOG" 2>/dev/null \
    | python3 -c "
import sys, json
for line in sys.stdin:
    line = line.strip()
    if not line: continue
    try:
        d = json.loads(line)
        print('  [{event_type}] session={session_id} text={text!r} is_final={is_final} seq={seq}'.format(**d))
    except Exception:
        print(' ', line)
" || true
info "--------------------"

# Kiểm tra kết quả
if [[ "$CALLBACK_OK" == "1" && "$FINAL_CB" -ge "$EXPECT_FINAL" ]]; then
    ok "Nhận ${FINAL_CB} final callback (expect ≥${EXPECT_FINAL}) ✓"
else
    warn "Nhận ${FINAL_CB} final callback, expect ≥${EXPECT_FINAL}"
    warn "Kiểm tra: mock-ai-worker có đang chạy không?"
fi

# Kiểm tra callback dùng HTTP/2 — xem gateway metrics
CB_SENT_AFTER=$(get_metric "media_ai_result_sent_total")
CB_ERR_AFTER=$(get_metric "media_ai_result_send_errors_total")
CB_SENT_DELTA=$(( ${CB_SENT_AFTER:-0} - ${CB_SENT_BEFORE:-0} ))
CB_ERR_DELTA=$(( ${CB_ERR_AFTER:-0} - ${CB_ERR_BEFORE:-0} ))

info ""
info "  result_sent_total delta        : +${CB_SENT_DELTA}"
info "  result_send_errors_total delta : +${CB_ERR_DELTA}"

[[ "$CB_SENT_DELTA" -gt 0 ]] && ok "Gateway đã gửi callback thành công: +${CB_SENT_DELTA} ✓" \
    || warn "result_sent_total không tăng — gateway chưa gửi callback (kiểm tra dispatcher)"
[[ "$CB_ERR_DELTA" -eq 0 ]] && ok "Không có callback error ✓" \
    || warn "callback send errors: +${CB_ERR_DELTA} (kiểm tra connectivity)"

# ── Step 8: Metrics pipeline ─────────────────────────────────────────────────
sep
info "Step 8 — Metrics pipeline"
sleep 2   # chờ metrics flush

PROC_AFTER=$(get_metric "media_ai_pool_processed_total")
AI_STREAMS=$(get_metric "media_ai_ai_streams_active")
AI_SEND_ERR=$(get_metric "media_ai_ai_send_errors_total")
AI_RECV_ERR=$(get_metric "media_ai_ai_recv_errors_total")

PROC_DELTA=$(( ${PROC_AFTER:-0} - ${PROC_BEFORE:-0} ))

info "  pool_processed_total  delta : +${PROC_DELTA}  (expect +200)"
info "  ai_streams_active           : ${AI_STREAMS}"
info "  ai_send_errors_total        : ${AI_SEND_ERR}"
info "  ai_recv_errors_total        : ${AI_RECV_ERR}"

[[ "$PROC_DELTA" -ge 190 ]] && ok "Pipeline decode OK: +${PROC_DELTA}/200 jobs ✓" \
    || warn "Jobs processed thấp: ${PROC_DELTA}/200"

# ── Step 9: Đóng session ─────────────────────────────────────────────────────
sep
info "Step 9 — DELETE session ${SESSION_ID}"

DEL_CODE=$(h2curl -o /dev/null -w "%{http_code}" \
    -X DELETE "${GW_BASE}/v1/sessions/${SESSION_ID}")
[[ "$DEL_CODE" == "204" ]] && ok "Session deleted (HTTP 204) ✓" \
    || warn "Delete HTTP $DEL_CODE"

# ── Summary ───────────────────────────────────────────────────────────────────
sep
echo -e "${BOLD}═══════ Callback E2E Test Summary ═══════${NC}"
echo ""
echo -e "  Gateway          : ${GW_BASE}  (${H2_MODE})"
echo -e "  Session ID       : ${SESSION_ID}"
echo -e "  RTP endpoint     : ${RTP_IP}:${RTP_PORT}"
echo -e "  Callback URL     : ${CALLBACK_URL}"
echo -e ""
echo -e "  RTP packets sent : ${RTP_PACKETS} (~$(( RTP_PACKETS / 25 )) AudioChunks @ 500ms)"
echo -e "  Pipeline jobs    : +${PROC_DELTA}"
echo -e ""
echo -e "  Callbacks received :"
echo -e "    Total   : ${TOTAL_CB}"
echo -e "    Final   : ${FINAL_CB}  (expect ≥${EXPECT_FINAL})"
echo -e "    Partial : ${PARTIAL_CB}"
echo -e ""
echo -e "  Dispatcher sent  : +${CB_SENT_DELTA}"
echo -e "  Dispatcher errors: +${CB_ERR_DELTA}"
echo -e ""
echo -e "  AI stream active : ${AI_STREAMS}  (send_err=${AI_SEND_ERR} recv_err=${AI_RECV_ERR})"
sep

# Final verdict
ERRORS=0
[[ "$FINAL_CB" -ge "$EXPECT_FINAL" ]] || ERRORS=$(( ERRORS + 1 ))
[[ "$CB_ERR_DELTA" -eq 0 ]]           || ERRORS=$(( ERRORS + 1 ))
[[ "$PROC_DELTA" -ge 190 ]]           || ERRORS=$(( ERRORS + 1 ))

if [[ "$ERRORS" -eq 0 ]]; then
    ok "Callback E2E test PASSED ✓"
    exit 0
else
    warn "Callback E2E test INCOMPLETE — ${ERRORS} check(s) failed"
    warn "Đảm bảo mock-ai-worker đang chạy và kết nối với gateway"
    exit 1
fi
