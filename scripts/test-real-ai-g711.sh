#!/usr/bin/env bash
# test-real-ai-g711.sh — E2E test: G.711 file → real AI → HTTP/2 callback
#
# Flow hoàn chỉnh:
#
#   [script] ──build──► media-ai-gateway, mock-callback-server
#   [gateway đang chạy với real AI]
#
#   Step 1  Health check gateway
#   Step 2  POST /v1/sessions           → tạo PCMU session, nhận rtp_port
#   Step 3  PATCH /v1/sessions/{id}     → gán callback_url + H.248 mediaResources
#   Step 4  Gửi RTP từ data/generated/g711/speech.pcmu → rtp_port
#   Step 5  Chờ real AI trả kết quả → gateway gọi HTTP/2 callback
#   Step 6  Phân tích callback log + metrics
#   Step 7  DELETE session
#
# Yêu cầu:
#   - media-ai-gateway đang chạy, kết nối tới real AI gRPC
#   - curl (nghttp2), python3
#   - Go (nếu cần tự build)
#
# Usage:
#   ./scripts/test-real-ai-g711.sh [HOST] [PORT]
#
# Environment vars:
#   CALLBACK_PORT      Port mock-callback-server lắng nghe  (default: 9999)
#   CALLBACK_HOST      Host mà gateway gọi callback về      (default: 127.0.0.1)
#   RTP_PACKETS        Số packet gửi từ file (0=toàn bộ)    (default: 250, ≈5s audio)
#   EXPECT_FINAL       Số final callback cần nhận            (default: 1)
#   CALLBACK_TIMEOUT   Giây chờ callback từ AI              (default: 60)
#   LANGUAGE           Ngôn ngữ gửi AI                      (default: vi)
#   TASK               Task gửi AI                          (default: transcribe)
#
# Ví dụ gửi toàn bộ file (~58s):
#   RTP_PACKETS=0 CALLBACK_TIMEOUT=90 ./scripts/test-real-ai-g711.sh

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────────────────
GW_HOST="${1:-127.0.0.1}"
GW_PORT="${2:-8080}"
GW_BASE="http://${GW_HOST}:${GW_PORT}"

CALLBACK_PORT="${CALLBACK_PORT:-9999}"
CALLBACK_HOST="${CALLBACK_HOST:-127.0.0.1}"
CALLBACK_URL="http://${CALLBACK_HOST}:${CALLBACK_PORT}"

RTP_PACKETS="${RTP_PACKETS:-250}"   # 0 = đọc toàn bộ file
PACKET_INTERVAL="0.02"             # 20ms/packet = realtime 8kHz
TIMESTAMP_INCR=160                 # 8000Hz × 20ms

EXPECT_FINAL="${EXPECT_FINAL:-1}"
CALLBACK_TIMEOUT="${CALLBACK_TIMEOUT:-60}"

LANGUAGE="${LANGUAGE:-vi}"
TASK="${TASK:-transcribe}"

SESSION_ID="real-ai-g711-$(date +%s)"
SSRC=88001

# Temp files / PIDs
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
    curl -s "${GW_BASE}/metrics" \
        | grep -m1 "^${1} " \
        | awk '{print $2}' \
        || echo "0"
}

# ── Paths ─────────────────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

G711_FILE="${PROJECT_ROOT}/data/generated/g711/speech.pcmu"
GW_BIN="${PROJECT_ROOT}/bin/media-ai-gateway"
CB_BIN="${PROJECT_ROOT}/bin/mock-callback-server"
# Fallback: binary di cùng thư mục project (scp từ Windows)
[[ ! -x "$GW_BIN" ]] && GW_BIN="${PROJECT_ROOT}/media-ai-gateway"
[[ ! -x "$CB_BIN" ]] && CB_BIN="${PROJECT_ROOT}/mock-callback-server"

