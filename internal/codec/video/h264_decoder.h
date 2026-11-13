#pragma once
#include "owned_decoded_frame.h"
#include "h264_access_unit.h"
#include <deque>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavutil/imgutils.h>
}

enum class DecodeStatus {
    OK,
    NotOpened,
    SendError,
    ReceiveError,
    Oversize, // libavcodec từ chối vì độ phân giải của stream vượt giới hạn
};

class H264Decoder {
public:
    H264Decoder();
    ~H264Decoder();

    // max_width/max_height > 0: giới hạn độ phân giải theo khung, không phân biệt ngang/dọc
    // (cạnh dài ≤ cạnh dài giới hạn, cạnh ngắn ≤ cạnh ngắn giới hạn) — stream dọc 720x1280
    // vừa giới hạn 1920x1080. <= 0: không giới hạn.
    bool         Open(int max_width = 0, int max_height = 0);
    void         Close();
    void         Flush();

    // oversize_dropped (có thể null) được cộng số frame đã decode nhưng vượt giới hạn — các
    // frame này bị bỏ, không copy ra.
    DecodeStatus Decode(const uint8_t* data, int size, int64_t pts_ext_ts,
                        uint64_t* oversize_dropped = nullptr);

    bool HasFrame() const;
    bool PopFrame(OwnedDecodedFrame& out);

private:
    void CopyFrameToOwned(AVFrame* f, int64_t pts_ext_ts, OwnedDecodedFrame& out);
    bool Exceeds(int width, int height) const;

    int max_long_  = 0; // 0 = không giới hạn
    int max_short_ = 0;

    const AVCodec*  codec_     = nullptr;
    AVCodecContext* codec_ctx_ = nullptr;
    AVFrame*        av_frame_  = nullptr;
    AVPacket*       av_packet_ = nullptr;
    bool            opened_    = false;

    std::deque<OwnedDecodedFrame> frame_queue_;
};
