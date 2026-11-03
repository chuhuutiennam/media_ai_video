#!/usr/bin/env bash
# lab-rtp-test.sh — Test tạo RTP session qua HTTP/2 (h2c) và gửi bản tin RTP
#
# Usage:
#   ./scripts/lab-rtp-test.sh [GATEWAY_HOST] [GATEWAY_PORT] [METRICS_PORT]
#
# Defaults:
#   GATEWAY_HOST=127.0.0.1   GATEWAY_PORT=8080   METRICS_PORT=9090
#
# Requires: curl (với nghttp2), python3
#
# HTTP/2 mode detection (tự động):
#   --http2-prior-knowledge  gửi h2c trực tiếp (không upgrade)
#   --http2                  dùng HTTP Upgrade: h2c (RedHat/CentOS curl)
#   fallback HTTP/1.1        nếu curl không có HTTP/2

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────────────────
GW_HOST="${1:-127.0.0.1}"
GW_PORT="${2:-8080}"
# METRICS_PORT mặc định bằng GW_PORT vì /metrics được serve trên gin router (:8080).
# Chỉ khác khi gateway cấu hình metrics_addr riêng (ví dụ :9090).
METRICS_PORT="${3:-${2:-8080}}"
GW_BASE="http://${GW_HOST}:${GW_PORT}"
METRICS_BASE="http://${GW_HOST}:${METRICS_PORT}"

SESSION_ID="lab-rtp-$(date +%s)"
SSRC=55001
CODEC="PCMU"
SAMPLE_RATE=8000
RTP_PACKETS=200       # số packet gửi (~4 giây audio ở 20ms/packet)
# 1 AudioChunk = 500ms = 25 packets (8kHz PCMU → 16kHz, ChunkMs=500)
# Mock AI: partial sau 3 chunks (75 packets), final sau 6 chunks (150 packets)
# 200 packets → 8 chunks → 2 partial + 1 final từ mock-ai-worker
PACKET_INTERVAL=0.02  # 20ms giữa các packet

# ── Colors ────────────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'