# ══════════════════════════════════════════════════════════════════════════════
sep
echo -e "${BOLD}╔═══════════════════════════════════════════════════╗${NC}"
echo -e "${BOLD}║  Real AI G.711 E2E Test                           ║${NC}"
echo -e "${BOLD}║  PCMU file → gateway → real AI → H2C callback    ║${NC}"
echo -e "${BOLD}╚═══════════════════════════════════════════════════╝${NC}"
echo ""
info "Gateway : ${GW_BASE}"
info "Callback: ${CALLBACK_URL}"
info "Language: ${LANGUAGE}  task=${TASK}"
info "RTP data: ${G711_FILE}"
info "Timeout : ${CALLBACK_TIMEOUT}s (real AI thường mất 2–10s/segment)"
echo ""

# ── Step 0: Build ─────────────────────────────────────────────────────────────
sep
info "Step 0 — Build binaries"

_build() {
    local bin="$1" pkg="$2" name="$3"
    if [[ -x "$bin" ]]; then
        ok "${name} đã có: ${bin}"
        return 0
    fi
    command -v go >/dev/null 2>&1 || fail "${name} chưa có và Go không có trong PATH"
    info "Building ${name} ..."
    (cd "$PROJECT_ROOT" && go build -mod=vendor -o "$bin" "$pkg") \
        || fail "Build ${name} thất bại"
    ok "${name} built → ${bin}"
}

_build "$GW_BIN"  "./cmd/media-ai-gateway/"    "media-ai-gateway"
_build "$CB_BIN"  "./cmd/mock-callback-server/" "mock-callback-server"

[[ -f "$G711_FILE" ]] || fail "File không tồn tại: ${G711_FILE}"
FILE_SIZE=$(stat -c%s "$G711_FILE" 2>/dev/null || stat -f%z "$G711_FILE")
FILE_PACKETS=$(( FILE_SIZE / 160 ))
FILE_SECS=$(( FILE_PACKETS / 50 ))
info "G.711 file: ${FILE_SIZE} bytes = ${FILE_PACKETS} packets ≈ ${FILE_SECS}s audio"

# Tính số packet thực gửi
if [[ "$RTP_PACKETS" -eq 0 || "$RTP_PACKETS" -gt "$FILE_PACKETS" ]]; then
    RTP_PACKETS=$FILE_PACKETS
fi
SEND_SECS=$(( (RTP_PACKETS * 20) / 1000 ))
CHUNK_EST=$(( (RTP_PACKETS * 20 + 499) / 500 ))  # ~N AudioChunks @ 500ms
info "Gửi: ${RTP_PACKETS} packets = ${SEND_SECS}s audio ≈ ${CHUNK_EST} AudioChunks @ 500ms"

# ── Preflight ─────────────────────────────────────────────────────────────────
sep
info "Preflight checks"

command -v curl    >/dev/null 2>&1 || fail "curl not found"
command -v python3 >/dev/null 2>&1 || fail "python3 not found"

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

# ── Step 1: Health check ──────────────────────────────────────────────────────
sep
info "Step 1 — Health check gateway"

RESP=$(h2curl -w "\n%{http_code}" "${GW_BASE}/health/ready")
CODE=$(echo "$RESP" | tail -1)
BODY=$(echo "$RESP" | head -1)
if [[ "$CODE" != "200" ]]; then
    warn "Gateway not ready (HTTP $CODE): $BODY"
    warn "Đảm bảo media-ai-gateway đang chạy với real AI được cấu hình"
    fail "Gateway không sẵn sàng — dừng test"
fi
ok "Gateway ready (HTTP 200) ✓"

