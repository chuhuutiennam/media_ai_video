#!/usr/bin/env bash
# test-rtp-amr.sh — Kịch bản test RTP transcode cho AMR (AMR-NB và AMR-WB)
#
# AMR (Adaptive Multi-Rate) là codec ACELP được dùng trong VoLTE:
#   AMR-NB (Narrowband):  8kHz,  20ms/frame, 4.75–12.2 kbps  (3GPP TS 26.090)
#   AMR-WB (Wideband):   16kHz,  20ms/frame, 6.60–23.85 kbps (3GPP TS 26.190)
#
# Payload format (RFC 4867, octet-aligned mode — VoLTE default):
#   Byte 0:   CMR  = [CMR[7:4] | padding[3:0]]
#             CMR field (4 bits): requested codec mode (FT index)
#   Byte 1:   TOC  = [F | FT[6:3] | Q | P[1:0]]
#             F=0 (last frame), FT=frame type, Q=quality bit, P=padding
#   Bytes 2+: compressed AMR frame data (size = amrXXFrameBytes[FT])
#
# Sub-test 1 — AMR-NB  (codec="AMR",    8kHz):
#   FT=7 (12.2kbps, mode MR122): amrNBFrameBytes[7] = 31 bytes
#   CMR=0x70, TOC=0x3C, frame=31 bytes → RTP payload = 33 bytes
#   Timestamp increment: 160 (8000Hz × 20ms)
#
# Sub-test 2 — AMR-WB  (codec="AMR-WB", 16kHz):
#   FT=8 (23.85kbps, mode MR2385): amrWBFrameBytes[8] = 60 bytes
#   CMR=0x80, TOC=0x44, frame=60 bytes → RTP payload = 62 bytes
#   Timestamp increment: 320 (16000Hz × 20ms)
#
# Expected outcome:
#   pool_decode_errors_total  += 200/packet  (ErrAMRNotAvailable — cần link opencore-amr CGO)
#   pool_processed_total      = 0 delta      (decode failure = không emit AudioChunk)
#   rtp_packets_routed_total  += 200         (routing vẫn hoạt động bình thường)
#
# Để enable AMR decode: link opencore-amr qua CGO (xem internal/codec/amr.go)
#
# Usage:
#   ./scripts/test-rtp-amr.sh [HOST] [PORT] [METRICS_PORT]

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────────────────
GW_HOST="${1:-127.0.0.1}"
GW_PORT="${2:-8080}"
METRICS_PORT="${3:-${2:-8080}}"
GW_BASE="http://${GW_HOST}:${GW_PORT}"
METRICS_BASE="http://${GW_HOST}:${METRICS_PORT}"

RTP_PACKETS=200      # 200 × 20ms = 4s
PACKET_INTERVAL=0.02 # 20ms inter-packet gap

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

sep
warn "AMR decode yêu cầu opencore-amr CGO (xem internal/codec/amr.go)"
warn "Test này xác nhận: session creation, RTP routing, và decode error handling"
warn "pool_decode_errors_total sẽ tăng — đây là EXPECTED BEHAVIOR khi chưa link CGO"

# ═════════════════════════════════════════════════════════════════════════════
# SUB-TEST 1: AMR-NB (8kHz, FT=7, 12.2kbps)
# ═════════════════════════════════════════════════════════════════════════════
sep
echo -e "${BOLD}╔════════════════════════════════════════════╗${NC}"
echo -e "${BOLD}║  Sub-test 1: AMR-NB  8kHz  FT=7 12.2kbps ║${NC}"
echo -e "${BOLD}╚════════════════════════════════════════════╝${NC}"

info "RFC 4867 octet-aligned payload structure:"
info "  Byte 0: CMR = 0x70  [CMR[7:4]=0b0111=FT7 | padding=0b0000]"
info "  Byte 1: TOC = 0x3C  [F=0 | FT[6:3]=0b0111=7 | Q=1 | P=0b00]"
info "  Bytes 2-32: 31 bytes frame data (amrNBFrameBytes[7]=31)"
info "  Total: 33 bytes/packet | ts_incr=160"

