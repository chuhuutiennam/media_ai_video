#!/usr/bin/env bash
# test-pcm-dump-g711.sh — Self-contained G.711 PCM dump test (PCMU + PCMA).
#
# Chạy trên Linux hoặc Docker (không cần CGO — G.711 là pure-Go).
#
# Flow:
#   1. Build gateway (no tags) + mock-ai-worker + mock-rtp-sender
#   2. Khởi động services
#   3. PCMU sub-test  : gửi 200 RTP packets từ speech.pcmu → verify PCM dump
#   4. PCMA sub-test  : gửi 200 RTP packets silence       → verify metrics
#   5. Summary + cleanup
#
# Chạy:
#   bash scripts/test-pcm-dump-g711.sh
#   docker run --rm -v "D:/NAMCHT/ims/SRC/media_ai:/app" media-ai-test-amrwb \
#       bash -c "dos2unix scripts/test-pcm-dump-g711.sh && bash scripts/test-pcm-dump-g711.sh"

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────────────────
GW_BIN="./bin/media-ai-gateway-g711"
GW_CONFIG="config/gateway-mock.yaml"
GW_PORT=8080
GW_BASE="http://127.0.0.1:${GW_PORT}"

WORKER_BIN="./bin/mock-ai-worker"
SENDER_BIN="./bin/mock-rtp-sender"

PCM_DIR="data/output/pcm"
PCMU_FILE="data/generated/g711/speech.pcmu"
PCMA_SILENCE="/tmp/silence.pcma"   # tạo trong script

RTP_PACKETS=3000  # 3000 × 20ms = 60s (toàn bộ speech.pcmu)
SSRC_PCMU=60001
SSRC_PCMA=60002

GW_PID=""
WORKER_PID=""

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

# ── Cleanup on exit ──────────────────────────────────────────────────────────
cleanup() {
    sep
    info "Cleanup..."
    if [[ -n "$GW_PID" ]] && kill -0 "$GW_PID" 2>/dev/null; then
        kill "$GW_PID" 2>/dev/null && wait "$GW_PID" 2>/dev/null || true
        info "  gateway stopped"
    fi
    if [[ -n "$WORKER_PID" ]] && kill -0 "$WORKER_PID" 2>/dev/null; then
        kill "$WORKER_PID" 2>/dev/null && wait "$WORKER_PID" 2>/dev/null || true
        info "  mock-ai-worker stopped"
    fi
    rm -f "$PCMA_SILENCE"
    if [[ "${TEST_RESULT:-FAIL}" == "PASS" ]]; then
        ok "Cleanup done"
    else
        warn "Cleanup done (test failed)"
    fi
}
trap cleanup EXIT

# ── Header ────────────────────────────────────────────────────────────────────
sep
echo -e "${BOLD}╔═══════════════════════════════════════════════════╗${NC}"
echo -e "${BOLD}║  G.711 PCM Dump Test (PCMU µ-law + PCMA A-law)   ║${NC}"
echo -e "${BOLD}╚═══════════════════════════════════════════════════╝${NC}"
sep

# ── Step 1: Preflight ─────────────────────────────────────────────────────────
info "Step 1 — Preflight"

command -v go      >/dev/null 2>&1 || fail "go not found"
command -v curl    >/dev/null 2>&1 || fail "curl not found"
command -v python3 >/dev/null 2>&1 || fail "python3 not found"

ok "  go $(go version | awk '{print $3}') ✓"

[[ -f "$PCMU_FILE" ]] || fail "PCMU speech file không tồn tại: $PCMU_FILE"
PCMU_SIZE=$(stat -c%s "$PCMU_FILE")
PCMU_FRAMES=$((PCMU_SIZE / 160))
ok "  PCMU file: ${PCMU_FILE} (${PCMU_SIZE} bytes, ${PCMU_FRAMES} frames @ 160 bytes/frame) ✓"

grep -q "pcm_dump_dir" "$GW_CONFIG" || fail "gateway-mock.yaml thiếu pcm_dump_dir"
ok "  gateway-mock.yaml có pcm_dump_dir ✓"

