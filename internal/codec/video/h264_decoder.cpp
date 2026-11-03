#include "h264_decoder.h"
#include <mutex>

extern "C" {
#include <libavutil/avutil.h>
}

H264Decoder::H264Decoder() = default;

H264Decoder::~H264Decoder() {
    Close();
}

bool H264Decoder::Open() {
    if (opened_) return true;

    #if LIBAVCODEC_VERSION_MAJOR < 58
        static std::once_flag register_once;
        std::call_once(register_once, [] {
            avcodec_register_all();
        });
    #endif

    codec_ = avcodec_find_decoder(AV_CODEC_ID_H264);
    if (!codec_) return false;

    codec_ctx_ = avcodec_alloc_context3(codec_);
    if (!codec_ctx_) return false;

    codec_ctx_->pkt_timebase = AVRational{1, 90000};

    if (avcodec_open2(codec_ctx_, codec_, nullptr) < 0) {
        avcodec_free_context(&codec_ctx_);
        return false;
    }

    av_packet_ = av_packet_alloc();
    av_frame_  = av_frame_alloc();
    if (!av_packet_ || !av_frame_) {
        Close();
        return false;
    }

    opened_ = true;
    return true;
}

void H264Decoder::Close() {
    av_frame_free(&av_frame_);
    av_packet_free(&av_packet_);
    if (codec_ctx_) avcodec_free_context(&codec_ctx_);
    codec_  = nullptr;
    opened_ = false;
    frame_queue_.clear();
}

void H264Decoder::Flush() {
    if (!opened_) return;
    avcodec_flush_buffers(codec_ctx_);
    frame_queue_.clear();
}

DecodeStatus H264Decoder::Decode(const uint8_t* data, int size, int64_t pts_ext_ts) {
    if (!opened_) return DecodeStatus::NotOpened;

    av_packet_unref(av_packet_);
    // buf is null after unref, so avcodec_send_packet will copy data internally
    av_packet_->data = const_cast<uint8_t*>(data);
    av_packet_->size = size;
    av_packet_->pts  = pts_ext_ts;
    av_packet_->dts  = AV_NOPTS_VALUE;

    int ret = avcodec_send_packet(codec_ctx_, av_packet_);
    if (ret < 0 && ret != AVERROR(EAGAIN)) return DecodeStatus::SendError;

    while (true) {
        ret = avcodec_receive_frame(codec_ctx_, av_frame_);
        if (ret == AVERROR(EAGAIN) || ret == AVERROR_EOF) break;
        if (ret < 0) return DecodeStatus::ReceiveError;

        OwnedDecodedFrame owned;
        CopyFrameToOwned(av_frame_, pts_ext_ts, owned);
        frame_queue_.push_back(std::move(owned));
        av_frame_unref(av_frame_);
    }

    return DecodeStatus::OK;
}

bool H264Decoder::HasFrame() const {
    return !frame_queue_.empty();
}

bool H264Decoder::PopFrame(OwnedDecodedFrame& out) {
    if (frame_queue_.empty()) return false;
    out = std::move(frame_queue_.front());
    frame_queue_.pop_front();
    return true;
}

void H264Decoder::CopyFrameToOwned(AVFrame* f, int64_t pts_ext_ts, OwnedDecodedFrame& out) {
    out.width  = f->width;
    out.height = f->height;

    // best_effort_timestamp for correct display order in streams with B-frames
    int64_t pts = f->best_effort_timestamp;
    if (pts == AV_NOPTS_VALUE || pts <= 0) pts = pts_ext_ts;
    out.av_pts = pts;

    AVPixelFormat fmt = static_cast<AVPixelFormat>(f->format);
    if (fmt == AV_PIX_FMT_YUV420P || fmt == AV_PIX_FMT_YUVJ420P) {
        out.pixel_format = VIDEO_PIX_FMT_YUV420P;
        out.stride0      = f->width;
        out.stride1      = f->width / 2;
        out.stride2      = f->width / 2;
    } else if (fmt == AV_PIX_FMT_NV12) {
        out.pixel_format = VIDEO_PIX_FMT_NV12;
        out.stride0      = f->width;
        out.stride1      = f->width;
        out.stride2      = 0;
    } else {
        out.pixel_format = VIDEO_PIX_FMT_UNKNOWN;
        out.stride0      = f->linesize[0];
        out.stride1      = f->linesize[1];
        out.stride2      = f->linesize[2];
    }

    int buf_size = av_image_get_buffer_size(fmt, f->width, f->height, 1);
    if (buf_size <= 0) return;

    out.buffer.resize(static_cast<size_t>(buf_size));
    av_image_copy_to_buffer(
        out.buffer.data(), buf_size,
        const_cast<const uint8_t**>(f->data), f->linesize,
        fmt, f->width, f->height, 1);

    out.data_size = buf_size;
}