# Kiểm tra AI connection
CONNS=$(h2curl "${GW_BASE}/v1/connections" 2>/dev/null || echo "{}")
AI_STATE=$(echo "$CONNS" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    workers = d.get('ai_workers', [])
    if workers:
        w = workers[0]
        print(f\"{w.get('addr','?')}  state={w.get('state','?')}\")
    else:
        print('no_ai_workers')
except:
    print('unknown')
" 2>/dev/null || echo "unknown")

info "AI connection: ${AI_STATE}"
if echo "$AI_STATE" | grep -qi "READY"; then
    ok "AI gRPC state = READY ✓"
elif echo "$AI_STATE" | grep -qi "no_ai_workers\|unknown"; then
    warn "Không đọc được AI state — tiếp tục test"
else
    warn "AI gRPC state: ${AI_STATE} (chưa READY — kết quả AI có thể chậm)"
fi

# ── Step 2: Metrics baseline ──────────────────────────────────────────────────
sep
info "Step 2 — Metrics baseline"

CB_SENT_BEFORE=$(get_metric "media_ai_result_sent_total")
CB_ERR_BEFORE=$(get_metric "media_ai_result_send_errors_total")
PROC_BEFORE=$(get_metric "media_ai_pool_processed_total")
SUBMIT_BEFORE=$(get_metric "media_ai_pool_submitted_total")
AI_SEND_ERR_BEFORE=$(get_metric "media_ai_ai_send_errors_total")
AI_RECV_ERR_BEFORE=$(get_metric "media_ai_ai_recv_errors_total")

info "  result_sent_total        = ${CB_SENT_BEFORE:-0}"
info "  result_send_errors_total = ${CB_ERR_BEFORE:-0}"
info "  pool_processed_total     = ${PROC_BEFORE:-0}"
info "  pool_submitted_total     = ${SUBMIT_BEFORE:-0}"
info "  ai_send_errors_total     = ${AI_SEND_ERR_BEFORE:-0}"
info "  ai_recv_errors_total     = ${AI_RECV_ERR_BEFORE:-0}"

# ── Step 3: Start mock-callback-server ────────────────────────────────────────
sep
info "Step 3 — Start mock H2C callback server trên port ${CALLBACK_PORT}"
info "  Expect ≥${EXPECT_FINAL} final result trong ${CALLBACK_TIMEOUT}s"
info "  Log: ${CALLBACK_LOG}"

"$CB_BIN" \
    --port "$CALLBACK_PORT" \
    --expect-final "$EXPECT_FINAL" \
    --timeout "${CALLBACK_TIMEOUT}s" \
    > "$CALLBACK_LOG" 2>&1 &
CALLBACK_PID=$!

# Chờ server sẵn sàng
READY_WAIT=0
while ! grep -q '"event":"ready"' "$CALLBACK_LOG" 2>/dev/null; do
    sleep 0.1
    READY_WAIT=$(( READY_WAIT + 1 ))
    if [[ $READY_WAIT -ge 40 ]]; then
        cat "$CALLBACK_LOG" >&2
        fail "mock-callback-server không khởi động sau 4s (PID=${CALLBACK_PID})"
    fi
done
ok "mock-callback-server ready trên port ${CALLBACK_PORT} (PID=${CALLBACK_PID}) ✓"

# ── Step 4: Tạo session ───────────────────────────────────────────────────────
sep
info "Step 4 — POST /v1/sessions"
info "  session_id = ${SESSION_ID}"
info "  codec      = PCMU  sample_rate=8000  ssrc=${SSRC}"
info "  language   = ${LANGUAGE}  task=${TASK}"

CREATE=$(h2curl -w "\n%{http_code}\n%{http_version}" \
    -X POST "${GW_BASE}/v1/sessions" \
    -H "Content-Type: application/json" \
    -H "X-Request-ID: $(date +%s%N)" \
    -d "{
      \"id\":          \"${SESSION_ID}\",
      \"source_type\": \"raw_rtp\",
      \"codec\":       \"PCMU\",
      \"sample_rate\": 8000,
      \"ssrc\":        ${SSRC},
      \"language\":    \"${LANGUAGE}\",
      \"task\":        \"${TASK}\"
    }")

CREATE_BODY=$(echo "$CREATE" | head -1)
CREATE_CODE=$(echo "$CREATE" | tail -2 | head -1)
HTTP_VER=$(echo "$CREATE" | tail -1)

[[ "$CREATE_CODE" == "201" ]] || fail "Create session thất bại (HTTP ${CREATE_CODE}): ${CREATE_BODY}"
ok "Session created (HTTP ${CREATE_CODE}, HTTP/${HTTP_VER}) ✓"
echo "$CREATE_BODY" | python3 -m json.tool 2>/dev/null || echo "$CREATE_BODY"

RTP_IP=$(echo "$CREATE_BODY"   | python3 -c "import sys,json; print(json.load(sys.stdin).get('rtp_ip','127.0.0.1'))")
RTP_PORT=$(echo "$CREATE_BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('rtp_port','0'))")
[[ -z "$RTP_PORT" || "$RTP_PORT" == "0" ]] && fail "Không lấy được rtp_port từ response"
ok "RTP endpoint: ${RTP_IP}:${RTP_PORT}"

sleep 0.3

# ── Step 5: PATCH session — gán callback_url + H.248 mediaResources ──────────
sep
info "Step 5 — PATCH /v1/sessions/${SESSION_ID}"
info "  callback_url  → ${CALLBACK_URL}"
info "  tAccess.endpoint → ${RTP_IP}:${RTP_PORT}  (H.248 termination)"
info "  tCore context  → ctx-core-001 / term-core/1"

PATCH=$(h2curl -w "\n%{http_code}" \
    -X PATCH "${GW_BASE}/v1/sessions/${SESSION_ID}" \
    -H "Content-Type: application/json" \
    -d "{
      \"callback_url\": \"${CALLBACK_URL}\",
      \"mediaResources\": {
        \"tCore\": {
          \"contextId\":     \"ctx-core-001\",
          \"terminationId\": \"term-core/1\",
          \"endpoint\":      \"${GW_HOST}:2944\"
        },
        \"tAccess\": {
          \"contextId\":     \"ctx-access-001\",
          \"terminationId\": \"term-access/1\",
          \"endpoint\":      \"${RTP_IP}:${RTP_PORT}\"
        }
      }
    }")

