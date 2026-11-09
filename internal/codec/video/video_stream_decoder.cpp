#include "video_stream_decoder.h"
#include <cstring>

// Giá trị mặc định của VideoDecoderConfig nằm ở MakeConfig (video_decoder_c_api.cpp).

VideoStreamDecoder::VideoStreamDecoder(const VideoDecoderConfig& config)
    : config_(config)
    , parser_(config.max_nalu_size, config.max_access_unit_size)
{}

VideoStreamDecoder::~VideoStreamDecoder() {
    Close();
}

bool VideoStreamDecoder::Open() {
    if (opened_) return true;
    if (!decoder_.Open()) return false;
    opened_ = true;
    return true;
}

void VideoStreamDecoder::Close() {
    decoder_.Close();
    ready_frames_.clear();
    leased_frames_.clear();
    opened_ = false;
}

VideoStatus VideoStreamDecoder::PushRtp(
    uint16_t seq, uint32_t ts, bool marker,
    const uint8_t* payload, int payload_size)
{
    if (!opened_)                     return VIDEO_ERR_DECODER_INIT;
    if (!payload || payload_size <= 0) return VIDEO_ERR_INVALID_ARGUMENT;

    stats_.rtp_packets_input++;

    // Track packet type for stats (before parsing)
    if (payload_size > 0) {
        uint8_t nt = payload[0] & 0x1F;
        if (nt >= 1 && nt <= 23)      stats_.single_nalu_packets++;
        else if (nt == 24)            stats_.stapa_packets++;
        else if (nt == 28)            stats_.fua_packets++;
    }

    const uint64_t gaps_before = parser_.SeqGaps();
    ParseStatus ps = parser_.Push(
        seq, ts, marker, payload, static_cast<size_t>(payload_size));
    // Sequence đứt (gói mất trước gateway hoặc rơi ở hàng đợi nội bộ): parser đã bỏ AU dở
    // và chuyển sang chờ IDR; đếm một lần cho lần Push này.
    const bool seq_gap = parser_.SeqGaps() != gaps_before;
    if (seq_gap) stats_.packet_gaps++;

    switch (ps) {
    case ParseStatus::PacketGap:
        if (!seq_gap) stats_.packet_gaps++;
        stats_.corrupted_access_units++;
        break;
    case ParseStatus::DropFragment:
        stats_.corrupted_access_units++;
        break;
    case ParseStatus::InvalidStapA:
        return VIDEO_ERR_INVALID_STAPA;
    case ParseStatus::InvalidFuA:
        return VIDEO_ERR_INVALID_FUA;
    case ParseStatus::EmptyPayload:
        return VIDEO_ERR_INVALID_PAYLOAD;
    case ParseStatus::SizeExceeded:
        stats_.corrupted_access_units++;
        break;
    default:
        break;
    }

    H264AccessUnit au;
    while (parser_.PopAccessUnit(au)) {
        stats_.access_units_completed++;
        if (au.key_frame) stats_.idr_frames++;

        uint64_t ext_ts = ts_tracker_.Extend(au.rtp_timestamp);
        int64_t  ts_ms  = ts_tracker_.ToMilliseconds(ext_ts);

        DecodeStatus ds = decoder_.Decode(
            au.data.data(), static_cast<int>(au.data.size()),
            static_cast<int64_t>(ext_ts));

        if (ds != DecodeStatus::OK) {
            stats_.decode_errors++;
            continue;
        }

        DrainDecoder(au.rtp_timestamp, ext_ts, ts_ms);
    }

    return (seq_gap || ps == ParseStatus::PacketGap) ? VIDEO_PACKET_GAP : VIDEO_OK;
}

VideoDecoderStats VideoStreamDecoder::GetStats() const {
    VideoDecoderStats s = stats_;
    s.wait_idr_drops    = parser_.WaitIdrDrops();
    s.wait_idr_episodes = parser_.WaitIdrEpisodes();
    s.waiting_for_idr   = parser_.WaitingForIdr() ? 1 : 0;
    return s;
}

void VideoStreamDecoder::DrainDecoder(
    uint32_t rtp_ts, uint64_t ext_ts, int64_t ts_ms)
{
    OwnedDecodedFrame frame;
    while (decoder_.PopFrame(frame)) {
        frame.frame_id               = next_frame_id_++;
        frame.rtp_timestamp          = rtp_ts;
        frame.extended_rtp_timestamp = ext_ts;
        frame.timestamp_ms           = ts_ms;

        if (static_cast<int>(ready_frames_.size()) >= config_.max_ready_frames) {
            ready_frames_.pop_front(); // drop oldest; fresh frame preferred
        }
        ready_frames_.push_back(std::move(frame));
        stats_.decoded_frames++;
    }
}

VideoStatus VideoStreamDecoder::ReceiveFrame(DecodedVideoFrame& out) {
    if (ready_frames_.empty()) return VIDEO_NO_FRAME;

    OwnedDecodedFrame owned = std::move(ready_frames_.front());
    ready_frames_.pop_front();

    out.frame_id               = owned.frame_id;
    out.rtp_timestamp          = owned.rtp_timestamp;
    out.extended_rtp_timestamp = owned.extended_rtp_timestamp;
    out.timestamp_ms           = owned.timestamp_ms;
    out.width                  = owned.width;
    out.height                 = owned.height;
    out.pixel_format           = static_cast<VideoPixelFormat>(owned.pixel_format);
    out.data                   = owned.buffer.data();
    out.data_size              = owned.data_size;
    out.stride0                = owned.stride0;
    out.stride1                = owned.stride1;
    out.stride2                = owned.stride2;

    // Key into leased map; buffer.data() is stable after vector move
    uint8_t* key = owned.buffer.data();
    leased_frames_[key] = std::move(owned);

    return VIDEO_OK;
}

void VideoStreamDecoder::ReleaseFrame(DecodedVideoFrame* frame) {
    if (!frame || !frame->data) return;
    leased_frames_.erase(frame->data);
    frame->data      = nullptr;
    frame->data_size = 0;
}

void VideoStreamDecoder::NotifyPacketLoss(uint16_t expected_seq, uint16_t received_seq) {
    parser_.NotifyPacketLoss(expected_seq, received_seq);
    stats_.packet_gaps++;
    stats_.corrupted_access_units++;
}

void VideoStreamDecoder::Reset() {
    parser_.Reset();
    decoder_.Flush();
    ts_tracker_.Reset();
    ready_frames_.clear();
    leased_frames_.clear();
    next_frame_id_ = 1;
    memset(&stats_, 0, sizeof(stats_));
}