SESSION_NB="amr-nb-$(date +%s)"
SSRC_NB=57001

DECODE_ERR_BEFORE=$(get_metric "media_ai_pool_decode_errors_total")
SUBMITTED_BEFORE=$(get_metric "media_ai_pool_submitted_total")
ROUTED_BEFORE=$(get_metric "media_ai_rtp_packets_routed_total")
PORTS_BEFORE=$(get_metric "media_ai_rtp_ports_available")

sep
info "Tạo session AMR-NB: ${GW_BASE}/v1/sessions"

CREATE=$(h2curl -w "\n%{http_code}" \
    -X POST "${GW_BASE}/v1/sessions" \
    -H "Content-Type: application/json" \
    -H "X-Request-ID: amrnb-$(date +%s)" \
    -d "{
      \"id\":          \"${SESSION_NB}\",
      \"source_type\": \"raw_rtp\",
      \"codec\":       \"AMR\",
      \"sample_rate\": 8000,
      \"ssrc\":        ${SSRC_NB}
    }")

BODY=$(echo "$CREATE" | head -1)
CODE=$(echo "$CREATE" | tail -1)
[[ "$CODE" == "201" ]] || fail "Create AMR-NB session thất bại (HTTP $CODE): $BODY"
ok "AMR-NB session created (HTTP 201)"
echo "$BODY" | python3 -m json.tool 2>/dev/null || echo "$BODY"

RTP_IP=$(echo "$BODY"   | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('rtp_ip','127.0.0.1'))")
RTP_PORT=$(echo "$BODY" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('rtp_port','0'))")
[[ -z "$RTP_PORT" || "$RTP_PORT" == "0" ]] && fail "Không lấy được rtp_port"
ok "RTP endpoint: ${RTP_IP}:${RTP_PORT}"

sleep 0.3

sep
info "Gửi ${RTP_PACKETS} AMR-NB RTP packet → ${RTP_IP}:${RTP_PORT}"

python3 - <<PYEOF
import socket, struct, time, sys

DEST    = ("${RTP_IP}", ${RTP_PORT})
SSRC    = ${SSRC_NB}
N       = ${RTP_PACKETS}
INTERVAL= ${PACKET_INTERVAL}
TS_INCR = 160   # 8000Hz × 20ms

# RFC 4867 §4.4.1 octet-aligned AMR-NB payload (FT=7, 12.2kbps):
#   CMR  byte: [CMR[7:4]=0b0111 | padding=0b0000] = 0x70
#              CMR field requests mode FT=7 (12.2kbps) from encoder
#   TOC  byte: [F=0 | FT[6:3]=0b0111 | Q=1 | P[1:0]=0b00] = 0x3C
#              F=0: this is the last (only) TOC entry
#              FT=7: frame type 7 = 12.2kbps speech frame
#              Q=1: good quality frame
#   Frame data: 31 bytes (amrNBFrameBytes[7] = 31 from RFC 4867 Table 1)
#              Using 0xAB pattern to make non-zero placeholder bytes visible
CMR   = bytes([0x70])
TOC   = bytes([0x3C])
FRAME = bytes([0xAB] * 31)  # placeholder: real ACELP frame needs opencore-amr
PAYLOAD = CMR + TOC + FRAME  # 33 bytes total

sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
errors = 0

print(f"  AMR-NB frame: CMR=0x70 | TOC=0x3C (FT=7,Q=1) | {len(FRAME)} bytes payload")
print(f"  RTP payload size: {len(PAYLOAD)} bytes | PT=96 (dynamic)")
print(f"  Sending {N} packets to {DEST[0]}:{DEST[1]} ...")

