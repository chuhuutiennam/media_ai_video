#!/usr/bin/env bash
# test-rtp-evs.sh — Kịch bản test RTP transcode cho EVS (Enhanced Voice Services)
#
# EVS (3GPP TS 26.441/26.442) là codec thế hệ tiếp theo của AMR-WB trong VoLTE/VoNR:
#   Primary mode:    5.9–128 kbps, 8/16/32/48 kHz
#   AMR-WB IO mode: tương thích ngược AMR-WB (16 kHz)
#
# Payload format (RFC 8130):
#   Compact format  (single frame, no header byte):
#     Payload size xác định bit rate — byte[0] bit7 = 0 (không phải H bit)
#     và size khớp đúng một entry trong evsPrimaryFrameBytes.
#     Đây là format phổ biến nhất trong VoLTE single-frame sessions.
#
#   Header-Full format (multi-frame hoặc khi cần CMR tường minh):
#     Byte 0: H(1) | CMR(4 bits) | padding(3 bits)  với H=1 (bit 7 = 1)
#     Bytes 1+: frame payload
#
# Sub-test 1 — Compact 13200bps (47 bytes):
#   evsPrimaryFrameBytes[13200] = 47 — phổ biến nhất trong VoLTE primary mode
#   byte[0] bit7 = 0 → DetectEVSFormat → Compact
#
# Sub-test 2 — Compact 9600bps (33 bytes):
#   evsPrimaryFrameBytes[9600] = 33 — bitrate thấp hơn, dùng trong điều kiện mạng kém
#
# Sub-test 3 — Header-Full (48 bytes = 1 header + 47 frame):
#   byte[0] = 0x98 (H=1, CMR=3, pad=0) → DetectEVSFormat → HeaderFull
#
# Expected outcome cho cả 3 sub-test:
#   pool_decode_errors_total  += N  (ErrEVSNotAvailable — cần 3GPP EVS codec CGO)
#   pool_processed_total      = 0 delta
#   rtp_packets_routed_total  += 200
#
# Tất cả sample_rate = 16kHz, timestamp increment = 320 (16000Hz × 20ms)
#
# Để enable EVS decode: link 3GPP reference codec via CGO (xem internal/codec/evs.go)
#
# Usage:
#   ./scripts/test-rtp-evs.sh [HOST] [PORT] [METRICS_PORT]

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────────────────
GW_HOST="${1:-127.0.0.1}"
GW_PORT="${2:-8080}"
METRICS_PORT="${3:-${2:-8080}}"
GW_BASE="http://${GW_HOST}:${GW_PORT}"
METRICS_BASE="http://${GW_HOST}:${METRICS_PORT}"

RTP_PACKETS=200      # 200 × 20ms = 4s
PACKET_INTERVAL=0.02 # 20ms inter-packet gap
SAMPLE_RATE=16000
TS_INCR=320          # 16000Hz × 20ms

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
warn "EVS decode yêu cầu 3GPP reference codec CGO (xem internal/codec/evs.go)"
warn "Test này xác nhận: session creation, RTP routing, DetectEVSFormat, và error handling"
warn "pool_decode_errors_total sẽ tăng — đây là EXPECTED BEHAVIOR khi chưa link CGO"

# ═════════════════════════════════════════════════════════════════════════════
# SUB-TEST 1: EVS Primary Compact — 13200bps (47 bytes)
# ═════════════════════════════════════════════════════════════════════════════
sep
echo -e "${BOLD}╔══════════════════════════════════════════════════╗${NC}"
echo -e "${BOLD}║  Sub-test 1: EVS Compact  13200bps  47 bytes    ║${NC}"
echo -e "${BOLD}╚══════════════════════════════════════════════════╝${NC}"

info "RFC 8130 compact format:"
info "  Payload = 47 bytes, không có header byte"
info "  DetectEVSFormat: byte[0]&0x80==0 AND size∈evsPrimaryFrameBytes → Compact"
info "  EVSPrimaryBitrate(47) → 13200 bps"
info "  16kHz, ts_incr=320"

SESSION_C1="evs-compact-13200-$(date +%s)"
SSRC_C1=58001

DECODE_ERR_BEFORE=$(get_metric "media_ai_pool_decode_errors_total")
SUBMITTED_BEFORE=$(get_metric "media_ai_pool_submitted_total")
SUBMITTED_BEFORE=$(get_metric "media_ai_pool_submitted_total")
PORTS_BEFORE=$(get_metric "media_ai_rtp_ports_available")