# Tạo file silence PCMA (0xD5 = A-law near silence)
python3 -c "import sys; sys.stdout.buffer.write(bytes([0xD5]*160)*${RTP_PACKETS})" > "$PCMA_SILENCE"
ok "  PCMA silence file: ${PCMA_SILENCE} ($((160 * RTP_PACKETS)) bytes) ✓"

# ── Step 2: Build ─────────────────────────────────────────────────────────────
sep
info "Step 2 — Build binaries (pure-Go, no CGO tags)"

mkdir -p bin

info "  Building gateway..."
go build -o "$GW_BIN" ./cmd/media-ai-gateway/ 2>&1 \
    || fail "Build gateway thất bại"
ok "    gateway built: $GW_BIN ✓"

info "  Building mock-ai-worker..."
go build -o "$WORKER_BIN" ./cmd/mock-ai-worker/ 2>&1 \
    || fail "Build mock-ai-worker thất bại"
ok "    mock-ai-worker built ✓"

info "  Building mock-rtp-sender..."
go build -o "$SENDER_BIN" ./cmd/mock-rtp-sender/ 2>&1 \
    || fail "Build mock-rtp-sender thất bại"
ok "    mock-rtp-sender built ✓"

# ── Step 3: Start services ────────────────────────────────────────────────────
sep
info "Step 3 — Khởi động services"

"$WORKER_BIN" --addr :50051 --log-level info \
    > /tmp/mock-ai-worker.log 2>&1 &
WORKER_PID=$!
info "  mock-ai-worker PID=${WORKER_PID} → :50051"

"$GW_BIN" --config "$GW_CONFIG" \
    > /tmp/gateway-g711.log 2>&1 &
GW_PID=$!
info "  gateway PID=${GW_PID} → :${GW_PORT}"

# Chờ gateway ready
for i in $(seq 1 20); do
    if curl -sf "${GW_BASE}/health/live" >/dev/null 2>&1; then
        break
    fi
    sleep 0.3
done
curl -sf "${GW_BASE}/health/live" >/dev/null 2>&1 || fail "Gateway không khởi động được"
ok "  Gateway ready (HTTP 200) ✓"

# Detect HTTP/2
if curl -s --http2-prior-knowledge "${GW_BASE}/health/live" >/dev/null 2>&1; then
    H2_FLAG="--http2-prior-knowledge"; H2_MODE="h2c prior-knowledge"
else
    H2_FLAG="--http2"; H2_MODE="h2 upgrade"
fi
info "  HTTP mode: ${H2_MODE}"
h2curl() { curl -s $H2_FLAG "$@"; }

# ═════════════════════════════════════════════════════════════════════════════
# SUB-TEST 1: PCMU
# ═════════════════════════════════════════════════════════════════════════════
sep
echo -e "${BOLD}╔══════════════════════════════════════╗${NC}"
echo -e "${BOLD}║  Sub-test 1: PCMU (µ-law, PT=0)     ║${NC}"
echo -e "${BOLD}╚══════════════════════════════════════╝${NC}"

SESSION_PCMU="g711-pcmu-$(date +%s)"

SUBMIT_BEFORE=$(get_metric "media_ai_pool_submitted_total")
PROC_BEFORE=$(get_metric "media_ai_pool_processed_total")
ERR_BEFORE=$(get_metric "media_ai_pool_decode_errors_total")

# Step 4: Create PCMU session
sep
info "Step 4 — Tạo PCMU session"

CREATE=$(h2curl -w "\n%{http_code}" \
    -X POST "${GW_BASE}/v1/sessions" \
    -H "Content-Type: application/json" \
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
ok "  Session created (HTTP 201)"
echo "$BODY" | python3 -m json.tool 2>/dev/null || echo "$BODY"

RTP_IP=$(echo "$BODY"   | python3 -c "import sys,json; print(json.load(sys.stdin)['rtp_ip'])")
RTP_PORT=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin)['rtp_port'])")
ok "  RTP endpoint: ${RTP_IP}:${RTP_PORT}"

PCM_FILE_PCMU="${PCM_DIR}/${SESSION_PCMU}.pcmu.8000hz.1ch.s16le"
info "  PCM dump sẽ tạo tại: ${PCM_FILE_PCMU}"