for i in range(N):
    seq = (i + 1) & 0xFFFF
    ts  = (i * TS_INCR) & 0xFFFFFFFF
    # RTP header: V=2, P=0, X=0, CC=0, M=0, PT=96 (dynamic, AMR-NB per RFC 4867)
    hdr = struct.pack('!BBHII', 0x80, 96, seq, ts, SSRC)
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
print(f"  Expected: decode fails with ErrAMRNotAvailable (needs opencore-amr CGO)")
PYEOF

sep
info "Chờ pipeline xử lý..."
sleep 4

info "Metrics sau AMR-NB test:"
DECODE_ERR_AFTER=$(get_metric "media_ai_pool_decode_errors_total")
SUBMITTED_AFTER=$(get_metric "media_ai_pool_submitted_total")
ROUTED_AFTER=$(get_metric "media_ai_rtp_packets_routed_total")

DECODE_DELTA=$(( ${DECODE_ERR_AFTER:-0} - ${DECODE_ERR_BEFORE:-0} ))
SUBMIT_DELTA=$(( ${SUBMITTED_AFTER:-0} - ${SUBMITTED_BEFORE:-0} ))
ROUTED_DELTA=$(( ${ROUTED_AFTER:-0} - ${ROUTED_BEFORE:-0} ))

info "  pool_submitted_total     : +${SUBMIT_DELTA}  (expect +200 jobs)"
info "  pool_decode_errors_total : +${DECODE_DELTA}  (expected — ErrAMRNotAvailable)"

[[ "$SUBMIT_DELTA" -ge 190 ]] \
    && ok "Pipeline nhận packet: +${SUBMIT_DELTA}/200 jobs submitted ✓" \
    || warn "Jobs submitted thấp: ${SUBMIT_DELTA}/200 (kiểm tra per-session port)"

if [[ "$DECODE_DELTA" -gt 0 ]]; then
    ok "Decode errors: +${DECODE_DELTA} ✓ (expected ErrAMRNotAvailable — cần CGO)"
else
    warn "Không có decode error — kiểm tra xem AMR decoder có được link không?"
fi

info "DELETE session AMR-NB..."
DEL_CODE=$(h2curl -o /dev/null -w "%{http_code}" -X DELETE "${GW_BASE}/v1/sessions/${SESSION_NB}")
[[ "$DEL_CODE" == "204" ]] && ok "Session AMR-NB deleted ✓" || warn "Delete HTTP $DEL_CODE"

sleep 0.5
PORTS_AFTER=$(get_metric "media_ai_rtp_ports_available")
[[ "${PORTS_AFTER}" == "${PORTS_BEFORE}" ]] && ok "Port returned to pool ✓" \
    || warn "Port pool: before=${PORTS_BEFORE} after=${PORTS_AFTER}"

NB_ROUTED=$SUBMIT_DELTA
NB_DECODE_ERR=$DECODE_DELTA

# ═════════════════════════════════════════════════════════════════════════════
# SUB-TEST 2: AMR-WB (16kHz, FT=8, 23.85kbps)
# ═════════════════════════════════════════════════════════════════════════════
sep
echo -e "${BOLD}╔═════════════════════════════════════════════╗${NC}"
echo -e "${BOLD}║  Sub-test 2: AMR-WB  16kHz  FT=8 23.85kbps ║${NC}"
echo -e "${BOLD}╚═════════════════════════════════════════════╝${NC}"

info "RFC 4867 octet-aligned payload structure:"
info "  Byte 0: CMR = 0x80  [CMR[7:4]=0b1000=FT8 | padding=0b0000]"
info "  Byte 1: TOC = 0x44  [F=0 | FT[6:3]=0b1000=8 | Q=1 | P=0b00]"
info "  Bytes 2-61: 60 bytes frame data (amrWBFrameBytes[8]=60)"
info "  Total: 62 bytes/packet | ts_incr=320"

SESSION_WB="amr-wb-$(date +%s)"
SSRC_WB=57002