sep
info "Tạo session EVS (Compact 13200): ${GW_BASE}/v1/sessions"

CREATE=$(h2curl -w "\n%{http_code}" \
    -X POST "${GW_BASE}/v1/sessions" \
    -H "Content-Type: application/json" \
    -H "X-Request-ID: evs-c13-$(date +%s)" \
    -d "{
      \"id\":          \"${SESSION_C1}\",
      \"source_type\": \"raw_rtp\",
      \"codec\":       \"EVS\",
      \"sample_rate\": ${SAMPLE_RATE},
      \"ssrc\":        ${SSRC_C1}
    }")

BODY=$(echo "$CREATE" | head -1)
CODE=$(echo "$CREATE" | tail -1)
[[ "$CODE" == "201" ]] || fail "Create EVS session thất bại (HTTP $CODE): $BODY"
ok "EVS session created (HTTP 201)"
echo "$BODY" | python3 -m json.tool 2>/dev/null || echo "$BODY"

RTP_IP=$(echo "$BODY"   | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('rtp_ip','127.0.0.1'))")
RTP_PORT=$(echo "$BODY" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('rtp_port','0'))")
[[ -z "$RTP_PORT" || "$RTP_PORT" == "0" ]] && fail "Không lấy được rtp_port"
ok "RTP endpoint: ${RTP_IP}:${RTP_PORT}"

sleep 0.3

sep
info "Gửi ${RTP_PACKETS} EVS Compact 13200bps packet → ${RTP_IP}:${RTP_PORT}"

python3 - <<PYEOF
import socket, struct, time, sys

DEST    = ("${RTP_IP}", ${RTP_PORT})
SSRC    = ${SSRC_C1}
N       = ${RTP_PACKETS}
INTERVAL= ${PACKET_INTERVAL}
TS_INCR = ${TS_INCR}

# EVS RFC 8130 §3.4 Compact format — 13200bps:
#   47 bytes, không có header (toàn bộ payload là frame data)
#   byte[0] phải có bit7 = 0 để DetectEVSFormat trả về EVSCompact
#   Size 47 → EVSPrimaryBitrate(47) = 13200 bps
#
#   Byte pattern: 0x55 = 0b01010101 → bit7=0 (compact), alternating bits
#   Byte[0] = 0x55 → phù hợp với EVSCompact detection logic
PAYLOAD = bytes([0x55] + [0xAA] * 46)  # 47 bytes, byte[0]&0x80==0

sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
errors = 0

print(f"  EVS Compact: {len(PAYLOAD)} bytes | byte[0]=0x{PAYLOAD[0]:02X} | PT=98 (dynamic)")
print(f"  DetectEVSFormat: byte[0]&0x80=={PAYLOAD[0]&0x80} -> Compact ({'YES' if not (PAYLOAD[0]&0x80) else 'NO'})")
print(f"  EVSPrimaryBitrate(47) = 13200 bps")
print(f"  Sending {N} packets to {DEST[0]}:{DEST[1]} ...")

for i in range(N):
    seq = (i + 1) & 0xFFFF
    ts  = (i * TS_INCR) & 0xFFFFFFFF
    # RTP: V=2, P=0, X=0, CC=0, M=0, PT=98 (EVS dynamic, per RFC 8130)
    hdr = struct.pack('!BBHII', 0x80, 98, seq, ts, SSRC)
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
info "Chờ pipeline xử lý..."
sleep 4

DECODE_ERR_AFTER=$(get_metric "media_ai_pool_decode_errors_total")
SUBMITTED_AFTER=$(get_metric "media_ai_pool_submitted_total")
SUBMITTED_AFTER=$(get_metric "media_ai_pool_submitted_total")

D1=$(( ${DECODE_ERR_AFTER:-0} - ${DECODE_ERR_BEFORE:-0} ))
S1=$(( ${SUBMITTED_AFTER:-0} - ${SUBMITTED_BEFORE:-0} ))
R1=$(( ${SUBMITTED_AFTER:-0} - ${SUBMITTED_BEFORE:-0} ))

info "  pool_submitted : +${R1}  | decode_errors : +${D1}"
[[ "$R1" -ge 190 ]] && ok "Pipeline OK: +${R1}/200 jobs submitted ✓" || warn "Jobs submitted thấp: ${R1}/200"
[[ "$D1" -gt 0 ]]   && ok "Decode errors: +${D1} ✓ (expected)" || warn "Decode errors = 0 — kiểm tra lại"

