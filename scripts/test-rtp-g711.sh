#!/usr/bin/env bash
# test-rtp-g711.sh — Kịch bản test RTP transcode cho G.711 (PCMU và PCMA)
#
# G.711 là codec PCM nén tại 8kHz, 1 byte/sample:
#   PCMU (µ-law, PT=0): ITU-T G.711 µ-law, dùng phổ biến ở Bắc Mỹ/Nhật Bản
#   PCMA (A-law, PT=8): ITU-T G.711 A-law, dùng phổ biến ở châu Âu/châu Á
#
# Payload format (RFC 3551):
#   Mỗi RTP packet = 160 bytes = 20ms audio @ 8kHz (1 sample/byte)
#   PCMU silence:  byte 0x7F (µ-law coded silence → PCM ≈ 0)
#   PCMA silence:  byte 0xD5 (A-law coded silence → PCM = +8, near silence, -74 dBFS)
#
# Kịch bản:
#   Sub-test 1 — PCMU: 200 packet × 20ms = 4s → ~8 AudioChunk (500ms each)
#   Sub-test 2 — PCMA: 200 packet × 20ms = 4s → ~8 AudioChunk
#
# Expected metrics sau mỗi sub-test:
#   pool_decode_errors_total  = 0          (G.711 là pure-Go, không cần CGO)
#   pool_processed_total      += ~8        (số AudioChunk phát ra)
#   rtp_packets_routed_total  += 200
#
# Usage:
#   ./scripts/test-rtp-g711.sh [HOST] [PORT] [METRICS_PORT]
#
# Defaults: HOST=127.0.0.1  PORT=8080  METRICS_PORT=<PORT>

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────────────────
GW_HOST="${1:-127.0.0.1}"
GW_PORT="${2:-8080}"
METRICS_PORT="${3:-${2:-8080}}"
GW_BASE="http://${GW_HOST}:${GW_PORT}"
METRICS_BASE="http://${GW_HOST}:${METRICS_PORT}"

RTP_PACKETS=200      # 200 × 20ms = 4s → 8 AudioChunks @ 500ms
PACKET_INTERVAL=0.02 # 20ms inter-packet gap
TIMESTAMP_INCR=160   # 8000 Hz × 20ms = 160 samples/frame

# ── Colors ────────────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'

