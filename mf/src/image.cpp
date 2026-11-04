#include "image.hpp"
#include <algorithm>
#include <fstream>
#include <memory>
#include <stdexcept>
extern "C" {
#include <libavcodec/avcodec.h>
#include <libavutil/error.h>
#include <libswscale/swscale.h>
}

namespace mf {
void alpha_blend(Image& dst, const Image& src, int left, int top) {
    if (dst.channels != 4 || src.channels != 4) throw std::invalid_argument("alpha blend requires RGBA");
    for (int y=0;y<src.height;++y) for(int x=0;x<src.width;++x) {
        const int dx=left+x,dy=top+y;
        if(dx<0||dy<0||dx>=dst.width||dy>=dst.height) continue;
        const auto* s=src.pixels.data()+(static_cast<size_t>(y)*src.width+x)*4;
        auto* d=dst.pixels.data()+(static_cast<size_t>(dy)*dst.width+dx)*4;
        const unsigned alpha=s[3];
        for(int c=0;c<3;++c) d[c]=static_cast<uint8_t>((s[c]*alpha+d[c]*(255-alpha)+127)/255);
        d[3]=static_cast<uint8_t>(alpha+(d[3]*(255-alpha)+127)/255);
    }
}
namespace {
struct CodecDelete { void operator()(AVCodecContext* p) const { avcodec_free_context(&p); } };
struct FrameDelete { void operator()(AVFrame* p) const { av_frame_free(&p); } };
struct PacketDelete { void operator()(AVPacket* p) const { av_packet_free(&p); } };
using Codec = std::unique_ptr<AVCodecContext, CodecDelete>;
using Frame = std::unique_ptr<AVFrame, FrameDelete>;
using Packet = std::unique_ptr<AVPacket, PacketDelete>;
void check(int code, const std::string& operation) {
    if (code >= 0) return;
    char message[AV_ERROR_MAX_STRING_SIZE];
    av_strerror(code, message, sizeof(message));
    throw std::runtime_error(operation + ": " + message);
}
void check_size(int w, int h) {
    if (w <= 0 || h <= 0 || w > 16384 || h > 16384 || static_cast<int64_t>(w) * h > 64 * 1024 * 1024)
        throw std::runtime_error("invalid/oversized image");
}
} // namespace

Image read_image(const std::filesystem::path& path) {
    std::ifstream input(path, std::ios::binary | std::ios::ate);
    if (!input) throw std::runtime_error("cannot read image: " + path.string());
    const auto size = input.tellg();
    if (size < 8 || size > 512 * 1024 * 1024) throw std::runtime_error("invalid image file size");
    Packet packet(av_packet_alloc());
    Frame frame(av_frame_alloc());
    if (!packet || !frame) throw std::bad_alloc();
    check(av_new_packet(packet.get(), static_cast<int>(size)), "allocate image packet");
    input.seekg(0);
    if (!input.read(reinterpret_cast<char*>(packet->data), size)) throw std::runtime_error("cannot read image bytes");
    const uint8_t signature[] = {137, 80, 78, 71, 13, 10, 26, 10};
    AVCodecID id;
    if (std::equal(std::begin(signature), std::end(signature), packet->data)) id = AV_CODEC_ID_PNG;
    else if (packet->data[0] == 255 && packet->data[1] == 216) id = AV_CODEC_ID_MJPEG;
    else throw std::runtime_error("image must be PNG or JPEG");
    const auto* decoder = avcodec_find_decoder(id);
    if (!decoder) throw std::runtime_error("FFmpeg image decoder unavailable");
    Codec codec(avcodec_alloc_context3(decoder));
    if (!codec) throw std::bad_alloc();
    codec->thread_count = 1;
    codec->max_pixels = 64 * 1024 * 1024;
    check(avcodec_open2(codec.get(), decoder, nullptr), "open image decoder");
    check(avcodec_send_packet(codec.get(), packet.get()), "decode image");
    check(avcodec_receive_frame(codec.get(), frame.get()), "receive decoded image");
    check_size(frame->width, frame->height);
    Image out{frame->width, frame->height, 4,
              std::vector<uint8_t>(static_cast<size_t>(frame->width) * frame->height * 4)};
    // Go source PNGs are RGB16. Preserve the high byte exactly as Pillow does.
    if (frame->format == AV_PIX_FMT_RGB48BE || frame->format == AV_PIX_FMT_RGB48LE ||
        frame->format == AV_PIX_FMT_RGBA64BE || frame->format == AV_PIX_FMT_RGBA64LE) {
        const int channels = (frame->format == AV_PIX_FMT_RGBA64BE || frame->format == AV_PIX_FMT_RGBA64LE) ? 4 : 3;
        const int low = frame->format == AV_PIX_FMT_RGB48LE || frame->format == AV_PIX_FMT_RGBA64LE;
        for (int y = 0; y < out.height; ++y) {
            const auto* row = frame->data[0] + y * frame->linesize[0];
            for (int x = 0; x < out.width; ++x) {
                auto* pixel = out.pixels.data() + (static_cast<size_t>(y) * out.width + x) * 4;
                pixel[3] = 255;
                for (int c = 0; c < channels; ++c) pixel[c] = row[(x * channels + c) * 2 + low];
            }
        }
    } else {
        std::unique_ptr<SwsContext, decltype(&sws_freeContext)> scaler(
            sws_getContext(out.width, out.height, static_cast<AVPixelFormat>(frame->format),
                           out.width, out.height, AV_PIX_FMT_RGBA, SWS_POINT, nullptr, nullptr, nullptr), sws_freeContext);
        if (!scaler) throw std::runtime_error("cannot create FFmpeg converter");
        uint8_t* data[] = {out.pixels.data(), nullptr, nullptr, nullptr};
        const int strides[] = {out.width * 4, 0, 0, 0};
        if (sws_scale(scaler.get(), frame->data, frame->linesize, 0, out.height, data, strides) != out.height)
            throw std::runtime_error("incomplete image conversion");
    }
    return out;
}

void write_png(const std::filesystem::path& path, const Image& image) {
    check_size(image.width, image.height);
    if ((image.channels != 1 && image.channels != 4) ||
        image.pixels.size() != static_cast<size_t>(image.width) * image.height * image.channels)
        throw std::runtime_error("invalid pixel buffer");
    const auto* encoder = avcodec_find_encoder(AV_CODEC_ID_PNG);
    if (!encoder) throw std::runtime_error("FFmpeg PNG encoder unavailable");
    Codec codec(avcodec_alloc_context3(encoder));
    Frame frame(av_frame_alloc());
    Packet packet(av_packet_alloc());
    if (!codec || !frame || !packet) throw std::bad_alloc();
    codec->width = image.width;
    codec->height = image.height;
    codec->pix_fmt = image.channels == 1 ? AV_PIX_FMT_GRAY8 : AV_PIX_FMT_RGBA;
    codec->time_base = AVRational{1, 1};
    codec->thread_count = 1;
    // Lossless recording spool; avoid deflate spikes on photographic frames.
    codec->compression_level = 0;
    check(avcodec_open2(codec.get(), encoder, nullptr), "open PNG encoder");
    frame->format = codec->pix_fmt;
    frame->width = image.width;
    frame->height = image.height;
    frame->pts = 0;
    check(av_frame_get_buffer(frame.get(), 32), "allocate PNG frame");
    const size_t stride = static_cast<size_t>(image.width) * image.channels;
    for (int y = 0; y < image.height; ++y)
        std::copy_n(image.pixels.data() + y * stride, stride, frame->data[0] + y * frame->linesize[0]);
    check(avcodec_send_frame(codec.get(), frame.get()), "encode PNG");
    check(avcodec_receive_packet(codec.get(), packet.get()), "receive PNG");
    const auto temporary = path.string() + ".tmp";
    std::ofstream output(temporary, std::ios::binary);
    output.write(reinterpret_cast<const char*>(packet->data), packet->size);
    output.close();
    if (!output) throw std::runtime_error("cannot write PNG: " + path.string());
    std::filesystem::rename(temporary, path);
}

Image resize_nearest(const Image& image, int width, int height) {
    check_size(width, height);
    Image out{width, height, image.channels,
              std::vector<uint8_t>(static_cast<size_t>(width) * height * image.channels)};
    for (int y = 0; y < height; ++y) {
        const int sy = static_cast<int>(static_cast<int64_t>(y) * image.height / height);
        for (int x = 0; x < width; ++x) {
            const int sx = static_cast<int>(static_cast<int64_t>(x) * image.width / width);
            std::copy_n(image.pixels.data() + (static_cast<size_t>(sy) * image.width + sx) * image.channels,
                        image.channels, out.pixels.data() + (static_cast<size_t>(y) * width + x) * image.channels);
        }
    }
    return out;
}
} // namespace mf