info "DELETE session..."
h2curl -o /dev/null -X DELETE "${GW_BASE}/v1/sessions/${SESSION_C1}"
sleep 0.5

PORTS_AFTER=$(get_metric "media_ai_rtp_ports_available")
[[ "${PORTS_AFTER}" == "${PORTS_BEFORE}" ]] && ok "Port returned ✓" || warn "Port pool thay đổi"

# ═════════════════════════════════════════════════════════════════════════════
# SUB-TEST 2: EVS Primary Compact — 9600bps (33 bytes)
# ═════════════════════════════════════════════════════════════════════════════
sep
echo -e "${BOLD}╔══════════════════════════════════════════════════╗${NC}"
echo -e "${BOLD}║  Sub-test 2: EVS Compact  9600bps   33 bytes    ║${NC}"
echo -e "${BOLD}╚══════════════════════════════════════════════════╝${NC}"

info "RFC 8130 compact format:"
info "  Payload = 33 bytes, không có header byte"
info "  EVSPrimaryBitrate(33) → 9600 bps"
info "  Kiểm tra DetectEVSFormat với size khác sub-test 1"

SESSION_C2="evs-compact-9600-$(date +%s)"
SSRC_C2=58002

DECODE_ERR_BEFORE=$(get_metric "media_ai_pool_decode_errors_total")
SUBMITTED_BEFORE=$(get_metric "media_ai_pool_submitted_total")
SUBMITTED_BEFORE=$(get_metric "media_ai_pool_submitted_total")
PORTS_BEFORE=$(get_metric "media_ai_rtp_ports_available")

sep
info "Tạo session EVS (Compact 9600): ${GW_BASE}/v1/sessions"

CREATE=$(h2curl -w "\n%{http_code}" \
    -X POST "${GW_BASE}/v1/sessions" \
    -H "Content-Type: application/json" \
    -H "X-Request-ID: evs-c9-$(date +%s)" \
    -d "{
      \"id\":          \"${SESSION_C2}\",
      \"source_type\": \"raw_rtp\",
      \"codec\":       \"EVS\",
      \"sample_rate\": ${SAMPLE_RATE},
      \"ssrc\":        ${SSRC_C2}
    }")

BODY=$(echo "$CREATE" | head -1)
CODE=$(echo "$CREATE" | tail -1)
[[ "$CODE" == "201" ]] || fail "Create EVS session thất bại (HTTP $CODE): $BODY"
ok "EVS session created (HTTP 201)"

RTP_IP=$(echo "$BODY"   | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('rtp_ip','127.0.0.1'))")
RTP_PORT=$(echo "$BODY" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('rtp_port','0'))")
[[ -z "$RTP_PORT" || "$RTP_PORT" == "0" ]] && fail "Không lấy được rtp_port"
ok "RTP endpoint: ${RTP_IP}:${RTP_PORT}"

sleep 0.3

python3 - <<PYEOF
import socket, struct, time, sys

DEST    = ("${RTP_IP}", ${RTP_PORT})
SSRC    = ${SSRC_C2}
N       = ${RTP_PACKETS}
INTERVAL= ${PACKET_INTERVAL}
TS_INCR = ${TS_INCR}

# EVS Compact 9600bps: 33 bytes
# byte[0]=0x33 (bit7=0 → compact detection OK)
PAYLOAD = bytes([0x33] + [0x66] * 32)  # 33 bytes

sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
errors = 0
print(f"  EVS Compact 9600: {len(PAYLOAD)} bytes | byte[0]=0x{PAYLOAD[0]:02X} | PT=98")
print(f"  EVSPrimaryBitrate(33) = 9600 bps")
print(f"  Sending {N} packets ...")

for i in range(N):
    seq = (i + 1) & 0xFFFF
    ts  = (i * TS_INCR) & 0xFFFFFFFF
    hdr = struct.pack('!BBHII', 0x80, 98, seq, ts, SSRC)
    try:
        sock.sendto(hdr + PAYLOAD, DEST)
    except Exception as e:
        errors += 1
    if (i + 1) % 50 == 0:
        print(f"  Sent {i+1}/{N} ...")
    time.sleep(INTERVAL)

sock.close()
print(f"  Done: {N - errors}/{N} sent")
PYEOF

sleep 4

DECODE_ERR_AFTER=$(get_metric "media_ai_pool_decode_errors_total")
SUBMITTED_AFTER=$(get_metric "media_ai_pool_submitted_total")
D2=$(( ${DECODE_ERR_AFTER:-0} - ${DECODE_ERR_BEFORE:-0} ))
R2=$(( ${SUBMITTED_AFTER:-0} - ${SUBMITTED_BEFORE:-0} ))

