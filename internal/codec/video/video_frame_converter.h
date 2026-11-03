#pragma once
#include "owned_decoded_frame.h"

extern "C" {
#include <libswscale/swscale.h>
#include <libavutil/imgutils.h>
}

enum class ConvertStatus {
    OK,
    UnsupportedFormat,
    ScaleError,
    NoMemory,
};

class VideoFrameConverter {
public:
    VideoFrameConverter();
    ~VideoFrameConverter();

    ConvertStatus Convert(
        const OwnedDecodedFrame& src,
        int                      dst_width,
        int                      dst_height,
        int                      dst_pixel_format,
        OwnedDecodedFrame&       dst);

private:
    SwsContext* sws_ctx_     = nullptr;
    int         last_src_w_  = 0;
    int         last_src_h_  = 0;
    int         last_src_fmt_= 0;
    int         last_dst_w_  = 0;
    int         last_dst_h_  = 0;
    int         last_dst_fmt_= 0;
};