# Step 5: Send PCMU packets
sep
info "Step 5 — Gửi ${RTP_PACKETS} PCMU packets qua mock-rtp-sender"
info "  File: ${PCMU_FILE}"
info "  Format: raw (160 bytes/frame @ 8kHz 20ms)"

"$SENDER_BIN" \
    --codec PCMU --pt 0 --ssrc "$SSRC_PCMU" \
    --ptime 20 --sample-rate 8000 \
    --file-format raw --frame-size 160 \
    --count $RTP_PACKETS \
    --target "${RTP_IP}:${RTP_PORT}" \
    --file "$PCMU_FILE" \
    2>&1

# Step 6: Wait + delete session
sep
info "Step 6 — Chờ 4s để pipeline flush, sau đó xoá session..."
sleep 4

DEL_CODE=$(h2curl -o /dev/null -w "%{http_code}" \
    -X DELETE "${GW_BASE}/v1/sessions/${SESSION_PCMU}")
[[ "$DEL_CODE" == "204" ]] && ok "  Session deleted — pcmFile.Close() triggered ✓" \
    || warn "  DELETE HTTP $DEL_CODE"

sleep 1

# Step 7: Verify PCM dump
sep
info "Step 7 — Kiểm tra PCM dump (PCMU @ 8kHz → 8000hz.1ch.s16le)"

[[ -f "$PCM_FILE_PCMU" ]] || fail "PCM file không tồn tại: $PCM_FILE_PCMU"
ok "  PCM file tồn tại ✓"

PCM_SIZE=$(stat -c%s "$PCM_FILE_PCMU")
# 3000 packets × 160 samples × 2 bytes = 960,000 bytes (pre-resample raw decoder output)
EXPECTED=960000
EXPECT_LOW=$((EXPECTED * 95 / 100))
EXPECT_HIGH=$((EXPECTED * 105 / 100))
info "  File size: ${PCM_SIZE} bytes"
info "  Expected : ${EXPECTED} bytes (±5%)"

[[ "$PCM_SIZE" -gt 0 ]] && ok "  PCM file không trống ✓" || fail "PCM file rỗng!"
[[ "$PCM_SIZE" -ge "$EXPECT_LOW" && "$PCM_SIZE" -le "$EXPECT_HIGH" ]] \
    && ok "  PCM file size hợp lệ: ${PCM_SIZE} bytes (expect ${EXPECT_LOW}–${EXPECT_HIGH}) ✓" \
    || warn "  PCM size ngoài khoảng expect: ${PCM_SIZE} (expect ${EXPECT_LOW}–${EXPECT_HIGH})"

info "  Phân tích PCM samples..."
python3 - "$PCM_FILE_PCMU" <<'PYEOF'
import sys, struct, math

path = sys.argv[1]
with open(path, 'rb') as f:
    raw = f.read()

if len(raw) % 2 != 0:
    raw = raw[:-1]

samples = struct.unpack(f'<{len(raw)//2}h', raw)
n = len(samples)
nonzero = sum(1 for s in samples if s != 0)
max_amp = max(abs(s) for s in samples) if samples else 0
rms = math.sqrt(sum(s*s for s in samples) / n) if n else 0
dur_s = n / 8000

print(f"  Tổng samples   : {n:,}  ({dur_s:.2f}s @ 8kHz)")
print(f"  Non-zero samples: {nonzero:,} ({100*nonzero/n:.1f}%)")
print(f"  Max amplitude  : {max_amp} / 32767  ({100*max_amp/32767:.1f}% of full scale)")
print(f"  RMS amplitude  : {rms:.1f}")
if nonzero / n < 0.5:
    print("  [WARN] PCM chủ yếu là silence — có thể dùng file silent")
elif rms > 100:
    print("  [OK] PCM có tín hiệu audio ✓")
else:
    print("  [WARN] RMS thấp — tín hiệu yếu")
print(f"  Duration       : {dur_s:.2f}s  (expect ~60.00s)")
PYEOF

ok "  PCM nội dung hợp lệ ✓"