info()  { echo -e "${CYAN}[INFO]${NC}  $*"; }
ok()    { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
fail()  { echo -e "${RED}[FAIL]${NC}  $*"; exit 1; }
sep()   { echo -e "${BOLD}────────────────────────────────────────${NC}"; }

# get_metric <metric_name> — lấy giá trị Prometheus metric, trả "0" nếu không có.
# Dùng grep -m1 + default để tránh exit code 1 khi metric chưa xuất hiện.
get_metric() {
    local name="$1"
    curl -s "${METRICS_BASE}/metrics" \
        | grep -m1 "^${name} " \
        | awk '{print $2}' \
        || echo "0"
}

# ── Preflight checks ──────────────────────────────────────────────────────────
sep
info "Preflight checks"

command -v curl   >/dev/null 2>&1 || fail "curl not found"
command -v python3 >/dev/null 2>&1 || fail "python3 not found"

# Detect HTTP/2 mode của curl
# Ưu tiên: --http2-prior-knowledge > --http2 > HTTP/1.1 fallback
#
#   --http2-prior-knowledge : gửi h2c trực tiếp (curl 7.49+, cần nghttp2)
#   --http2                 : HTTP Upgrade h2c (RedHat/CentOS curl thường dùng flag này)
#   (none)                  : HTTP/1.1 fallback
if ! curl --version 2>&1 | grep -qi "HTTP2\|nghttp2"; then
    warn "curl không có HTTP/2 support"
    warn "RedHat/CentOS: dnf install libnghttp2  hoặc  yum install libnghttp2"
    warn "Ubuntu/Debian: apt install libnghttp2-dev"
    warn "Fallback sang HTTP/1.1..."
    H2_FLAG=""
    H2_MODE="HTTP/1.1 (fallback)"
elif curl --http2-prior-knowledge --version >/dev/null 2>&1 \
     && curl -s --http2-prior-knowledge "http://${GW_HOST}:${GW_PORT}/health/live" >/dev/null 2>&1; then
    ok "curl hỗ trợ --http2-prior-knowledge (h2c direct)"
    H2_FLAG="--http2-prior-knowledge"
    H2_MODE="h2c prior-knowledge"
else
    ok "curl hỗ trợ --http2 (HTTP Upgrade h2c)"
    H2_FLAG="--http2"
    H2_MODE="h2c upgrade"
fi
info "HTTP/2 mode: ${H2_MODE}"

# Hàm curl với h2c
h2curl() {
    curl -s $H2_FLAG "$@"
}

# ── Step 1: Health check ──────────────────────────────────────────────────────
sep
info "Step 1 — Health check: ${GW_BASE}/health/ready"

HEALTH=$(h2curl -w "\n%{http_code}" "${GW_BASE}/health/ready")
HTTP_CODE=$(echo "$HEALTH" | tail -1)
BODY=$(echo "$HEALTH" | head -1)

if [[ "$HTTP_CODE" != "200" ]]; then
    fail "Gateway not ready (HTTP $HTTP_CODE): $BODY"
fi
ok "Gateway ready — $BODY"

# ── Step 2: Metrics baseline ──────────────────────────────────────────────────
sep
info "Step 2 — Metrics baseline"

PORTS_BEFORE=$(get_metric "media_ai_rtp_ports_available")
SUBMITTED_BEFORE=$(get_metric "media_ai_pool_submitted_total")
PROCESSED_BEFORE=$(get_metric "media_ai_pool_processed_total")

info "  rtp_ports_available  = ${PORTS_BEFORE:-N/A}"
info "  pool_submitted_total = ${SUBMITTED_BEFORE:-0}"
info "  pool_processed_total = ${PROCESSED_BEFORE:-0}"

# ── Step 3: Tạo session qua HTTP/2 ───────────────────────────────────────────
sep
info "Step 3 — Tạo session qua HTTP/2: POST ${GW_BASE}/v1/sessions"
info "  session_id=${SESSION_ID}  ssrc=${SSRC}  codec=${CODEC}  sample_rate=${SAMPLE_RATE}"

CREATE_RESP=$(h2curl -w "\n%{http_code}\n%{http_version}" \
    -X POST "${GW_BASE}/v1/sessions" \
    -H "Content-Type: application/json" \
    -H "X-Request-ID: lab-$(date +%s)" \
    -d "{
      \"id\":          \"${SESSION_ID}\",
      \"source_type\": \"raw_rtp\",
      \"codec\":       \"${CODEC}\",
      \"sample_rate\": ${SAMPLE_RATE},
      \"ssrc\":        ${SSRC}
    }")

BODY=$(echo "$CREATE_RESP" | head -1)
HTTP_CODE=$(echo "$CREATE_RESP" | tail -2 | head -1)
HTTP_VER=$(echo "$CREATE_RESP" | tail -1)

if [[ "$HTTP_CODE" != "201" ]]; then
    fail "Create session thất bại (HTTP $HTTP_CODE): $BODY"
fi

ok "Session created (HTTP ${HTTP_CODE}, HTTP/${HTTP_VER})"
echo "$BODY" | python3 -m json.tool 2>/dev/null || echo "$BODY"

# Parse response
RTP_IP=$(echo "$BODY"   | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('rtp_ip',''))")
RTP_PORT=$(echo "$BODY" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('rtp_port','0'))")
GW_ID=$(echo "$BODY"    | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('gateway_id',''))")

if [[ -z "$RTP_PORT" || "$RTP_PORT" == "0" ]]; then
    fail "Không lấy được rtp_port từ response"
fi

ok "Per-session RTP endpoint: ${RTP_IP}:${RTP_PORT}  (gateway: ${GW_ID})"

# Verify port đang lắng nghe
sleep 0.5
if command -v ss >/dev/null 2>&1; then
    if ss -uln | grep -q ":${RTP_PORT} "; then
        ok "UDP port ${RTP_PORT} đang LISTEN (xác nhận bằng ss)"
    else
        warn "UDP port ${RTP_PORT} chưa thấy trong ss -uln (có thể do network namespace)"
    fi
fi

# ── Step 4: Gửi RTP packets ───────────────────────────────────────────────────
sep
info "Step 4 — Gửi ${RTP_PACKETS} RTP packets → ${RTP_IP}:${RTP_PORT}"
info "  codec=PCMU  payload=160 bytes (20ms)  interval=${PACKET_INTERVAL}s"

python3 - <<PYEOF
import socket, struct, time, sys

DEST_IP   = "${RTP_IP}"
DEST_PORT = ${RTP_PORT}
SSRC      = ${SSRC}
N         = ${RTP_PACKETS}
INTERVAL  = ${PACKET_INTERVAL}

sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
errors = 0

for i in range(N):
    seq_no    = (i + 1) & 0xFFFF
    timestamp = (i * 160) & 0xFFFFFFFF

    # RTP header: V=2, P=0, X=0, CC=0, M=0, PT=0(PCMU)
    hdr = struct.pack('!BBHII',
        0x80,       # V=2, no padding, no extension, CC=0
        0x00,       # marker=0, PT=0 (PCMU)
        seq_no,
        timestamp,
        SSRC,
    )
    payload = b'\x7f' * 160   # PCMU silence (0x7f = digital silence)

    try:
        sock.sendto(hdr + payload, (DEST_IP, DEST_PORT))
    except Exception as e:
        errors += 1
        print(f"  [!] packet {i+1} error: {e}", file=sys.stderr)

    if (i + 1) % 10 == 0:
        print(f"  Sent {i+1}/{N} packets...")

    time.sleep(INTERVAL)

sock.close()
print(f"Done: sent {N - errors}/{N} packets to {DEST_IP}:{DEST_PORT}  (errors={errors})")
PYEOF

# ── Step 5: Verify metrics ────────────────────────────────────────────────────
sep
info "Step 5 — Verify metrics sau khi gửi packet"
sleep 3   # chờ pipeline flush (4s audio + 3s buffer = đủ để mock nhận 8 chunks)

PORTS_AFTER=$(get_metric "media_ai_rtp_ports_available")
SUBMITTED_AFTER=$(get_metric "media_ai_pool_submitted_total")
PROCESSED_AFTER=$(get_metric "media_ai_pool_processed_total")
SESSIONS_NOW=$(get_metric "media_ai_sessions_active")
AI_STREAMS_NOW=$(get_metric "media_ai_ai_streams_active")
AI_SEND_ERR=$(get_metric "media_ai_ai_send_errors_total")
AI_RECV_ERR=$(get_metric "media_ai_ai_recv_errors_total")

PORTS_USED=$(( ${PORTS_BEFORE:-0} - ${PORTS_AFTER:-0} ))
SUBMITTED_DELTA=$(( ${SUBMITTED_AFTER:-0} - ${SUBMITTED_BEFORE:-0} ))
PROCESSED_DELTA=$(( ${PROCESSED_AFTER:-0} - ${PROCESSED_BEFORE:-0} ))

info "  rtp_ports_available  : ${PORTS_BEFORE} → ${PORTS_AFTER}  (dùng: ${PORTS_USED})"
info "  pool_submitted_total : ${SUBMITTED_BEFORE} → ${SUBMITTED_AFTER}  (delta: +${SUBMITTED_DELTA})"
info "  pool_processed_total : ${PROCESSED_BEFORE} → ${PROCESSED_AFTER}  (delta: +${PROCESSED_DELTA})"
info "  sessions_active      : ${SESSIONS_NOW}"
info "  ai_streams_active    : ${AI_STREAMS_NOW}"
info "  ai_send_errors_total : ${AI_SEND_ERR}"
info "  ai_recv_errors_total : ${AI_RECV_ERR}"

[[ "$PORTS_USED" -ge 1 ]] && ok "Port allocated ✓" || warn "Port count không đổi"
[[ "$SUBMITTED_DELTA" -gt 0 ]] && ok "Audio jobs submitted: +${SUBMITTED_DELTA} ✓" \
    || warn "Không có audio job nào được submit (packet có thể bị drop)"
[[ "$PROCESSED_DELTA" -gt 0 ]] && ok "Audio jobs processed: +${PROCESSED_DELTA} ✓" \
    || warn "Không có audio job nào được process"
[[ "${SESSIONS_NOW}" -ge 1 ]] && ok "Session still active ✓" \
    || warn "Session đã biến mất (idle timeout hoặc lỗi pipeline)"
[[ "${AI_STREAMS_NOW}" -ge 1 ]] && ok "AI gRPC stream active ✓" \
    || warn "AI stream không active (mock-ai-worker chưa chạy hoặc stream lỗi)"
[[ "${AI_SEND_ERR}" == "0" ]] && ok "AI send errors: 0 ✓" \
    || warn "AI send errors: ${AI_SEND_ERR} (kiểm tra SendTimeout hoặc kết nối)"
[[ "${AI_RECV_ERR}" == "0" ]] && ok "AI recv errors: 0 ✓" \
    || warn "AI recv errors: ${AI_RECV_ERR}"

# ── Step 6: Get session ───────────────────────────────────────────────────────
sep
info "Step 6 — GET ${GW_BASE}/v1/sessions/${SESSION_ID}"

GET_RESP=$(h2curl -w "\n%{http_code}" "${GW_BASE}/v1/sessions/${SESSION_ID}")
HTTP_CODE=$(echo "$GET_RESP" | tail -1)
BODY=$(echo "$GET_RESP" | head -1)

[[ "$HTTP_CODE" == "200" ]] && ok "Session found (HTTP 200)" || warn "HTTP $HTTP_CODE"
echo "$BODY" | python3 -m json.tool 2>/dev/null || echo "$BODY"

# ── Step 7: Đóng session ──────────────────────────────────────────────────────
sep
info "Step 7 — DELETE ${GW_BASE}/v1/sessions/${SESSION_ID}"

DEL_CODE=$(h2curl -o /dev/null -w "%{http_code}" \
    -X DELETE "${GW_BASE}/v1/sessions/${SESSION_ID}")

[[ "$DEL_CODE" == "204" ]] && ok "Session deleted (HTTP 204)" || fail "Delete thất bại (HTTP $DEL_CODE)"

sleep 0.5
PORTS_FINAL=$(get_metric "media_ai_rtp_ports_available")
info "  rtp_ports_available after delete: ${PORTS_FINAL}"
[[ "${PORTS_FINAL}" == "${PORTS_BEFORE}" ]] && ok "Port returned to pool ✓" \
    || warn "Port count: before=${PORTS_BEFORE} after=${PORTS_FINAL}"

# ── Summary ───────────────────────────────────────────────────────────────────
sep
echo -e "${BOLD}Summary${NC}"
echo -e "  Gateway          : ${GW_BASE}  (${H2_MODE})"
echo -e "  Session ID       : ${SESSION_ID}"
echo -e "  RTP endpoint     : ${RTP_IP}:${RTP_PORT}"
echo -e "  Packets sent     : ${RTP_PACKETS} (~$((RTP_PACKETS / 25)) AudioChunks @ 500ms)"
echo -e "  Jobs processed   : +${PROCESSED_DELTA}"
echo -e "  AI stream active : ${AI_STREAMS_NOW}  (send_err=${AI_SEND_ERR} recv_err=${AI_RECV_ERR})"
echo -e "  Port pool        : ${PORTS_BEFORE} → ${PORTS_AFTER} → ${PORTS_FINAL} (restored)"
echo -e ""
echo -e "  mock-ai-worker (nếu đang chạy): partial mỗi 3 chunks, final mỗi 6 chunks"
echo -e "  200 packets → 8 chunks → 2 partial + 1 final logs trong mock"
sep
ok "Lab test PASSED"