DECODE_ERR_BEFORE=$(get_metric "media_ai_pool_decode_errors_total")
SUBMITTED_BEFORE=$(get_metric "media_ai_pool_submitted_total")
ROUTED_BEFORE=$(get_metric "media_ai_rtp_packets_routed_total")
PORTS_BEFORE=$(get_metric "media_ai_rtp_ports_available")

sep
info "Tạo session AMR-WB: ${GW_BASE}/v1/sessions"

CREATE=$(h2curl -w "\n%{http_code}" \
    -X POST "${GW_BASE}/v1/sessions" \
    -H "Content-Type: application/json" \
    -H "X-Request-ID: amrwb-$(date +%s)" \
    -d "{
      \"id\":          \"${SESSION_WB}\",
      \"source_type\": \"raw_rtp\",
      \"codec\":       \"AMR-WB\",
      \"sample_rate\": 16000,
      \"ssrc\":        ${SSRC_WB}
    }")

BODY=$(echo "$CREATE" | head -1)
CODE=$(echo "$CREATE" | tail -1)
[[ "$CODE" == "201" ]] || fail "Create AMR-WB session thất bại (HTTP $CODE): $BODY"
ok "AMR-WB session created (HTTP 201)"
echo "$BODY" | python3 -m json.tool 2>/dev/null || echo "$BODY"

RTP_IP=$(echo "$BODY"   | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('rtp_ip','127.0.0.1'))")
RTP_PORT=$(echo "$BODY" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('rtp_port','0'))")
[[ -z "$RTP_PORT" || "$RTP_PORT" == "0" ]] && fail "Không lấy được rtp_port"
ok "RTP endpoint: ${RTP_IP}:${RTP_PORT}"

sleep 0.3

sep
info "Gửi ${RTP_PACKETS} AMR-WB RTP packet → ${RTP_IP}:${RTP_PORT}"

python3 - <<PYEOF
import socket, struct, time, sys

DEST    = ("${RTP_IP}", ${RTP_PORT})
SSRC    = ${SSRC_WB}
N       = ${RTP_PACKETS}
INTERVAL= ${PACKET_INTERVAL}
TS_INCR = 320   # 16000Hz × 20ms

# RFC 4867 §4.4.1 octet-aligned AMR-WB payload (FT=8, 23.85kbps):
#   CMR  byte: [CMR[7:4]=0b1000 | padding=0b0000] = 0x80
#              CMR=8 requests mode FT=8 (23.85kbps = MR2385)
#   TOC  byte: [F=0 | FT[6:3]=0b1000 | Q=1 | P[1:0]=0b00] = 0x44
#              F=0: last TOC entry
#              FT=8: frame type 8 = 23.85kbps AMR-WB speech
#              Q=1: good quality
#   Frame data: 60 bytes (amrWBFrameBytes[8] = 60 from RFC 4867 Table 2)
CMR   = bytes([0x80])
TOC   = bytes([0x44])
FRAME = bytes([0xCD] * 60)  # placeholder: real ACELP frame needs opencore-amr
PAYLOAD = CMR + TOC + FRAME  # 62 bytes total

sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
errors = 0

print(f"  AMR-WB frame: CMR=0x80 | TOC=0x44 (FT=8,Q=1) | {len(FRAME)} bytes payload")
print(f"  RTP payload size: {len(PAYLOAD)} bytes | PT=97 (dynamic)")
print(f"  Sending {N} packets to {DEST[0]}:{DEST[1]} ...")

for i in range(N):
    seq = (i + 1) & 0xFFFF
    ts  = (i * TS_INCR) & 0xFFFFFFFF
    # RTP header: V=2, P=0, X=0, CC=0, M=0, PT=97 (dynamic, AMR-WB per RFC 4867)
    hdr = struct.pack('!BBHII', 0x80, 97, seq, ts, SSRC)
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
print(f"  Expected: decode fails with ErrAMRNotAvailable (needs opencore-amr CGO)")
PYEOF

sep
info "Chờ pipeline xử lý..."
sleep 4