PATCH_BODY=$(echo "$PATCH" | head -1)
PATCH_CODE=$(echo "$PATCH" | tail -1)

if [[ "$PATCH_CODE" == "200" ]]; then
    ok "PATCH session OK (HTTP 200) ✓"
    echo "$PATCH_BODY" | python3 -m json.tool 2>/dev/null || echo "$PATCH_BODY"
else
    fail "PATCH session thất bại (HTTP ${PATCH_CODE}): ${PATCH_BODY}"
fi

sleep 0.2

# ── Step 6: Gửi RTP từ file ───────────────────────────────────────────────────
sep
info "Step 6 — Đọc ${G711_FILE} và gửi RTP → ${RTP_IP}:${RTP_PORT}"
info "  ${RTP_PACKETS} packets × 160 bytes = ${SEND_SECS}s PCMU audio @ 8kHz"
info "  Interval: ${PACKET_INTERVAL}s (realtime 20ms)"

python3 - <<PYEOF
import socket, struct, time, sys, os

FILE     = "${G711_FILE}"
DEST     = ("${RTP_IP}", ${RTP_PORT})
SSRC     = ${SSRC}
N        = ${RTP_PACKETS}
INTERVAL = ${PACKET_INTERVAL}
TS_INCR  = ${TIMESTAMP_INCR}
PT       = 0   # PCMU payload type

# Đọc toàn bộ file raw PCMU (1 byte/sample, no header)
with open(FILE, 'rb') as f:
    raw = f.read()

# Chia thành frame 160 bytes (= 20ms @ 8kHz)
frames = []
for i in range(0, N * 160, 160):
    chunk = raw[i:i+160]
    if len(chunk) == 0:
        break
    if len(chunk) < 160:
        chunk = chunk + bytes([0x7F] * (160 - len(chunk)))  # pad với silence
    frames.append(chunk)

actual = len(frames)
sock   = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
errors = 0

print(f"  File  : {FILE} ({len(raw)} bytes)")
print(f"  Frames: {actual} × 160 bytes  → {actual * 20}ms audio")
print(f"  Dest  : {DEST[0]}:{DEST[1]}  SSRC={SSRC}  PT={PT}")
print()

