#pragma once
#include <cstdint>
#include <filesystem>
#include <vector>

namespace mf {
struct Image {
    int width = 0, height = 0, channels = 4;
    std::vector<uint8_t> pixels;
};
Image read_image(const std::filesystem::path& path);
void write_png(const std::filesystem::path& path, const Image& image);
Image resize_nearest(const Image& image, int width, int height);
void alpha_blend(Image& destination, const Image& sticker, int x, int y);
} // namespace mf