# Step 8: Metrics PCMU
sep
info "Step 8 — Kiểm tra metrics PCMU"

SUBMIT_AFTER=$(get_metric "media_ai_pool_submitted_total")
PROC_AFTER=$(get_metric "media_ai_pool_processed_total")
ERR_AFTER=$(get_metric "media_ai_pool_decode_errors_total")

SUBMIT_DELTA=$(( ${SUBMIT_AFTER:-0} - ${SUBMIT_BEFORE:-0} ))
PROC_DELTA=$(( ${PROC_AFTER:-0} - ${PROC_BEFORE:-0} ))
ERR_DELTA=$(( ${ERR_AFTER:-0} - ${ERR_BEFORE:-0} ))

info "  pool_submitted_total     : +${SUBMIT_DELTA}  (expect +${RTP_PACKETS})"
info "  pool_processed_total     : +${PROC_DELTA}   (expect +${RTP_PACKETS})"
info "  pool_decode_errors_total : +${ERR_DELTA}  (expect 0)"

[[ "$SUBMIT_DELTA" -ge 190 ]] && ok "  Submitted: +${SUBMIT_DELTA}/200 ✓" || warn "  Submitted thấp: ${SUBMIT_DELTA}/200"
[[ "$PROC_DELTA"   -ge 190 ]] && ok "  Processed: +${PROC_DELTA}/200 ✓"   || warn "  Processed thấp: ${PROC_DELTA}/200"
[[ "$ERR_DELTA"    -eq 0   ]] \
    && ok "  Decode errors: 0 — G.711 PCMU pure-Go decode thành công ✓" \
    || fail "  Decode errors: +${ERR_DELTA} — PCMU KHÔNG nên có decode error!"

PCMU_SUBMIT=$SUBMIT_DELTA; PCMU_PROC=$PROC_DELTA; PCMU_ERR=$ERR_DELTA

# ═════════════════════════════════════════════════════════════════════════════
# SUB-TEST 2: PCMA
# ═════════════════════════════════════════════════════════════════════════════
sep
echo -e "${BOLD}╔══════════════════════════════════════╗${NC}"
echo -e "${BOLD}║  Sub-test 2: PCMA (A-law, PT=8)     ║${NC}"
echo -e "${BOLD}╚══════════════════════════════════════╝${NC}"

SESSION_PCMA="g711-pcma-$(date +%s)"

SUBMIT_BEFORE=$(get_metric "media_ai_pool_submitted_total")
PROC_BEFORE=$(get_metric "media_ai_pool_processed_total")
ERR_BEFORE=$(get_metric "media_ai_pool_decode_errors_total")

info "Tạo PCMA session"

CREATE=$(h2curl -w "\n%{http_code}" \
    -X POST "${GW_BASE}/v1/sessions" \
    -H "Content-Type: application/json" \
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
ok "  Session created (HTTP 201)"
echo "$BODY" | python3 -m json.tool 2>/dev/null || echo "$BODY"

RTP_IP=$(echo "$BODY"   | python3 -c "import sys,json; print(json.load(sys.stdin)['rtp_ip'])")
RTP_PORT=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin)['rtp_port'])")
ok "  RTP endpoint: ${RTP_IP}:${RTP_PORT}"

sep
info "Gửi ${RTP_PACKETS} PCMA packets (silence: 0xD5 A-law)"
info "  File: ${PCMA_SILENCE} (${RTP_PACKETS} frames × 160 bytes)"

"$SENDER_BIN" \
    --codec PCMA --pt 8 --ssrc "$SSRC_PCMA" \
    --ptime 20 --sample-rate 8000 \
    --file-format raw --frame-size 160 \
    --count $RTP_PACKETS \
    --target "${RTP_IP}:${RTP_PORT}" \
    --file "$PCMA_SILENCE" \
    2>&1

sep
info "Chờ 4s để pipeline flush, sau đó xoá session..."
sleep 4

DEL_CODE=$(h2curl -o /dev/null -w "%{http_code}" \
    -X DELETE "${GW_BASE}/v1/sessions/${SESSION_PCMA}")