t0 = time.monotonic()
for i, payload in enumerate(frames):
    seq = (i + 1) & 0xFFFF
    ts  = (i * TS_INCR) & 0xFFFFFFFF
    # RTP: V=2, P=0, X=0, CC=0, M=0, PT=0 (PCMU)
    hdr = struct.pack('!BBHII', 0x80, PT, seq, ts, SSRC)
    try:
        sock.sendto(hdr + payload, DEST)
    except Exception as e:
        errors += 1
        print(f"  [!] packet {i+1} error: {e}", file=sys.stderr)
    if (i + 1) % 100 == 0 or i == 0:
        elapsed = time.monotonic() - t0
        print(f"  Sent {i+1:4d}/{actual}  seq={seq:5d}  ts={ts:10d}  elapsed={elapsed:.1f}s")
    time.sleep(INTERVAL)

sock.close()
elapsed_total = time.monotonic() - t0
print()
print(f"  Done: {actual - errors}/{actual} packets sent  errors={errors}  time={elapsed_total:.1f}s")
PYEOF

# ── Step 7: Chờ callback từ real AI ──────────────────────────────────────────
sep
info "Step 7 — Chờ real AI xử lý và gateway gửi HTTP/2 callback"
info "  Pipeline: PCMU → decode → resample 16kHz → 500ms chunk → gRPC AI"
info "  Real AI: xử lý segment, trả result → gateway → POST ${CALLBACK_URL}"
info "  Timeout: ${CALLBACK_TIMEOUT}s"
echo ""

WAIT_START=$(date +%s)
if wait "$CALLBACK_PID"; then
    WAIT_END=$(date +%s)
    ELAPSED=$(( WAIT_END - WAIT_START ))
    ok "Callback nhận thành công sau ${ELAPSED}s ✓"
    CALLBACK_OK=1
else
    CALLBACK_OK=0
    WAIT_END=$(date +%s)
    ELAPSED=$(( WAIT_END - WAIT_START ))
    warn "mock-callback-server thoát sau ${ELAPSED}s — có thể timeout hoặc không nhận đủ final"
fi
CALLBACK_PID=""  # đã thoát

# ── Step 8: Phân tích callback log ───────────────────────────────────────────
sep
info "Step 8 — Callback log"

TOTAL_CB=$(grep -c '"event":"callback"' "$CALLBACK_LOG" 2>/dev/null || echo "0")
FINAL_CB=$(grep '"event":"callback"' "$CALLBACK_LOG" 2>/dev/null \
    | grep -c '"is_final":true' || echo "0")
PARTIAL_CB=$(grep '"event":"callback"' "$CALLBACK_LOG" 2>/dev/null \
    | grep -c '"is_final":false' || echo "0")
SUMMARY=$(grep '"event":"summary"\|"event":"timeout"' "$CALLBACK_LOG" 2>/dev/null | tail -1 || echo "")

info "  Total callbacks : ${TOTAL_CB}"
info "  Final           : ${FINAL_CB}  (expect ≥${EXPECT_FINAL})"
info "  Partial         : ${PARTIAL_CB}"
[[ -n "$SUMMARY" ]] && info "  Summary         : ${SUMMARY}"
echo ""

# Hiển thị nội dung transcript
TRANS_COUNT=$(grep '"event":"callback"' "$CALLBACK_LOG" 2>/dev/null | wc -l || echo "0")
if [[ "$TRANS_COUNT" -gt 0 ]]; then
    info "--- Transcript ---"
    grep '"event":"callback"' "$CALLBACK_LOG" 2>/dev/null \
        | python3 -c "
import sys, json
for line in sys.stdin:
    line = line.strip()
    if not line: continue
    try:
        d = json.loads(line)
        final_mark = '[FINAL]  ' if d.get('is_final') else '[partial]'
        text  = d.get('text', '')
        sessid = d.get('session_id', '')
        seq   = d.get('seq', '?')
        ctx_id  = d.get('context_id', '')
        term_id = d.get('termination_id', '')
        h248 = f'  ctx={ctx_id} term={term_id}' if ctx_id else ''
        print(f'  {final_mark} seq={seq}  text={text!r}{h248}')
    except Exception:
        print(' ', line)
