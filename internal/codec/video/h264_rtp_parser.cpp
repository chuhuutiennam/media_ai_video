#include "h264_rtp_parser.h"

static constexpr uint8_t kStartCode[4] = {0x00, 0x00, 0x00, 0x01};

static void AppendAnnexB(std::vector<uint8_t>& buf, const uint8_t* nalu, size_t size) {
    buf.insert(buf.end(), kStartCode, kStartCode + 4);
    buf.insert(buf.end(), nalu, nalu + size);
}

H264RtpParser::H264RtpParser(size_t max_nalu_size, size_t max_access_unit_size)
    : max_nalu_size_(max_nalu_size)
    , max_access_unit_size_(max_access_unit_size)
{}

ParseStatus H264RtpParser::Push(
    uint16_t seq, uint32_t ts, bool marker,
    const uint8_t* payload, size_t payload_size)
{
    if (!payload || payload_size == 0) return ParseStatus::EmptyPayload;

    if (!seq_initialized_) {
        seq_initialized_   = true;
        next_expected_seq_ = seq;
    } else if (seq != next_expected_seq_) {
        NotifyPacketLoss(next_expected_seq_, seq);
    }
    next_expected_seq_ = static_cast<uint16_t>(seq + 1);

    uint8_t nal_type = payload[0] & 0x1F;

    if (nal_type >= 1 && nal_type <= 23) return HandleSingleNal(seq, ts, marker, payload, payload_size);
    if (nal_type == 24)                  return HandleStapA(seq, ts, marker, payload, payload_size);
    if (nal_type == 28)                  return HandleFuA(seq, ts, marker, payload, payload_size);

    return ParseStatus::OK; // unsupported types ignored
}

void H264RtpParser::NotifyPacketLoss(uint16_t /*expected_seq*/, uint16_t /*received_seq*/) {
    AbortFuA();
    MarkCurrentAUCorrupted();
}

bool H264RtpParser::HasAccessUnit() const {
    return !ready_aus_.empty();
}

bool H264RtpParser::PopAccessUnit(H264AccessUnit& out) {
    if (ready_aus_.empty()) return false;
    out = std::move(ready_aus_.front());
    ready_aus_.erase(ready_aus_.begin());
    return true;
}

void H264RtpParser::Reset() {
    fu_buffer_.clear();
    fu_in_progress_    = false;
    expected_fu_seq_   = 0;
    current_timestamp_ = 0;
    has_timestamp_     = false;
    current_au_        = H264AccessUnit{};
    sps_.clear();
    pps_.clear();
    wait_for_idr_      = true;
    seq_initialized_   = false;
    next_expected_seq_ = 0;
    ready_aus_.clear();
}

void H264RtpParser::AppendNAL(const uint8_t* nalu, size_t size) {
    if (size == 0) return;

    uint8_t nal_type = nalu[0] & 0x1F;
    bool    is_sps   = (nal_type == 7);
    bool    is_pps   = (nal_type == 8);
    bool    is_idr   = (nal_type == 5);

    if (size > max_nalu_size_) {
        MarkCurrentAUCorrupted();
        return;
    }

    if (is_sps) sps_.assign(nalu, nalu + size);
    if (is_pps) pps_.assign(nalu, nalu + size);

    if (wait_for_idr_) {
        if (is_sps || is_pps) return; // cache only, emit on IDR
        if (!is_idr)          return; // drop until IDR

        // IDR: prepend cached SPS + PPS so decoder can initialize
        wait_for_idr_ = false;
        if (!sps_.empty()) AppendAnnexB(current_au_.data, sps_.data(), sps_.size());
        if (!pps_.empty()) AppendAnnexB(current_au_.data, pps_.data(), pps_.size());
    }

    if (current_au_.data.size() + 4 + size > max_access_unit_size_) {
        MarkCurrentAUCorrupted();
        return;
    }

    if (is_idr) current_au_.key_frame = true;
    AppendAnnexB(current_au_.data, nalu, size);
}

void H264RtpParser::FlushCurrentAccessUnit() {
    if (!has_timestamp_) return;
    if (!current_au_.data.empty() && !current_au_.corrupted) {
        ready_aus_.push_back(std::move(current_au_));
    }
    current_au_    = H264AccessUnit{};
    has_timestamp_ = false;
}

