#pragma once
#include "h264_access_unit.h"
#include <cstddef>
#include <cstdint>
#include <vector>

enum class ParseStatus {
    OK,
    EmptyPayload,
    PacketGap,
    DropFragment,
    InvalidStapA,
    InvalidFuA,
    SizeExceeded,
};

class H264RtpParser {
public:
    explicit H264RtpParser(
        size_t max_nalu_size        = 2 * 1024 * 1024,
        size_t max_access_unit_size = 8 * 1024 * 1024);

    ParseStatus Push(
        uint16_t       sequence,
        uint32_t       timestamp,
        bool           marker,
        const uint8_t* payload,
        size_t         payload_size);

    void NotifyPacketLoss(uint16_t expected_seq, uint16_t received_seq);

    bool HasAccessUnit() const;
    bool PopAccessUnit(H264AccessUnit& out);

    void Reset();

private:
    void        AppendNAL(const uint8_t* nalu, size_t size);
    void        FlushCurrentAccessUnit();
    void        StartAccessUnit(uint32_t ts);
    void        AbortFuA();
    void        MarkCurrentAUCorrupted();

    ParseStatus HandleSingleNal(uint16_t seq, uint32_t ts, bool marker,
                                const uint8_t* payload, size_t size);
    ParseStatus HandleFuA(uint16_t seq, uint32_t ts, bool marker,
                          const uint8_t* payload, size_t size);
    ParseStatus HandleStapA(uint16_t seq, uint32_t ts, bool marker,
                            const uint8_t* payload, size_t size);

    size_t max_nalu_size_;
    size_t max_access_unit_size_;

    // FU-A reassembly state
    std::vector<uint8_t> fu_buffer_;
    bool                 fu_in_progress_  = false;
    uint16_t             expected_fu_seq_ = 0;

    // Access Unit accumulation state
    uint32_t       current_timestamp_ = 0;
    bool           has_timestamp_     = false;
    H264AccessUnit current_au_;

    // SPS/PPS parameter set cache
    std::vector<uint8_t> sps_;
    std::vector<uint8_t> pps_;

    // IDR / loss recovery state
    bool wait_for_idr_ = true;

    // RTP sequence tracking for gap detection
    bool     seq_initialized_   = false;
    uint16_t next_expected_seq_ = 0;

    std::vector<H264AccessUnit> ready_aus_;
};