" || true
    info "-----------------"
fi

# ── Step 9: Metrics sau test ──────────────────────────────────────────────────
sep
info "Step 9 — Metrics pipeline"
sleep 2  # chờ metrics flush

CB_SENT_AFTER=$(get_metric "media_ai_result_sent_total")
CB_ERR_AFTER=$(get_metric "media_ai_result_send_errors_total")
PROC_AFTER=$(get_metric "media_ai_pool_processed_total")
SUBMIT_AFTER=$(get_metric "media_ai_pool_submitted_total")
AI_SEND_ERR_AFTER=$(get_metric "media_ai_ai_send_errors_total")
AI_RECV_ERR_AFTER=$(get_metric "media_ai_ai_recv_errors_total")
AI_STREAMS_ACTIVE=$(get_metric "media_ai_ai_streams_active")
DECODE_ERR_AFTER=$(get_metric "media_ai_pool_decode_errors_total")

CB_SENT_DELTA=$(( ${CB_SENT_AFTER:-0} - ${CB_SENT_BEFORE:-0} ))
CB_ERR_DELTA=$(( ${CB_ERR_AFTER:-0} - ${CB_ERR_BEFORE:-0} ))
PROC_DELTA=$(( ${PROC_AFTER:-0} - ${PROC_BEFORE:-0} ))
SUBMIT_DELTA=$(( ${SUBMIT_AFTER:-0} - ${SUBMIT_BEFORE:-0} ))
AI_SEND_DELTA=$(( ${AI_SEND_ERR_AFTER:-0} - ${AI_SEND_ERR_BEFORE:-0} ))
AI_RECV_DELTA=$(( ${AI_RECV_ERR_AFTER:-0} - ${AI_RECV_ERR_BEFORE:-0} ))

info "  pool_submitted_total     : +${SUBMIT_DELTA}  (expect ≈+${RTP_PACKETS})"
info "  pool_processed_total     : +${PROC_DELTA}  (expect ≈+${RTP_PACKETS})"
info "  pool_decode_errors_total : ${DECODE_ERR_AFTER:-0}  (expect 0)"
info "  ai_streams_active        : ${AI_STREAMS_ACTIVE}"
info "  ai_send_errors delta     : +${AI_SEND_DELTA}  (expect 0)"
info "  ai_recv_errors delta     : +${AI_RECV_DELTA}  (expect 0)"
info "  result_sent_total delta  : +${CB_SENT_DELTA}"
info "  result_send_errors delta : +${CB_ERR_DELTA}  (expect 0)"

