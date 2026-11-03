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
};

class H264Decoder {
public:
    H264Decoder();
    ~H264Decoder();

    bool         Open();
    void         Close();
    void         Flush();

    DecodeStatus Decode(const uint8_t* data, int size, int64_t pts_ext_ts);

    bool HasFrame() const;
    bool PopFrame(OwnedDecodedFrame& out);

private:
    void CopyFrameToOwned(AVFrame* f, int64_t pts_ext_ts, OwnedDecodedFrame& out);

    const AVCodec*  codec_     = nullptr;
    AVCodecContext* codec_ctx_ = nullptr;
    AVFrame*        av_frame_  = nullptr;
    AVPacket*       av_packet_ = nullptr;
    bool            opened_    = false;

    std::deque<OwnedDecodedFrame> frame_queue_;
};
