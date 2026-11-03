#pragma once
#include "h264_rtp_parser.h"
#include "h264_decoder.h"
#include "rtp_timestamp_tracker.h"
#include "video_decoder.h"
#include <deque>
#include <unordered_map>

class VideoStreamDecoder {
public:
    explicit VideoStreamDecoder(const VideoDecoderConfig& config);
    ~VideoStreamDecoder();

    bool Open();
    void Close();

    VideoStatus PushRtp(
        uint16_t       sequence,
        uint32_t       timestamp,
        bool           marker,
        const uint8_t* payload,
        int            payload_size);

    VideoStatus ReceiveFrame(DecodedVideoFrame& out);
    void        ReleaseFrame(DecodedVideoFrame* frame);

    void NotifyPacketLoss(uint16_t expected_seq, uint16_t received_seq);
    void Reset();

    const VideoDecoderStats& GetStats() const { return stats_; }

private:
    void DrainDecoder(uint32_t rtp_ts, uint64_t ext_ts, int64_t ts_ms);

    VideoDecoderConfig config_;
    VideoDecoderStats  stats_ = {};

    H264RtpParser       parser_;
    H264Decoder         decoder_;
    RtpTimestampTracker ts_tracker_;

    std::deque<OwnedDecodedFrame>                    ready_frames_;
    std::unordered_map<uint8_t*, OwnedDecodedFrame>  leased_frames_;

    uint64_t next_frame_id_ = 1;
    bool     opened_        = false;
};