# AI latency từ connections API
CONNS_AFTER=$(h2curl "${GW_BASE}/v1/connections" 2>/dev/null || echo "{}")
AI_LATENCY=$(echo "$CONNS_AFTER" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    workers = d.get('ai_workers', [])
    if workers:
        w = workers[0]
        lat = w.get('latency', {})
        print(f\"last={lat.get('last_ms','?')}ms  avg={lat.get('avg_ms','?')}ms  first_result={lat.get('avg_first_result_ms','?')}ms\")
    else:
        print('no_data')
except:
    print('no_data')
" 2>/dev/null || echo "no_data")
info "  AI latency               : ${AI_LATENCY}"

# ── Step 10: DELETE session ───────────────────────────────────────────────────
sep
info "Step 10 — DELETE /v1/sessions/${SESSION_ID}"

DEL_CODE=$(h2curl -o /dev/null -w "%{http_code}" \
    -X DELETE "${GW_BASE}/v1/sessions/${SESSION_ID}")
[[ "$DEL_CODE" == "204" ]] && ok "Session deleted (HTTP 204) ✓" \
    || warn "DELETE HTTP $DEL_CODE"

# ── Summary ───────────────────────────────────────────────────────────────────
sep
echo -e "${BOLD}═══════════════════════════════════════════════════${NC}"
echo -e "${BOLD}  Real AI G.711 E2E Test — Summary                 ${NC}"
echo -e "${BOLD}═══════════════════════════════════════════════════${NC}"
echo ""
printf "  %-28s %s\n" "Gateway:"         "${GW_BASE}  (${H2_MODE})"
printf "  %-28s %s\n" "Session ID:"      "${SESSION_ID}"
printf "  %-28s %s\n" "RTP endpoint:"    "${RTP_IP}:${RTP_PORT}"
printf "  %-28s %s\n" "Callback URL:"    "${CALLBACK_URL}"
printf "  %-28s %s\n" "Language / Task:" "${LANGUAGE} / ${TASK}"
echo ""
printf "  %-28s %s\n" "Audio sent:"       "${RTP_PACKETS} packets = ${SEND_SECS}s"
printf "  %-28s %s\n" "Pool submitted:"   "+${SUBMIT_DELTA}"
printf "  %-28s %s\n" "Pool processed:"   "+${PROC_DELTA}"
printf "  %-28s %s\n" "AI send errors:"   "+${AI_SEND_DELTA}"
printf "  %-28s %s\n" "AI recv errors:"   "+${AI_RECV_DELTA}"
printf "  %-28s %s\n" "AI latency:"       "${AI_LATENCY}"
echo ""
printf "  %-28s %s\n" "Callbacks total:"  "${TOTAL_CB}"
printf "  %-28s %s\n" "  Final:"          "${FINAL_CB}  (expect ≥${EXPECT_FINAL})"
printf "  %-28s %s\n" "  Partial:"        "${PARTIAL_CB}"
printf "  %-28s %s\n" "Dispatcher sent:"  "+${CB_SENT_DELTA}"
printf "  %-28s %s\n" "Dispatcher errors:""  +${CB_ERR_DELTA}"
echo ""
sep

# Verdict
ERRORS=0
[[ "${SUBMIT_DELTA}" -ge $(( RTP_PACKETS * 9 / 10 )) ]] \
    || { warn "pipeline_submitted thấp: ${SUBMIT_DELTA}/${RTP_PACKETS}"; ERRORS=$(( ERRORS+1 )); }
[[ "${DECODE_ERR_AFTER:-0}" -eq 0 ]] \
    || { warn "decode_errors > 0: ${DECODE_ERR_AFTER}"; ERRORS=$(( ERRORS+1 )); }
[[ "${AI_SEND_DELTA}" -eq 0 ]] \
    || { warn "ai_send_errors: +${AI_SEND_DELTA}"; ERRORS=$(( ERRORS+1 )); }
[[ "${AI_RECV_DELTA}" -eq 0 ]] \
    || { warn "ai_recv_errors: +${AI_RECV_DELTA}"; ERRORS=$(( ERRORS+1 )); }
[[ "${CB_ERR_DELTA}" -eq 0 ]] \
    || { warn "callback_send_errors: +${CB_ERR_DELTA}"; ERRORS=$(( ERRORS+1 )); }
[[ "${FINAL_CB}" -ge "${EXPECT_FINAL}" ]] \
    || { warn "final callback: ${FINAL_CB} < expected ${EXPECT_FINAL}"; ERRORS=$(( ERRORS+1 )); }

if [[ "$ERRORS" -eq 0 ]]; then
    ok "Real AI G.711 E2E test PASSED ✓"
    exit 0
else
    warn "Test INCOMPLETE — ${ERRORS} check(s) failed"
    if [[ "${AI_SEND_DELTA}" -gt 0 || "${AI_RECV_DELTA}" -gt 0 ]]; then
        warn "→ Kiểm tra kết nối real AI tại ${GW_BASE}/v1/connections"
    fi
    if [[ "${FINAL_CB}" -lt "${EXPECT_FINAL}" ]]; then
        warn "→ Real AI chưa trả final result — tăng CALLBACK_TIMEOUT hoặc gửi thêm audio"
        warn "   Gợi ý: RTP_PACKETS=0 CALLBACK_TIMEOUT=90 ./scripts/test-real-ai-g711.sh"
    fi
    exit 1
fi
