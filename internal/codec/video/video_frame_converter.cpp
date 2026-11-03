#include "video_frame_converter.h"

VideoFrameConverter::VideoFrameConverter() = default;

VideoFrameConverter::~VideoFrameConverter() {
    if (sws_ctx_) sws_freeContext(sws_ctx_);
}

ConvertStatus VideoFrameConverter::Convert(
    const OwnedDecodedFrame& src,
    int dst_width, int dst_height, int dst_pixel_format,
    OwnedDecodedFrame& dst)
{
    if (src.width == dst_width &&
        src.height == dst_height &&
        src.pixel_format == dst_pixel_format) {
        dst = src;
        return ConvertStatus::OK;
    }

    // TODO: implement swscale conversion for phase 2
    // Map VideoPixelFormat -> AVPixelFormat, recreate sws_ctx_ when params change.
    return ConvertStatus::UnsupportedFormat;
}
