#include "rtp_timestamp_tracker.h"

static constexpr uint64_t kRtpClockHz = 90000;
static constexpr uint32_t kHalfRange  = 0x80000000u;

uint64_t RtpTimestampTracker::Extend(uint32_t rtp_ts) {
    if (!initialized_) {
        initialized_   = true;
        last_ts_       = rtp_ts;
        base_extended_ = static_cast<uint64_t>(rtp_ts);
        base_set_      = true;
        return static_cast<uint64_t>(rtp_ts);
    }

    // Detect 32-bit wrap-around: new value is much smaller than last
    if (rtp_ts < last_ts_ && (last_ts_ - rtp_ts) > kHalfRange) {
        wrap_count_++;
    }
    last_ts_ = rtp_ts;

    uint64_t extended = (wrap_count_ << 32) | static_cast<uint64_t>(rtp_ts);
    if (!base_set_) {
        base_extended_ = extended;
        base_set_      = true;
    }
    return extended;
}

int64_t RtpTimestampTracker::ToMilliseconds(uint64_t extended_ts) const {
    if (!base_set_) return 0;
    uint64_t delta = extended_ts - base_extended_;
    return static_cast<int64_t>(delta * 1000 / kRtpClockHz);
}

void RtpTimestampTracker::Reset() {
    initialized_   = false;
    last_ts_       = 0;
    wrap_count_    = 0;
    base_extended_ = 0;
    base_set_      = false;
}
