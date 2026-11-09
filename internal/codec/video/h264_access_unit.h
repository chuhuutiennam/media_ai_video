#pragma once
#include <cstdint>
#include <vector>

struct H264AccessUnit {
    uint32_t rtp_timestamp = 0;
    bool     key_frame     = false;
    bool     corrupted     = false;
    bool     wait_idr_dropped = false; // có slice bị bỏ vì parser đang chờ IDR
    std::vector<uint8_t> data;
};
