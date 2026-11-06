#pragma once
#include "image.hpp"
#include <chrono>
#include <condition_variable>
#include <deque>
#include <map>
#include <mutex>
#include <thread>
#include <nlohmann/json.hpp>

namespace mf {
struct PlayoutConfig {
    std::string gesture_render_mode = "realtime"; // realtime | arrival | source (test export)
    int delay_ms = 300;
    int mask_max_age_ms = 500;
    int gesture_max_age_ms = 500;
    int max_lateness_ms = 100;
    size_t result_capacity = 4096;
    size_t result_bytes = 64 * 1024 * 1024;
    size_t recording_capacity = 32;
    size_t recording_bytes = 64 * 1024 * 1024;
    size_t legs = 2; // số leg phải render trước khi /finalize (1 cho dịch vụ 1 leg, ví dụ change_background)
};
class CapacityError : public std::runtime_error {
public: using std::runtime_error::runtime_error;
};
// Per-leg clock. Callback acceptance never performs image I/O. Selected frames
// are immutable; the bounded recorder cannot revise decisions with late results.
class Renderer {
public:
    Renderer(const std::filesystem::path& directory, const std::string& background,
             const std::string& emoji_directory = "", bool require_masks = true, PlayoutConfig config = {});
    ~Renderer();
    void start();
    nlohmann::json accept(const nlohmann::json& body);
    nlohmann::json accept_gesture(const nlohmann::json& body);
    void finish();
    nlohmann::json status() const;
    nlohmann::json render_for_test(size_t index, int64_t deadline_ms);
private:
    using Clock = std::chrono::steady_clock;
    struct Result { nlohmann::json body; int64_t arrived_ms; };
    struct Recording { std::filesystem::path path; Image image; };
    int64_t elapsed_ms() const;
    nlohmann::json accept_result(const nlohmann::json&, bool gesture);
    nlohmann::json render(size_t, int64_t, Image&, Image&);
    void run();
    void record();
    void enqueue(Recording);
    void fail(const std::string&);
    void save_timeline();
    std::filesystem::path directory_;
    Image background_;
    std::filesystem::path emoji_directory_;
    bool require_masks_;
    PlayoutConfig config_;
    std::map<std::string, Image> stickers_;
    std::map<size_t, Result> gestures_;
    std::map<std::string,nlohmann::json> gesture_ids_;
    std::map<std::string, int64_t> reaction_arrivals_;
    nlohmann::json frames_, mapping_ = nlohmann::json::array();
    std::map<uint32_t, size_t> indices_;
    std::map<size_t, Result> masks_;
    std::map<uint64_t, nlohmann::json> mask_ids_;
    Image cached_mask_;
    uint64_t cached_mask_id_ = 0;
    mutable std::mutex mutex_;
    std::condition_variable wake_;
    Clock::time_point origin_;
    std::thread playout_, recorder_;
    std::deque<Recording> recordings_;
    size_t recording_bytes_ = 0, written_ = 0, rendered_ = 0, dropped_ = 0;
    size_t result_bytes_ = 0;
    bool started_ = false, sealed_ = false, stop_ = false, render_done_ = false, finalized_ = false;
    std::string error_;
};
} // namespace mf