info()  { echo -e "${CYAN}[INFO]${NC}  $*"; }
ok()    { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
fail()  { echo -e "${RED}[FAIL]${NC}  $*"; exit 1; }
sep()   { echo -e "${BOLD}────────────────────────────────────────${NC}"; }

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

# ── Health check ──────────────────────────────────────────────────────────────
sep
info "Health check: ${GW_BASE}/health/ready"

RESP=$(h2curl -w "\n%{http_code}" "${GW_BASE}/health/ready")
CODE=$(echo "$RESP" | tail -1)
[[ "$CODE" == "200" ]] || fail "Gateway not ready (HTTP $CODE)"
ok "Gateway ready"

# ═════════════════════════════════════════════════════════════════════════════
# SUB-TEST 1: PCMU (µ-law, PT=0)
# ═════════════════════════════════════════════════════════════════════════════
sep
echo -e "${BOLD}╔══════════════════════════════════════╗${NC}"
echo -e "${BOLD}║  Sub-test 1: PCMU (µ-law)  PT=0     ║${NC}"
echo -e "${BOLD}╚══════════════════════════════════════╝${NC}"
info "Codec: PCMU | 8kHz | 160 bytes/frame | silence=0x7F"

SESSION_PCMU="g711-pcmu-$(date +%s)"
SSRC_PCMU=60001

# Baseline
DECODE_ERR_BEFORE=$(get_metric "media_ai_pool_decode_errors_total")
PROCESSED_BEFORE=$(get_metric "media_ai_pool_processed_total")
SUBMITTED_BEFORE=$(get_metric "media_ai_pool_submitted_total")
PORTS_BEFORE=$(get_metric "media_ai_rtp_ports_available")

# Note: rtp_packets_routed_total chỉ đếm packet qua shared ingress (:5004).
# Per-session ports (40000-40099) dùng StartSessionListener bypass ingress stats.
# Dùng pool_submitted_total để xác nhận packet đã đến pipeline.

sep
info "Tạo session PCMU: ${GW_BASE}/v1/sessions"

CREATE=$(h2curl -w "\n%{http_code}" \
    -X POST "${GW_BASE}/v1/sessions" \
    -H "Content-Type: application/json" \
    -H "X-Request-ID: pcmu-$(date +%s)" \
    -d "{
      \"id\":          \"${SESSION_PCMU}\",
      \"source_type\": \"raw_rtp\",
      \"codec\":       \"PCMU\",
      \"sample_rate\": 8000,
      \"ssrc\":        ${SSRC_PCMU}
    }")

BODY=$(echo "$CREATE" | head -1)
CODE=$(echo "$CREATE" | tail -1)
[[ "$CODE" == "201" ]] || fail "Create PCMU session thất bại (HTTP $CODE): $BODY"
ok "PCMU session created (HTTP 201)"
echo "$BODY" | python3 -m json.tool 2>/dev/null || echo "$BODY"

RTP_IP=$(echo "$BODY"   | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('rtp_ip','127.0.0.1'))")
RTP_PORT=$(echo "$BODY" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('rtp_port','0'))")
[[ -z "$RTP_PORT" || "$RTP_PORT" == "0" ]] && fail "Không lấy được rtp_port"
ok "RTP endpoint: ${RTP_IP}:${RTP_PORT}"

sleep 0.3

sep
info "Gửi ${RTP_PACKETS} PCMU RTP packet → ${RTP_IP}:${RTP_PORT}"
info "  Payload: 160 bytes × 0x7F (µ-law silence, PT=0)"
info "  Timestamp increment: ${TIMESTAMP_INCR} (8000Hz × 20ms)"

python3 - <<PYEOF
import socket, struct, time, sys

DEST    = ("${RTP_IP}", ${RTP_PORT})
SSRC    = ${SSRC_PCMU}
N       = ${RTP_PACKETS}
INTERVAL= ${PACKET_INTERVAL}
TS_INCR = ${TIMESTAMP_INCR}

# G.711 µ-law silence: 0x7F = complement 0x80 → exp=0, mantissa=0 → PCM≈0
PAYLOAD = bytes([0x7F] * 160)

sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
errors = 0

print("  Frame format: RTP/PCMU  PT=0  160 bytes/packet  8kHz")
print(f"  Sending {N} packets to {DEST[0]}:{DEST[1]} ...")

for i in range(N):
    seq  = (i + 1) & 0xFFFF
    ts   = (i * TS_INCR) & 0xFFFFFFFF
    # RTP header: V=2, P=0, X=0, CC=0, M=0, PT=0 (PCMU)
    hdr  = struct.pack('!BBHII', 0x80, 0x00, seq, ts, SSRC)
    try:
        sock.sendto(hdr + PAYLOAD, DEST)
    except Exception as e:
        errors += 1
        print(f"  [!] packet {i+1} error: {e}", file=sys.stderr)
    if (i + 1) % 50 == 0:
        print(f"  Sent {i+1}/{N} packets  (ts={ts})")
    time.sleep(INTERVAL)

sock.close()
print(f"  Done: {N - errors}/{N} sent  errors={errors}")
PYEOF

sep
info "Chờ pipeline flush (4s audio → 8 chunks @ 500ms)..."
sleep 5

info "Metrics sau PCMU test:"
DECODE_ERR_AFTER=$(get_metric "media_ai_pool_decode_errors_total")
PROCESSED_AFTER=$(get_metric "media_ai_pool_processed_total")
SUBMITTED_AFTER=$(get_metric "media_ai_pool_submitted_total")

DECODE_DELTA=$(( ${DECODE_ERR_AFTER:-0} - ${DECODE_ERR_BEFORE:-0} ))
PROC_DELTA=$(( ${PROCESSED_AFTER:-0} - ${PROCESSED_BEFORE:-0} ))
SUBMIT_DELTA=$(( ${SUBMITTED_AFTER:-0} - ${SUBMITTED_BEFORE:-0} ))

info "  pool_submitted_total     : +${SUBMIT_DELTA}  (expect +200 jobs)"
info "  pool_processed_total     : +${PROC_DELTA}  (expect +200 jobs)"
info "  pool_decode_errors_total : +${DECODE_DELTA}  (expect 0)"

[[ "$SUBMIT_DELTA" -ge 190 ]] && ok "Pipeline nhận packet: +${SUBMIT_DELTA}/200 jobs submitted ✓" \
    || warn "Jobs submitted thấp: ${SUBMIT_DELTA}/200 (kiểm tra per-session port ${RTP_PORT})"

[[ "$PROC_DELTA" -ge 190 ]] && ok "Transcode OK: +${PROC_DELTA}/200 jobs processed ✓" \
    || warn "Jobs processed thấp: ${PROC_DELTA} (kiểm tra pipeline decode)"

if [[ "$DECODE_DELTA" -eq 0 ]]; then
    ok "Decode errors: 0 — G.711 PCMU transcode thành công ✓"
else
    fail "Decode errors: +${DECODE_DELTA} — G.711 PCMU KHÔNG nên có decode error!"
fi

info "DELETE session PCMU..."
DEL_CODE=$(h2curl -o /dev/null -w "%{http_code}" -X DELETE "${GW_BASE}/v1/sessions/${SESSION_PCMU}")
[[ "$DEL_CODE" == "204" ]] && ok "Session PCMU deleted ✓" || warn "Delete HTTP $DEL_CODE"

sleep 0.5
PORTS_AFTER=$(get_metric "media_ai_rtp_ports_available")
[[ "${PORTS_AFTER}" == "${PORTS_BEFORE}" ]] && ok "Port returned to pool ✓" \
    || warn "Port pool: before=${PORTS_BEFORE} after=${PORTS_AFTER}"

PCMU_PROC_DELTA=$PROC_DELTA
PCMU_DECODE_ERR=$DECODE_DELTA

# ═════════════════════════════════════════════════════════════════════════════
# SUB-TEST 2: PCMA (A-law, PT=8)
# ═════════════════════════════════════════════════════════════════════════════
sep
echo -e "${BOLD}╔══════════════════════════════════════╗${NC}"
echo -e "${BOLD}║  Sub-test 2: PCMA (A-law)   PT=8    ║${NC}"
echo -e "${BOLD}╚══════════════════════════════════════╝${NC}"
info "Codec: PCMA | 8kHz | 160 bytes/frame | silence=0xD5"

SESSION_PCMA="g711-pcma-$(date +%s)"
SSRC_PCMA=60002

# Baseline
DECODE_ERR_BEFORE=$(get_metric "media_ai_pool_decode_errors_total")
PROCESSED_BEFORE=$(get_metric "media_ai_pool_processed_total")
SUBMITTED_BEFORE=$(get_metric "media_ai_pool_submitted_total")
PORTS_BEFORE=$(get_metric "media_ai_rtp_ports_available")

sep
info "Tạo session PCMA: ${GW_BASE}/v1/sessions"

CREATE=$(h2curl -w "\n%{http_code}" \
    -X POST "${GW_BASE}/v1/sessions" \
    -H "Content-Type: application/json" \
    -H "X-Request-ID: pcma-$(date +%s)" \
    -d "{
      \"id\":          \"${SESSION_PCMA}\",
      \"source_type\": \"raw_rtp\",
      \"codec\":       \"PCMA\",
      \"sample_rate\": 8000,
      \"ssrc\":        ${SSRC_PCMA}
    }")

BODY=$(echo "$CREATE" | head -1)
CODE=$(echo "$CREATE" | tail -1)
[[ "$CODE" == "201" ]] || fail "Create PCMA session thất bại (HTTP $CODE): $BODY"
ok "PCMA session created (HTTP 201)"
echo "$BODY" | python3 -m json.tool 2>/dev/null || echo "$BODY"

RTP_IP=$(echo "$BODY"   | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('rtp_ip','127.0.0.1'))")
RTP_PORT=$(echo "$BODY" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('rtp_port','0'))")
[[ -z "$RTP_PORT" || "$RTP_PORT" == "0" ]] && fail "Không lấy được rtp_port"
ok "RTP endpoint: ${RTP_IP}:${RTP_PORT}"

sleep 0.3

sep
info "Gửi ${RTP_PACKETS} PCMA RTP packet → ${RTP_IP}:${RTP_PORT}"
info "  Payload: 160 bytes × 0xD5 (A-law silence, PT=8)"
info "  A-law decode: 0xD5 XOR 0x55 = 0x80 → sign=+, exp=0, mantissa=0 → PCM=+8 (near silence)"

python3 - <<PYEOF
import socket, struct, time, sys

DEST    = ("${RTP_IP}", ${RTP_PORT})
SSRC    = ${SSRC_PCMA}
N       = ${RTP_PACKETS}
INTERVAL= ${PACKET_INTERVAL}
TS_INCR = ${TIMESTAMP_INCR}

# G.711 A-law silence: 0xD5 XOR 0x55 = 0x80 → sign=1(pos), exp=0, mantissa=0 → PCM=+8 (near silence)
PAYLOAD = bytes([0xD5] * 160)

sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
errors = 0

print("  Frame format: RTP/PCMA  PT=8  160 bytes/packet  8kHz")
print(f"  Sending {N} packets to {DEST[0]}:{DEST[1]} ...")

for i in range(N):
    seq  = (i + 1) & 0xFFFF
    ts   = (i * TS_INCR) & 0xFFFFFFFF
    # RTP header: V=2, P=0, X=0, CC=0, M=0, PT=8 (PCMA)
    hdr  = struct.pack('!BBHII', 0x80, 0x08, seq, ts, SSRC)
    try:
        sock.sendto(hdr + PAYLOAD, DEST)
    except Exception as e:
        errors += 1
        print(f"  [!] packet {i+1} error: {e}", file=sys.stderr)
    if (i + 1) % 50 == 0:
        print(f"  Sent {i+1}/{N} packets  (ts={ts})")
    time.sleep(INTERVAL)

sock.close()
print(f"  Done: {N - errors}/{N} sent  errors={errors}")
PYEOF

sep
info "Chờ pipeline flush..."
sleep 5

info "Metrics sau PCMA test:"
DECODE_ERR_AFTER=$(get_metric "media_ai_pool_decode_errors_total")
PROCESSED_AFTER=$(get_metric "media_ai_pool_processed_total")
SUBMITTED_AFTER=$(get_metric "media_ai_pool_submitted_total")

DECODE_DELTA=$(( ${DECODE_ERR_AFTER:-0} - ${DECODE_ERR_BEFORE:-0} ))
PROC_DELTA=$(( ${PROCESSED_AFTER:-0} - ${PROCESSED_BEFORE:-0} ))
SUBMIT_DELTA=$(( ${SUBMITTED_AFTER:-0} - ${SUBMITTED_BEFORE:-0} ))

info "  pool_submitted_total     : +${SUBMIT_DELTA}  (expect +200 jobs)"
info "  pool_processed_total     : +${PROC_DELTA}  (expect +200 jobs)"
info "  pool_decode_errors_total : +${DECODE_DELTA}  (expect 0)"

[[ "$SUBMIT_DELTA" -ge 190 ]] && ok "Pipeline nhận packet: +${SUBMIT_DELTA}/200 jobs submitted ✓" \
    || warn "Jobs submitted thấp: ${SUBMIT_DELTA}/200 (kiểm tra per-session port ${RTP_PORT})"

[[ "$PROC_DELTA" -ge 190 ]] && ok "Transcode OK: +${PROC_DELTA}/200 jobs processed ✓" \
    || warn "Jobs processed thấp: ${PROC_DELTA}"

if [[ "$DECODE_DELTA" -eq 0 ]]; then
    ok "Decode errors: 0 — G.711 PCMA transcode thành công ✓"
else
    fail "Decode errors: +${DECODE_DELTA} — G.711 PCMA KHÔNG nên có decode error!"
fi

info "DELETE session PCMA..."
DEL_CODE=$(h2curl -o /dev/null -w "%{http_code}" -X DELETE "${GW_BASE}/v1/sessions/${SESSION_PCMA}")
[[ "$DEL_CODE" == "204" ]] && ok "Session PCMA deleted ✓" || warn "Delete HTTP $DEL_CODE"

sleep 0.5
PORTS_FINAL=$(get_metric "media_ai_rtp_ports_available")
[[ "${PORTS_FINAL}" == "${PORTS_BEFORE}" ]] && ok "Port returned to pool ✓" \
    || warn "Port pool: before=${PORTS_BEFORE} final=${PORTS_FINAL}"

PCMA_PROC_DELTA=$PROC_DELTA
PCMA_DECODE_ERR=$DECODE_DELTA

# ── Summary ───────────────────────────────────────────────────────────────────
sep
echo -e "${BOLD}═══════ G.711 Test Summary ═══════${NC}"
echo ""
echo -e "  Gateway   : ${GW_BASE}  (${H2_MODE})"
echo -e "  Packets   : ${RTP_PACKETS} per codec × 2 = $((RTP_PACKETS * 2)) total"
echo ""
echo -e "  PCMU (µ-law):"
echo -e "    Chunks processed : +${PCMU_PROC_DELTA}"
echo -e "    Decode errors    : ${PCMU_DECODE_ERR}  $([ "$PCMU_DECODE_ERR" -eq 0 ] && echo '✓ PASS' || echo '✗ FAIL')"
echo ""
echo -e "  PCMA (A-law):"
echo -e "    Chunks processed : +${PCMA_PROC_DELTA}"
echo -e "    Decode errors    : ${PCMA_DECODE_ERR}  $([ "$PCMA_DECODE_ERR" -eq 0 ] && echo '✓ PASS' || echo '✗ FAIL')"
echo ""
echo -e "  Note: G.711 decode là pure-Go (không cần CGO)."
echo -e "        Transcode pipeline: PCMU/PCMA → PCM int16 → resample 16kHz → AudioChunk → AI"
sep

TOTAL_ERRORS=$(( PCMU_DECODE_ERR + PCMA_DECODE_ERR ))
if [[ "$TOTAL_ERRORS" -eq 0 ]]; then
    ok "G.711 test PASSED (decode errors = 0)"
else
    fail "G.711 test FAILED (total decode errors = ${TOTAL_ERRORS})"
fi