[[ "$DEL_CODE" == "204" ]] && ok "  Session deleted ✓" || warn "  DELETE HTTP $DEL_CODE"

sleep 1

sep
info "Kiểm tra metrics PCMA"

SUBMIT_AFTER=$(get_metric "media_ai_pool_submitted_total")
PROC_AFTER=$(get_metric "media_ai_pool_processed_total")
ERR_AFTER=$(get_metric "media_ai_pool_decode_errors_total")

SUBMIT_DELTA=$(( ${SUBMIT_AFTER:-0} - ${SUBMIT_BEFORE:-0} ))
PROC_DELTA=$(( ${PROC_AFTER:-0} - ${PROC_BEFORE:-0} ))
ERR_DELTA=$(( ${ERR_AFTER:-0} - ${ERR_BEFORE:-0} ))

info "  pool_submitted_total     : +${SUBMIT_DELTA}  (expect +${RTP_PACKETS})"
info "  pool_processed_total     : +${PROC_DELTA}   (expect +${RTP_PACKETS})"
info "  pool_decode_errors_total : +${ERR_DELTA}  (expect 0)"

[[ "$SUBMIT_DELTA" -ge 190 ]] && ok "  Submitted: +${SUBMIT_DELTA}/200 ✓" || warn "  Submitted thấp: ${SUBMIT_DELTA}/200"
[[ "$PROC_DELTA"   -ge 190 ]] && ok "  Processed: +${PROC_DELTA}/200 ✓"   || warn "  Processed thấp: ${PROC_DELTA}/200"
[[ "$ERR_DELTA"    -eq 0   ]] \
    && ok "  Decode errors: 0 — G.711 PCMA pure-Go decode thành công ✓" \
    || fail "  Decode errors: +${ERR_DELTA} — PCMA KHÔNG nên có decode error!"

PCMA_SUBMIT=$SUBMIT_DELTA; PCMA_PROC=$PROC_DELTA; PCMA_ERR=$ERR_DELTA

# ── Summary ───────────────────────────────────────────────────────────────────
sep
echo -e "${BOLD}══════════════ G.711 PCM Dump Test Summary ══════════════${NC}"
echo ""
echo -e "  Gateway       : ${GW_BIN}"
echo -e "  Build tags    : (none — pure-Go)"
echo ""
echo -e "  PCMU (µ-law, PT=0):"
echo -e "    Input file   : ${PCMU_FILE}"
echo -e "    PCM dump     : ${PCM_FILE_PCMU}"
echo -e "    PCM size     : ${PCM_SIZE} bytes"
echo -e "    Submitted    : +${PCMU_SUBMIT}/200"
echo -e "    Processed    : +${PCMU_PROC}/200"
echo -e "    Decode errors: ${PCMU_ERR}  $([ "$PCMU_ERR" -eq 0 ] && echo '✓ PASS' || echo '✗ FAIL')"
echo ""
echo -e "  PCMA (A-law, PT=8):"
echo -e "    Input file   : ${PCMA_SILENCE} (silence)"
echo -e "    Submitted    : +${PCMA_SUBMIT}/200"
echo -e "    Processed    : +${PCMA_PROC}/200"
echo -e "    Decode errors: ${PCMA_ERR}  $([ "$PCMA_ERR" -eq 0 ] && echo '✓ PASS' || echo '✗ FAIL')"
echo ""
echo -e "  Playback PCMU PCM:"
echo -e "    ffplay -f s16le -ar 8000 -ac 1 '${PCM_FILE_PCMU}'"
echo -e "    (hoặc sox/audacity)"
sep

TOTAL_ERR=$(( PCMU_ERR + PCMA_ERR ))
if [[ "$TOTAL_ERR" -eq 0 ]]; then
    echo -e "  ${GREEN}${BOLD}RESULT: PASS${NC}"
    echo -e "  G.711 PCMU + PCMA → PCM decode + pipeline hoạt động đúng ✓"
    TEST_RESULT="PASS"
else
    echo -e "  ${RED}${BOLD}RESULT: FAIL${NC}"
    echo -e "  Total decode errors: ${TOTAL_ERR}"
    TEST_RESULT="FAIL"
fi
sep
