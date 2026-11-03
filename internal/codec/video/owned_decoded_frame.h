#pragma once
#include <cstdint>
#include <vector>
#include "video_decoder.h"

struct OwnedDecodedFrame {
    uint64_t frame_id               = 0;
    uint32_t rtp_timestamp          = 0;
    uint64_t extended_rtp_timestamp = 0;
    int64_t  timestamp_ms           = 0;
    int      width                  = 0;
    int      height                 = 0;
    int      pixel_format           = VIDEO_PIX_FMT_UNKNOWN;
    int      stride0                = 0;
    int      stride1                = 0;
    int      stride2                = 0;
    int      data_size              = 0;
    int64_t  av_pts                 = 0;
    std::vector<uint8_t> buffer;
    // buffer.data() is the pixel data pointer exposed via DecodedVideoFrame.data
};