info "  pool_submitted: +${R2}  | decode_errors: +${D2}"
[[ "$R2" -ge 190 ]] && ok "Pipeline OK: +${R2}/200 jobs submitted ✓" || warn "Jobs submitted thấp: ${R2}/200"
[[ "$D2" -gt 0 ]]   && ok "Decode errors: +${D2} ✓ (expected)" || warn "Decode errors = 0"

h2curl -o /dev/null -X DELETE "${GW_BASE}/v1/sessions/${SESSION_C2}"
sleep 0.5

PORTS_AFTER=$(get_metric "media_ai_rtp_ports_available")
[[ "${PORTS_AFTER}" == "${PORTS_BEFORE}" ]] && ok "Port returned ✓" || warn "Port pool thay đổi"

# ═════════════════════════════════════════════════════════════════════════════
# SUB-TEST 3: EVS Header-Full (byte[0] có H bit = 1)
# ═════════════════════════════════════════════════════════════════════════════
sep
echo -e "${BOLD}╔══════════════════════════════════════════════════╗${NC}"
echo -e "${BOLD}║  Sub-test 3: EVS Header-Full  1+47=48 bytes     ║${NC}"
echo -e "${BOLD}╚══════════════════════════════════════════════════╝${NC}"

info "RFC 8130 Header-Full format:"
info "  Byte 0: 0x98 = [H=1 | CMR[6:3]=0011 | padding[2:0]=000]"
info "          H=1 → DetectEVSFormat → EVSHeaderFull"
info "  Bytes 1-47: 47 bytes EVS primary frame payload"
info "  Total: 48 bytes/packet"
info "  Dùng khi cần tường minh CMR hoặc multi-frame packet"

SESSION_HF="evs-headerfull-$(date +%s)"
SSRC_HF=58003

DECODE_ERR_BEFORE=$(get_metric "media_ai_pool_decode_errors_total")
SUBMITTED_BEFORE=$(get_metric "media_ai_pool_submitted_total")
SUBMITTED_BEFORE=$(get_metric "media_ai_pool_submitted_total")
PORTS_BEFORE=$(get_metric "media_ai_rtp_ports_available")

sep
info "Tạo session EVS (Header-Full): ${GW_BASE}/v1/sessions"

CREATE=$(h2curl -w "\n%{http_code}" \
    -X POST "${GW_BASE}/v1/sessions" \
    -H "Content-Type: application/json" \
    -H "X-Request-ID: evs-hf-$(date +%s)" \
    -d "{
      \"id\":          \"${SESSION_HF}\",
      \"source_type\": \"raw_rtp\",
      \"codec\":       \"EVS\",
      \"sample_rate\": ${SAMPLE_RATE},
      \"ssrc\":        ${SSRC_HF}
    }")

BODY=$(echo "$CREATE" | head -1)
CODE=$(echo "$CREATE" | tail -1)
[[ "$CODE" == "201" ]] || fail "Create EVS Header-Full session thất bại (HTTP $CODE): $BODY"
ok "EVS Header-Full session created (HTTP 201)"

RTP_IP=$(echo "$BODY"   | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('rtp_ip','127.0.0.1'))")
RTP_PORT=$(echo "$BODY" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('rtp_port','0'))")
[[ -z "$RTP_PORT" || "$RTP_PORT" == "0" ]] && fail "Không lấy được rtp_port"
ok "RTP endpoint: ${RTP_IP}:${RTP_PORT}"

sleep 0.3

python3 - <<PYEOF
import socket, struct, time, sys

DEST    = ("${RTP_IP}", ${RTP_PORT})
SSRC    = ${SSRC_HF}
N       = ${RTP_PACKETS}
INTERVAL= ${PACKET_INTERVAL}
TS_INCR = ${TS_INCR}

# EVS RFC 8130 §4.2 Header-Full format:
#   Byte 0: H(1bit)=1 | CMR(4bits) | padding(3bits)
#   0x98 = 1001_1000:
#     H=1      → Header-Full, CMR byte present
#     CMR=0011 → CMR value 3 (request 13200bps primary mode)
#     pad=000
#   Bytes 1-47: EVS primary frame payload (47 bytes placeholder)
HEADER  = bytes([0x98])           # H=1, CMR=3
FRAME   = bytes([0xEF] * 47)     # placeholder EVS primary frame
PAYLOAD = HEADER + FRAME         # 48 bytes total

sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
errors = 0

