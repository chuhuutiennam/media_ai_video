#pragma once
#include <cstdint>
#include <vector>

struct H264AccessUnit {
    uint32_t rtp_timestamp = 0;
    bool     key_frame     = false;
    bool     corrupted     = false;
    std::vector<uint8_t> data;
};
