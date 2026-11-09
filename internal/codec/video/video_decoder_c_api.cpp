#include "video_decoder.h"
#include "video_stream_decoder.h"
#include <new>
#include <cstring>

static VideoDecoderConfig MakeConfig(const VideoDecoderConfig* c) {
    VideoDecoderConfig cfg{};
    cfg.max_nalu_size          = 2 * 1024 * 1024;
    cfg.max_access_unit_size   = 8 * 1024 * 1024;
    cfg.max_width              = 3840;
    cfg.max_height             = 2160;
    cfg.max_ready_frames       = 4;
    // A caller may override just the dimensions. Zero numeric fields retain
    // defaults; otherwise max_nalu_size=0 silently drops every NAL, and
    // max_ready_frames=0 attempts to pop an empty frame queue.
    if (c) {
        if (c->max_nalu_size > 0) cfg.max_nalu_size = c->max_nalu_size;
        if (c->max_access_unit_size > 0) cfg.max_access_unit_size = c->max_access_unit_size;
        if (c->max_width > 0) cfg.max_width = c->max_width;
        if (c->max_height > 0) cfg.max_height = c->max_height;
        if (c->max_ready_frames > 0) cfg.max_ready_frames = c->max_ready_frames;
    }
    return cfg;
}

extern "C" {

VideoDecoderHandle video_decoder_create(void) {
    return video_decoder_create_with_config(nullptr);
}

VideoDecoderHandle video_decoder_create_with_config(const VideoDecoderConfig* config) {
    try {
        VideoDecoderConfig cfg = MakeConfig(config);
        auto* dec = new (std::nothrow) VideoStreamDecoder(cfg);
        if (!dec) return nullptr;
        if (!dec->Open()) {
            delete dec;
            return nullptr;
        }
        return dec;
    } catch (...) {
        return nullptr;
    }
}

VideoStatus video_decoder_push_rtp(
    VideoDecoderHandle handle,
    uint16_t           sequence,
    uint32_t           timestamp,
    int                marker,
    const uint8_t*     payload,
    int                payload_size)
{
    if (!handle) return VIDEO_ERR_INVALID_ARGUMENT;
    try {
        return static_cast<VideoStreamDecoder*>(handle)->PushRtp(
            sequence, timestamp, marker != 0, payload, payload_size);
    } catch (const std::bad_alloc&) {
        return VIDEO_ERR_NO_MEMORY;
    } catch (...) {
        return VIDEO_ERR_DECODE;
    }
}

VideoStatus video_decoder_receive_frame(
    VideoDecoderHandle handle,
    DecodedVideoFrame* frame)
{
    if (!handle || !frame) return VIDEO_ERR_INVALID_ARGUMENT;
    try {
        return static_cast<VideoStreamDecoder*>(handle)->ReceiveFrame(*frame);
    } catch (const std::bad_alloc&) {
        return VIDEO_ERR_NO_MEMORY;
    } catch (...) {
        return VIDEO_ERR_DECODE;
    }
}

void video_decoder_release_frame(
    VideoDecoderHandle handle,
    DecodedVideoFrame* frame)
{
    if (!handle || !frame) return;
    try {
        static_cast<VideoStreamDecoder*>(handle)->ReleaseFrame(frame);
    } catch (...) {}
}

VideoStatus video_decoder_notify_packet_loss(
    VideoDecoderHandle handle,
    uint16_t           expected_seq,
    uint16_t           received_seq)
{
    if (!handle) return VIDEO_ERR_INVALID_ARGUMENT;
    try {
        static_cast<VideoStreamDecoder*>(handle)->NotifyPacketLoss(
            expected_seq, received_seq);
        return VIDEO_PACKET_GAP;
    } catch (...) {
        return VIDEO_ERR_DECODE;
    }
}

VideoStatus video_decoder_reset(VideoDecoderHandle handle) {
    if (!handle) return VIDEO_ERR_INVALID_ARGUMENT;
    try {
        static_cast<VideoStreamDecoder*>(handle)->Reset();
        return VIDEO_OK;
    } catch (...) {
        return VIDEO_ERR_DECODE;
    }
}

VideoStatus video_decoder_get_stats(
    VideoDecoderHandle handle,
    VideoDecoderStats* stats)
{
    if (!handle || !stats) return VIDEO_ERR_INVALID_ARGUMENT;
    try {
        *stats = static_cast<VideoStreamDecoder*>(handle)->GetStats();
        return VIDEO_OK;
    } catch (...) {
        return VIDEO_ERR_DECODE;
    }
}

void video_decoder_destroy(VideoDecoderHandle handle) {
    if (!handle) return;
    try {
        delete static_cast<VideoStreamDecoder*>(handle);
    } catch (...) {}
}

} // extern "C"