void H264RtpParser::StartAccessUnit(uint32_t ts) {
    current_timestamp_        = ts;
    has_timestamp_            = true;
    current_au_.rtp_timestamp = ts;
    current_au_.key_frame     = false;
    current_au_.corrupted     = false;
    current_au_.data.clear();
}

void H264RtpParser::AbortFuA() {
    fu_buffer_.clear();
    fu_in_progress_ = false;
}

void H264RtpParser::MarkCurrentAUCorrupted() {
    current_au_    = H264AccessUnit{};
    has_timestamp_ = false;
    wait_for_idr_  = true; // DROP_UNTIL_IDR policy
}

// ── packet type handlers ──────────────────────────────────────────────────────

ParseStatus H264RtpParser::HandleSingleNal(
    uint16_t /*seq*/, uint32_t ts, bool marker,
    const uint8_t* payload, size_t size)
{
    if (fu_in_progress_) AbortFuA();
    if (has_timestamp_ && ts != current_timestamp_) FlushCurrentAccessUnit();
    if (!has_timestamp_) StartAccessUnit(ts);
    AppendNAL(payload, size);
    if (marker) FlushCurrentAccessUnit();
    return ParseStatus::OK;
}

ParseStatus H264RtpParser::HandleFuA(
    uint16_t seq, uint32_t ts, bool marker,
    const uint8_t* payload, size_t size)
{
    if (size < 2) return ParseStatus::InvalidFuA;

    uint8_t fu_indicator = payload[0];
    uint8_t fu_header    = payload[1];
    bool    start        = (fu_header >> 7) & 1;
    bool    end          = (fu_header >> 6) & 1;
    uint8_t recon_nal    = (fu_indicator & 0xE0) | (fu_header & 0x1F);

    if (start) {
        AbortFuA(); // discard any incomplete prior fragment
        fu_buffer_.clear();
        fu_buffer_.push_back(recon_nal);
        if (size > 2) fu_buffer_.insert(fu_buffer_.end(), payload + 2, payload + size);
        fu_in_progress_  = true;
        expected_fu_seq_ = static_cast<uint16_t>(seq + 1);

        if (has_timestamp_ && ts != current_timestamp_) FlushCurrentAccessUnit();
        if (!has_timestamp_) StartAccessUnit(ts);
    } else {
        if (!fu_in_progress_) {
            // Mid-fragment arrived without a start → previous start was lost
            MarkCurrentAUCorrupted();
            return ParseStatus::DropFragment;
        }
        if (seq != expected_fu_seq_) {
            AbortFuA();
            MarkCurrentAUCorrupted();
            return ParseStatus::PacketGap;
        }
        if (size > 2) fu_buffer_.insert(fu_buffer_.end(), payload + 2, payload + size);
        expected_fu_seq_ = static_cast<uint16_t>(seq + 1);
    }

    if (end && fu_in_progress_) {
        fu_in_progress_ = false;
        AppendNAL(fu_buffer_.data(), fu_buffer_.size());
        fu_buffer_.clear();
        if (marker) FlushCurrentAccessUnit();
    }

    return ParseStatus::OK;
}

ParseStatus H264RtpParser::HandleStapA(
    uint16_t /*seq*/, uint32_t ts, bool marker,
    const uint8_t* payload, size_t size)
{
    if (fu_in_progress_) AbortFuA();
    if (has_timestamp_ && ts != current_timestamp_) FlushCurrentAccessUnit();
    if (!has_timestamp_) StartAccessUnit(ts);

    size_t offset = 1; // skip STAP-A header byte
    while (offset + 2 <= size) {
        uint16_t nalu_size = (static_cast<uint16_t>(payload[offset]) << 8)
                           |  static_cast<uint16_t>(payload[offset + 1]);
        offset += 2;

        if (nalu_size == 0 || offset + nalu_size > size) {
            MarkCurrentAUCorrupted();
            return ParseStatus::InvalidStapA;
        }

        AppendNAL(payload + offset, nalu_size);
        offset += nalu_size;
    }

    if (marker) FlushCurrentAccessUnit();
    return ParseStatus::OK;
}