print(f"  EVS Header-Full: {len(PAYLOAD)} bytes | byte[0]=0x{PAYLOAD[0]:02X}")
print(f"  H bit: byte[0]&0x80 = {PAYLOAD[0]&0x80:#04x} -> Header-Full detection")
print(f"  CMR bits[6:3] = {(PAYLOAD[0]>>3)&0xF} (request mode)")
print(f"  Frame payload: {len(FRAME)} bytes")
print(f"  Sending {N} packets to {DEST[0]}:{DEST[1]} ...")

for i in range(N):
    seq = (i + 1) & 0xFFFF
    ts  = (i * TS_INCR) & 0xFFFFFFFF
    hdr = struct.pack('!BBHII', 0x80, 98, seq, ts, SSRC)
    try:
        sock.sendto(hdr + PAYLOAD, DEST)
    except Exception as e:
        errors += 1
        print(f"  [!] packet {i+1}: {e}", file=sys.stderr)
    if (i + 1) % 50 == 0:
        print(f"  Sent {i+1}/{N} packets  (ts={ts})")
    time.sleep(INTERVAL)

sock.close()
print(f"  Done: {N - errors}/{N} sent  errors={errors}")
PYEOF

sleep 4

DECODE_ERR_AFTER=$(get_metric "media_ai_pool_decode_errors_total")
SUBMITTED_AFTER=$(get_metric "media_ai_pool_submitted_total")
D3=$(( ${DECODE_ERR_AFTER:-0} - ${DECODE_ERR_BEFORE:-0} ))
R3=$(( ${SUBMITTED_AFTER:-0} - ${SUBMITTED_BEFORE:-0} ))

info "  pool_submitted: +${R3}  | decode_errors: +${D3}"
[[ "$R3" -ge 190 ]] && ok "Pipeline OK: +${R3}/200 jobs submitted ✓" || warn "Jobs submitted thấp: ${R3}/200"
[[ "$D3" -gt 0 ]]   && ok "Decode errors: +${D3} ✓ (expected ErrEVSNotAvailable)" || warn "Decode errors = 0"

info "DELETE session EVS Header-Full..."
DEL_CODE=$(h2curl -o /dev/null -w "%{http_code}" -X DELETE "${GW_BASE}/v1/sessions/${SESSION_HF}")
[[ "$DEL_CODE" == "204" ]] && ok "Session deleted ✓" || warn "Delete HTTP $DEL_CODE"

sleep 0.5
PORTS_FINAL=$(get_metric "media_ai_rtp_ports_available")
[[ "${PORTS_FINAL}" == "${PORTS_BEFORE}" ]] && ok "Port returned ✓" || warn "Port pool thay đổi"

# ── Summary ───────────────────────────────────────────────────────────────────
sep
echo -e "${BOLD}═══════ EVS Test Summary ═══════${NC}"
echo ""
echo -e "  Gateway   : ${GW_BASE}  (${H2_MODE})"
echo -e "  Packets   : ${RTP_PACKETS} per sub-test × 3 = $((RTP_PACKETS * 3)) total"
echo ""
echo -e "  EVS Compact 13200bps (47 bytes, DetectEVSFormat → Compact):"
echo -e "    Jobs submitted : +${R1} | Decode errors: +${D1}"
echo ""
echo -e "  EVS Compact 9600bps (33 bytes, DetectEVSFormat → Compact):"
echo -e "    Jobs submitted : +${R2} | Decode errors: +${D2}"
echo ""
echo -e "  EVS Header-Full (48 bytes, byte[0]=0x98 H=1):"
echo -e "    Jobs submitted : +${R3} | Decode errors: +${D3}"
echo ""
echo -e "  Decode errors expected (ErrEVSNotAvailable)"
echo -e "  Để enable EVS decode:"
echo -e "    Download 3GPP EVS codec từ https://www.3gpp.org/ftp/Specs/archive/26_series/26.443/"
echo -e "    Build và link qua CGO trong internal/codec/evs.go (xem Option A)"
echo -e "    Hoặc dùng AMR-WB IO mode: amrDecoder với variant=amrWB"
sep

ROUTING_OK=true
[[ "$R1" -lt 190 || "$R2" -lt 190 || "$R3" -lt 190 ]] && ROUTING_OK=false

if $ROUTING_OK; then
    ok "EVS test PASSED (pipeline OK, decode errors expected khi chưa link CGO)"
else
    fail "EVS test FAILED — jobs submitted thấp: C13k=${R1} C9k=${R2} HF=${R3}"
fi
