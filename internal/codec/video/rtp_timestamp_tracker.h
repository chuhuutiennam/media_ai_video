#pragma once
#include <cstdint>

class RtpTimestampTracker {
public:
    uint64_t Extend(uint32_t rtp_ts);
    int64_t  ToMilliseconds(uint64_t extended_ts) const;
    void     Reset();

private:
    bool     initialized_   = false;
    uint32_t last_ts_       = 0;
    uint64_t wrap_count_    = 0;
    uint64_t base_extended_ = 0;
    bool     base_set_      = false;
};