info "Metrics sau AMR-WB test:"
DECODE_ERR_AFTER=$(get_metric "media_ai_pool_decode_errors_total")
SUBMITTED_AFTER=$(get_metric "media_ai_pool_submitted_total")
ROUTED_AFTER=$(get_metric "media_ai_rtp_packets_routed_total")

DECODE_DELTA=$(( ${DECODE_ERR_AFTER:-0} - ${DECODE_ERR_BEFORE:-0} ))
SUBMIT_DELTA=$(( ${SUBMITTED_AFTER:-0} - ${SUBMITTED_BEFORE:-0} ))
ROUTED_DELTA=$(( ${ROUTED_AFTER:-0} - ${ROUTED_BEFORE:-0} ))

info "  pool_submitted_total     : +${SUBMIT_DELTA}  (expect +200 jobs)"
info "  pool_decode_errors_total : +${DECODE_DELTA}  (expected — ErrAMRNotAvailable)"

[[ "$SUBMIT_DELTA" -ge 190 ]] \
    && ok "Pipeline nhận packet: +${SUBMIT_DELTA}/200 jobs submitted ✓" \
    || warn "Jobs submitted thấp: ${SUBMIT_DELTA}/200 (kiểm tra per-session port)"

if [[ "$DECODE_DELTA" -gt 0 ]]; then
    ok "Decode errors: +${DECODE_DELTA} ✓ (expected ErrAMRNotAvailable — cần CGO)"
else
    warn "Không có decode error — kiểm tra xem AMR-WB decoder có được link không?"
fi

info "DELETE session AMR-WB..."
DEL_CODE=$(h2curl -o /dev/null -w "%{http_code}" -X DELETE "${GW_BASE}/v1/sessions/${SESSION_WB}")
[[ "$DEL_CODE" == "204" ]] && ok "Session AMR-WB deleted ✓" || warn "Delete HTTP $DEL_CODE"

sleep 0.5
PORTS_FINAL=$(get_metric "media_ai_rtp_ports_available")
[[ "${PORTS_FINAL}" == "${PORTS_BEFORE}" ]] && ok "Port returned to pool ✓" \
    || warn "Port pool: before=${PORTS_BEFORE} final=${PORTS_FINAL}"

WB_ROUTED=$SUBMIT_DELTA
WB_DECODE_ERR=$DECODE_DELTA

# ── Summary ───────────────────────────────────────────────────────────────────
sep
echo -e "${BOLD}═══════ AMR Test Summary ═══════${NC}"
echo ""
echo -e "  Gateway   : ${GW_BASE}  (${H2_MODE})"
echo -e "  Packets   : ${RTP_PACKETS} per variant × 2 = $((RTP_PACKETS * 2)) total"
echo ""
echo -e "  AMR-NB (8kHz, FT=7, 12.2kbps, 33 bytes/packet):"
echo -e "    Jobs submitted   : +${NB_ROUTED}"
echo -e "    Decode errors    : +${NB_DECODE_ERR}  (expected ≈200 — ErrAMRNotAvailable)"
echo ""
echo -e "  AMR-WB (16kHz, FT=8, 23.85kbps, 62 bytes/packet):"
echo -e "    Jobs submitted   : +${WB_ROUTED}"
echo -e "    Decode errors    : +${WB_DECODE_ERR}  (expected ≈200 — ErrAMRNotAvailable)"
echo ""
echo -e "  Để enable AMR decode:"
echo -e "    apt-get install libopencore-amrnb-dev libopencore-amrwb-dev"
echo -e "    Implement CGO binding trong internal/codec/amr.go (xem Option A)"
sep

ROUTING_OK=true
[[ "$NB_ROUTED" -lt 190 || "$WB_ROUTED" -lt 190 ]] && ROUTING_OK=false

if $ROUTING_OK; then
    ok "AMR test PASSED (pipeline OK, decode errors expected khi chưa link CGO)"
else
    fail "AMR test FAILED (jobs submitted thấp: NB=${NB_ROUTED} WB=${WB_ROUTED})"
fi
