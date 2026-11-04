#include "renderer.hpp"
#include <algorithm>
#include <cstdio>
#include <climits>
#include <memory>
#include <cmath>
#include <fstream>
#include <iomanip>
#include <optional>
#include <sstream>
#include <stdexcept>

namespace mf {
namespace {
using json = nlohmann::json;
void require(bool ok, const std::string& message) {
    if (!ok) throw std::invalid_argument(message);
}
uint64_t integer(const json& value, uint64_t maximum, uint64_t minimum = 0) {
    require(value.is_number_integer() && (value.is_number_unsigned() || value.get<int64_t>() >= 0), "invalid integer");
    const auto n = value.get<uint64_t>();
    require(n >= minimum && n <= maximum, "integer out of range");
    return n;
}
std::string filename(uint64_t n) {
    std::ostringstream name;
    name << std::setfill('0') << std::setw(6) << n << ".png";
    return name.str();
}
std::string reaction_key(const json& gesture, const json& hand) {
    return gesture.at("recognizer_epoch").get<std::string>() + "/" +
        std::to_string(hand.at("track_id").get<uint64_t>()) + "/" +
        hand.at("active_reaction").at("event_id").get<std::string>();
}
Image decode_mask(const json& mask) {
    const int w = integer(mask.at("mask_width"), 4096, 1), h = integer(mask.at("mask_height"), 4096, 1);
    const auto& counts = mask.at("rle_counts");
    require(mask.at("status") == "ok" && counts.is_array() && !counts.empty(), "invalid mask status/RLE");
    Image out{w, h, 1, std::vector<uint8_t>(static_cast<size_t>(w) * h)};
    size_t offset = 0;
    uint8_t value = 0;
    for (const auto& run : counts) {
        const auto count = integer(run, UINT32_MAX);
        require(count <= out.pixels.size() - offset, "RLE exceeds dimensions");
        std::fill_n(out.pixels.begin() + offset, count, value);
        offset += count;
        value = 255 - value;
    }
    require(offset == out.pixels.size(), "RLE does not fill dimensions");
    return out;
}
Image source_frame(const std::filesystem::path& directory, const json& frame, std::ifstream* raw = nullptr) {
    if (!frame.contains("rgba_offset")) return read_image(directory / frame.at("file").get<std::string>());
    Image image{frame.at("width"), frame.at("height"), 4, {}};
    image.pixels.resize(static_cast<size_t>(image.width) * image.height * 4);
    std::ifstream owned;
    if (!raw) { owned.open(directory / "frames.rgba", std::ios::binary); raw = &owned; }
    raw->clear();
    raw->seekg(frame.at("rgba_offset").get<int64_t>());
    if (!raw->read(reinterpret_cast<char*>(image.pixels.data()), image.pixels.size()))
        throw std::runtime_error("incomplete raw RGBA source spool");
    return image;
}
} // namespace

Renderer::Renderer(const std::filesystem::path& directory, const std::string& background,
                   const std::string& emoji_directory, bool require_masks, PlayoutConfig config)
    : directory_(directory), emoji_directory_(emoji_directory), require_masks_(require_masks), config_(config), origin_(Clock::now()) {
    std::ifstream input(directory / "frames.json");
    if (!input) throw std::runtime_error("source frames.json not ready: " + directory.string());
    frames_ = json::parse(input);
    require(frames_.is_array() && frames_.size() >= 2 && frames_.size() <= 18000, "need at least two source frames");
    const int width = integer(frames_[0].at("width"), 16384, 1), height = integer(frames_[0].at("height"), 16384, 1);
    require(static_cast<int64_t>(width) * height <= 64 * 1024 * 1024, "oversized source frame");
    require(config_.gesture_render_mode == "realtime" || config_.gesture_render_mode == "arrival" ||
            config_.gesture_render_mode == "source", "invalid gesture render mode");
    require(config_.delay_ms >= 0 && config_.mask_max_age_ms > 0 && config_.gesture_max_age_ms > 0 &&
            config_.result_capacity > 0 && config_.recording_capacity > 0, "invalid playout config");
    double previous = -1;
    for (size_t i = 0; i < frames_.size(); ++i) {
        const auto& f = frames_[i];
        const auto ts = integer(f.at("rtp_timestamp"), UINT32_MAX);
        require(indices_.emplace(ts, i).second, "duplicate source RTP timestamp");
        require(integer(f.at("width"), 16384, 1) == static_cast<uint64_t>(width) &&
                integer(f.at("height"), 16384, 1) == static_cast<uint64_t>(height), "source resolution changed");
        require(f.at("time_sec").is_number(), "invalid source time");
        const double time = f.at("time_sec").get<double>();
        require(std::isfinite(time) && time >= 0 && (!i || (time > previous && time - previous <= 60)), "invalid frame interval");
        require(i || time == 0, "source media timeline must start at zero");
        previous = time;
        require(f.contains("rgba_offset") == frames_[0].contains("rgba_offset"), "mixed source spool formats");
        if (f.contains("rgba_offset"))
            require(integer(f.at("rgba_offset"), INT64_MAX) == i * static_cast<uint64_t>(width) * height * 4,
                    "invalid source RGBA offset");
    }
    if (frames_[0].contains("rgba_offset"))
        require(std::filesystem::file_size(directory / "frames.rgba") == frames_.size() * static_cast<uint64_t>(width) * height * 4,
                "source RGBA spool size mismatch");
    background_ = {width, height, 4, std::vector<uint8_t>(static_cast<size_t>(width) * height * 4, 0)};
    for (size_t i = 3; i < background_.pixels.size(); i += 4) background_.pixels[i] = 255;
    if (!background.empty()) background_ = resize_nearest(read_image(background), width, height);
    std::filesystem::create_directories(directory / "output");
    std::filesystem::create_directories(directory / "masks");
}


Renderer::~Renderer() {
    { std::lock_guard<std::mutex> lock(mutex_); stop_ = true; }
    wake_.notify_all();
    if (playout_.joinable()) playout_.join();
    if (recorder_.joinable()) recorder_.join();
}
int64_t Renderer::elapsed_ms() const {
    return std::chrono::duration_cast<std::chrono::milliseconds>(Clock::now() - origin_).count();
}
void Renderer::start() {
    std::lock_guard<std::mutex> lock(mutex_);
    if (started_) return;
    origin_ = Clock::now(); started_ = true;
    recorder_ = std::thread(&Renderer::record, this);
    if (config_.gesture_render_mode != "source") playout_ = std::thread(&Renderer::run, this);
}
json Renderer::accept(const json& body) { return accept_result(body, false); }
json Renderer::accept_gesture(const json& body) { return accept_result(body, true); }
json Renderer::accept_result(const json& body, bool gesture) {
    const auto timestamp = integer(body.at("rtp_timestamp"), UINT32_MAX);
    require(indices_.count(timestamp), "callback timestamp has no source frame");
    const size_t index = indices_.at(timestamp);
    json value = body;
    if (!gesture) {
        value = json::object();
        for (const char* key : {"frame_id", "rtp_timestamp", "mask_width", "mask_height", "rle_counts", "status"}) value[key] = body.at(key);
    } else {
        require(body.at("original_width") == frames_[index].at("width") && body.at("original_height") == frames_[index].at("height"),
                "gesture source dimensions mismatch");
    }
    std::lock_guard<std::mutex> lock(mutex_);
    auto& results = gesture ? gestures_ : masks_;
    bool duplicate = false;
    if (gesture) {
        const std::string response = body.at("response_id");
        duplicate = gesture_ids_.count(response);
        require(!duplicate || gesture_ids_.at(response) == value, "conflicting gesture response ID");
    } else {
        const auto id = integer(value.at("frame_id"), UINT64_MAX, 1);
        duplicate = mask_ids_.count(id);
        require(!duplicate || mask_ids_.at(id) == value, "conflicting mask frame ID");
    }
    require(!results.count(index) || results.at(index).body == value, "conflicting source timestamp");
    require(!sealed_ || duplicate, "MF callback input sealed");
    if (!error_.empty()) throw std::runtime_error(error_);
    if (!duplicate) {
        // Include the JSON DOM's per-RLE-element cost and the idempotency copy.
        const size_t bytes = 2 * (value.dump().size() +
            (gesture ? value.at("hands").size() * 2048 : value.at("rle_counts").size() * sizeof(json)));
        if (masks_.size() + gestures_.size() >= config_.result_capacity || result_bytes_ + bytes > config_.result_bytes)
            throw CapacityError("MF result capacity exhausted");
        results.emplace(index, Result{value, 0});
        if (gesture) gesture_ids_.emplace(body.at("response_id").get<std::string>(), value);
        else mask_ids_.emplace(value.at("frame_id").get<uint64_t>(), value);
        result_bytes_ += bytes;
        // Timestamp completed admission, not the start of parsing/copying.
        // Round admission UP: a result at deadline + 0.2 ms must not be
        // reported as available at the integer deadline (flooring races the
        // renderer and makes a recorded decision impossible to audit).
        const auto ns = std::chrono::duration_cast<std::chrono::nanoseconds>(Clock::now() - origin_).count();
        results.at(index).arrived_ms = (ns + 999999) / 1000000;
        if (gesture) for (const auto& hand : value.at("hands")) {
            if (hand.contains("active_reaction") && !hand.at("active_reaction").is_null())
                reaction_arrivals_.emplace(reaction_key(value, hand), results.at(index).arrived_ms);
        }
    }
    const auto arrived = results.at(index).arrived_ms;
    const auto source_ms = std::llround(frames_[index].at("time_sec").get<double>() * 1000);
    return {{"duplicate", duplicate}, {"accepted_at_ms", arrived}, {"source_media_ms", source_ms},
            {"receive_lag_ms", arrived - source_ms},
            {"playout_media_at_receive_ms", std::max<int64_t>(0, arrived - config_.delay_ms)},
            {"gesture_render_mode", config_.gesture_render_mode},
            {"queued", true}, {"frames_written", written_}, {"realtime", config_.gesture_render_mode != "source"}};
}
json Renderer::render(size_t i, int64_t deadline, Image& source, Image& output) {
    json mask = nullptr, gesture = nullptr;
    int64_t gesture_arrived = 0;
    std::map<std::string, int64_t> event_arrivals;
    // Only data actually accepted before this frame's nominal deadline is eligible,
    // even if the compositor is scheduled late. Never select future media data.
    {
        std::lock_guard<std::mutex> lock(mutex_);
        auto select = [&](const auto& results) -> json {
            auto it = results.upper_bound(i);
            while (it != results.begin()) {
                --it;
                if (config_.gesture_render_mode == "source" || it->second.arrived_ms <= deadline) return it->second.body;
            }
            return nullptr;
        };
        mask = select(masks_); gesture = select(gestures_);
        if (!gesture.is_null()) {
            gesture_arrived = gestures_.at(indices_.at(gesture.at("rtp_timestamp").get<uint32_t>())).arrived_ms;
            for (const auto& hand : gesture.at("hands"))
                if (hand.contains("active_reaction") && !hand.at("active_reaction").is_null()) {
                    const auto key = reaction_key(gesture, hand);
                    event_arrivals[key] = reaction_arrivals_.at(key);
                }
        }
    }
    const auto& frame = frames_.at(i);
    uint64_t id = 0; bool exact = false;
    output = source; // Missing/stale segmentation falls back to source, not black.
    if (require_masks_ && !mask.is_null()) {
        const auto age = (frame.at("rtp_timestamp").get<uint32_t>() - mask.at("rtp_timestamp").get<uint32_t>()) / 90;
        if (age <= static_cast<uint32_t>(config_.mask_max_age_ms)) {
            id = mask.at("frame_id").get<uint64_t>();
            exact = mask.at("rtp_timestamp") == frame.at("rtp_timestamp");
            if (cached_mask_id_ != id) {
                cached_mask_ = resize_nearest(decode_mask(mask), background_.width, background_.height);
                cached_mask_id_ = id;
            }
            output = background_;
            for (size_t pixel = 0; pixel < cached_mask_.pixels.size(); ++pixel)
                if (cached_mask_.pixels[pixel]) std::copy_n(source.pixels.data() + pixel * 4, 4, output.pixels.data() + pixel * 4);
        }
    }
    json rendered_stickers=json::array();
    json trace = {{"mode", config_.gesture_render_mode}, {"decision", "not_available"}};
    const int64_t frame_media_ms = std::llround(frame.at("time_sec").get<double>() * 1000);
        if (!gesture.is_null()) {
            const uint32_t delta=frame.at("rtp_timestamp").get<uint32_t>()-gesture.at("rtp_timestamp").get<uint32_t>();
            const int64_t age=delta/90;
            const int64_t media_time=gesture.at("timestamp_ms").get<int64_t>()+age;
            const bool arrival_mode = config_.gesture_render_mode == "arrival";
            const bool fresh = arrival_mode ? deadline - gesture_arrived <= config_.gesture_max_age_ms : age <= config_.gesture_max_age_ms;
            const auto source_ms = std::llround(frames_[indices_.at(gesture.at("rtp_timestamp").get<uint32_t>())].at("time_sec").get<double>() * 1000);
            trace = {{"mode", config_.gesture_render_mode}, {"response_id", gesture.at("response_id")},
                {"source_frame_id", gesture.at("frame_id")}, {"source_media_ms", source_ms},
                {"accepted_at_ms", gesture_arrived}, {"receive_lag_ms", gesture_arrived - source_ms},
                {"display_lag_ms", frame_media_ms - source_ms}, {"snapshot_age_ms", age},
                {"decision", fresh ? "no_active_reaction" : "stale_snapshot"}};
            if(fresh) for(const auto& hand:gesture.at("hands")) {
                if(!hand.contains("active_reaction") || hand.at("active_reaction").is_null()) continue;
                const auto& reaction=hand.at("active_reaction");
                const int64_t start = reaction.at("started_at_ms").get<int64_t>();
                const int64_t end = reaction.at("valid_until_ms").get<int64_t>();
                const auto first_arrival = event_arrivals.at(reaction_key(gesture, hand));
                const bool active = arrival_mode ? (deadline >= first_arrival && deadline - first_arrival < end - start)
                    : (media_time >= start && media_time < end);
                if (!active) { trace["decision"] = "expired_reaction"; continue; }
                const std::string key=reaction.at("emoji_key");
                const std::vector<std::string> allowed={"thumb_up","thumb_down","victory","wave","heart","fist","point_up"};
                require(std::find(allowed.begin(),allowed.end(),key)!=allowed.end(),"unknown emoji key");
                require(!emoji_directory_.empty(),"emoji directory is required for gesture rendering");
                if(!stickers_.count(key)) stickers_[key]=read_image(emoji_directory_/(key+".png"));
                const int size=std::min(std::min(output.width,output.height),std::max(24,std::min(output.width,output.height)/5));
                const int cx=std::lround(hand.at("center")[0].get<double>()*(output.width-1));
                const int cy=std::lround(hand.at("center")[1].get<double>()*(output.height-1));
                const int x=std::clamp(cx-size/2,0,output.width-size);
                const int y=std::clamp(static_cast<int>(hand.at("bbox")[1].get<double>()*output.height)-size-4,0,output.height-size);
                alpha_blend(output,resize_nearest(stickers_.at(key),size,size),x,y);
                rendered_stickers.push_back({{"track_id",hand.at("track_id")},{"event_id",reaction.at("event_id")},
                    {"emoji_key",key},{"x",x},{"y",y},{"width",size},{"height",size},{"center_px",{cx,cy}}, {"source_started_at_ms", start}, {"source_valid_until_ms", end},
                    {"event_first_received_at_ms", first_arrival}});
            }

        }
    if (!rendered_stickers.empty()) trace["decision"] = "rendered";
    return {{"file", "output/" + filename(i + 1)}, {"rtp_timestamp", frame.at("rtp_timestamp")},
            {"time_sec", frame.at("time_sec")}, {"source_index", i}, {"mask_frame_id", id},
            {"exact_mask", exact}, {"stickers", rendered_stickers}, {"deadline_ms", deadline},
            {"gesture_response_id", gesture.is_null() ? "" : gesture.at("response_id").get<std::string>()},
            {"dropped", false}, {"mask_fallback", require_masks_ && !id}, {"gesture_trace", trace}};
}
void Renderer::enqueue(Recording item) {
    std::unique_lock<std::mutex> lock(mutex_);
    const auto bytes = item.image.pixels.size();
    // Backpressure is safe only in source export, after all callbacks drained.
    if (config_.gesture_render_mode == "source" && bytes <= config_.recording_bytes)
        wake_.wait(lock, [&] { return stop_ || (recordings_.size() < config_.recording_capacity && recording_bytes_ + bytes <= config_.recording_bytes); });
    if (stop_) return;
    if (recordings_.size() >= config_.recording_capacity || recording_bytes_ + bytes > config_.recording_bytes)
        throw CapacityError("MF recording capacity exhausted; recording cannot keep up with realtime");
    recording_bytes_ += bytes;
    recordings_.push_back(std::move(item)); wake_.notify_all();
}
void Renderer::fail(const std::string& error) {
    std::lock_guard<std::mutex> lock(mutex_);
    if (error_.empty()) error_ = error;
    stop_ = true; wake_.notify_all();
}
void Renderer::run() {
    try {
        // Keep the source handle open: opening a /mnt/c file every frame is
        // expensive even when the pixel pages themselves are cached.
        std::ifstream source_raw;
        if (frames_[0].contains("rgba_offset")) {
            source_raw.open(directory_ / "frames.rgba", std::ios::binary);
            if (!source_raw) throw std::runtime_error("cannot open source RGBA spool");
        }
        for (size_t i = 0; i < frames_.size(); ++i) {
            const auto deadline = config_.delay_ms + std::llround(frames_[i].at("time_sec").get<double>() * 1000);
            // Raw RGBA is prepared before RTP; PNG fallback supports old fixtures.
            // Production MF supplies decoded in-memory source frames here.
            Image source;
            if (config_.gesture_render_mode == "source" || mapping_.empty() || elapsed_ms() <= deadline + config_.max_lateness_ms)
                source = source_frame(directory_, frames_[i], source_raw.is_open() ? &source_raw : nullptr);
            {
                std::unique_lock<std::mutex> lock(mutex_);
                if (config_.gesture_render_mode != "source")
                    wake_.wait_until(lock, origin_ + std::chrono::milliseconds(deadline), [&] { return stop_; });
                if (stop_) return;
            }
            const auto lateness = config_.gesture_render_mode == "source" ? 0 : std::max<int64_t>(0, elapsed_ms() - deadline);
            json row;
            if (lateness > config_.max_lateness_ms && !mapping_.empty()) {
                // Repeat the already emitted picture. Never process an old frame
                // in an unbounded catch-up queue or let late results repair it.
                row = mapping_.back(); row["rtp_timestamp"] = frames_[i].at("rtp_timestamp");
                row["time_sec"] = frames_[i].at("time_sec"); row["deadline_ms"] = deadline;
                row["dropped"] = true;
                std::lock_guard<std::mutex> lock(mutex_); ++dropped_;
            } else {
                Image output;
                row = render(i, deadline, source, output);
                enqueue({directory_ / row.at("file").get<std::string>(), std::move(output)});
            }
            row["gesture_trace"]["output_media_ms"] = std::llround(frames_[i].at("time_sec").get<double>() * 1000);
            row["gesture_trace"]["repeated_frame"] = row.at("dropped");
            if (row["gesture_trace"].contains("source_media_ms"))
                row["gesture_trace"]["display_lag_ms"] = row["gesture_trace"]["output_media_ms"].get<int64_t>() - row["gesture_trace"]["source_media_ms"].get<int64_t>();
            row["render_lateness_ms"] = lateness;
            mapping_.push_back(std::move(row));
            { std::lock_guard<std::mutex> lock(mutex_); ++rendered_; }
        }
        { std::lock_guard<std::mutex> lock(mutex_); render_done_ = true; }
        wake_.notify_all();
    } catch (const std::exception& e) { fail(e.what()); }
}
void Renderer::record() {
    try {
        // Realtime capture must not compete with RTP/AI for PNG compression or
        // one-file-per-frame filesystem metadata. Spool immutable RGBA frames
        // sequentially; encode debug PNGs only after input has drained/sealed.
        // System temporary storage isolates hot capture writes from a Windows
        // artifact directory and its logging/metadata I/O. tmpfile is private
        // and automatically removed on both success and failure.
        const auto close_file = [](std::FILE* file) { if (file) std::fclose(file); };
        std::unique_ptr<std::FILE, decltype(close_file)> spool(std::tmpfile(), close_file);
        if (!spool) throw std::runtime_error("cannot open temporary recording spool");
        std::vector<std::pair<std::filesystem::path, uint64_t>> records;
        uint64_t offset = 0;
        while (true) {
            Recording item;
            {
                std::unique_lock<std::mutex> lock(mutex_);
                wake_.wait(lock, [&] { return stop_ || !recordings_.empty() || (render_done_ && sealed_); });
                if (stop_) return;
                if (recordings_.empty()) break;
                item = std::move(recordings_.front()); recordings_.pop_front();
                recording_bytes_ -= item.image.pixels.size();
                wake_.notify_all();
            }
            if (std::fwrite(item.image.pixels.data(), 1, item.image.pixels.size(), spool.get()) != item.image.pixels.size())
                throw std::runtime_error("cannot write recording spool");
            records.emplace_back(item.path, offset);
            offset += item.image.pixels.size();
            { std::lock_guard<std::mutex> lock(mutex_); ++written_; }
        }
        if (std::fflush(spool.get()) != 0) throw std::runtime_error("cannot flush recording spool");
        Image image{background_.width, background_.height, 4,
                    std::vector<uint8_t>(static_cast<size_t>(background_.width) * background_.height * 4)};
        for (const auto& item : records) {
            if (item.second > LONG_MAX || std::fseek(spool.get(), static_cast<long>(item.second), SEEK_SET) != 0 ||
                std::fread(image.pixels.data(), 1, image.pixels.size(), spool.get()) != image.pixels.size())
                throw std::runtime_error("incomplete recording spool");
            write_png(item.first, image);
        }
        // Source PNGs are also debug artifacts. The sender may defer them so
        // startup and concurrent RTP never pay for per-frame PNG encoding.
        if (frames_[0].contains("rgba_offset")) {
            for (const auto& frame : frames_)
                write_png(directory_ / frame.at("file").get<std::string>(), source_frame(directory_, frame));
        }
        // Debug masks are written after playout, never by the callback handler.
        for (const auto& item : masks_) {
            const auto& mask = item.second.body;
            write_png(directory_ / "masks" / filename(mask.at("frame_id")), decode_mask(mask));
        }
        save_timeline();
        { std::lock_guard<std::mutex> lock(mutex_); finalized_ = true; }
    } catch (const std::exception& e) { fail(e.what()); }
}
void Renderer::finish() {
    std::lock_guard<std::mutex> lock(mutex_);
    require(started_, "source playout not started");
    sealed_ = true;
    if (config_.gesture_render_mode == "source" && !playout_.joinable())
        playout_ = std::thread(&Renderer::run, this);
    wake_.notify_all();
}
json Renderer::status() const {
    std::lock_guard<std::mutex> lock(mutex_);
    return {{"frames", frames_.size()}, {"frames_written", written_}, {"frames_rendered", rendered_},
        {"frames_dropped", dropped_}, {"masks", masks_.size()}, {"gestures", gestures_.size()},
        {"finalized", finalized_}, {"sealed", sealed_}, {"error", error_}, {"realtime", config_.gesture_render_mode != "source"}, {"gesture_render_mode", config_.gesture_render_mode},
        {"recording_queue", recordings_.size()}, {"recording_bytes", recording_bytes_}, {"result_bytes", result_bytes_},
        {"playout_delay_ms", config_.delay_ms}, {"mask_max_age_ms", config_.mask_max_age_ms},
        {"gesture_max_age_ms", config_.gesture_max_age_ms}};
}
json Renderer::render_for_test(size_t i, int64_t deadline) {
    auto source = source_frame(directory_, frames_[i]);
    Image output; auto row = render(i, deadline, source, output);
    write_png(directory_ / row.at("file").get<std::string>(), output);
    return row;
}
void Renderer::save_timeline() {
    std::ofstream trace(directory_ / "gesture-trace.jsonl");
    for (const auto& item : gestures_) {
        const auto source_ms = std::llround(frames_[item.first].at("time_sec").get<double>() * 1000);
        json row = {{"event", "gesture_callback"}, {"mode", config_.gesture_render_mode},
            {"response_id", item.second.body.at("response_id")}, {"source_frame_id", item.second.body.at("frame_id")},
            {"source_media_ms", source_ms}, {"accepted_at_ms", item.second.arrived_ms},
            {"receive_lag_ms", item.second.arrived_ms - source_ms},
            {"playout_media_at_receive_ms", std::max<int64_t>(0, item.second.arrived_ms - config_.delay_ms)},
            {"received_after_playout", item.second.arrived_ms > config_.delay_ms + std::llround(frames_.back().at("time_sec").get<double>() * 1000)}};
        trace << row.dump() << '\n';
    }
    for (const auto& frame : mapping_) {
        auto row = frame.at("gesture_trace");
        row["event"] = "gesture_frame"; row["output_media_ms"] = std::llround(frame.at("time_sec").get<double>() * 1000);
        row["deadline_ms"] = frame.at("deadline_ms"); row["stickers"] = frame.at("stickers");
        trace << row.dump() << '\n';
    }
    trace.close();
    if (!trace) throw std::runtime_error("cannot write gesture trace");

    std::ofstream concat(directory_ / "output.ffconcat.tmp");
    concat << "ffconcat version 1.0\n" << std::fixed << std::setprecision(9);
    for (size_t i = 0; i < mapping_.size(); ++i) {
        const double time = frames_[i].at("time_sec").get<double>();
        const double next = i + 1 < frames_.size() ? frames_[i + 1].at("time_sec").get<double>()
                                                 : 2 * time - frames_[i - 1].at("time_sec").get<double>();
        // Concat parses durations at microsecond precision. Quantize absolute
        // endpoints so 30 FPS does not accumulate a 1/3 us error per frame.
        const double duration = (std::llround(next * 1000000) - std::llround(time * 1000000)) / 1000000.0;
        concat << "file 'output/" << mapping_[i].at("file").get<std::string>().substr(7)
               << "'\noption framerate 90000\nduration " << duration << '\n';
    }
    if (!mapping_.empty()) concat << "file 'output/" << mapping_[mapping_.size() - 1].at("file").get<std::string>().substr(7)
                                 << "'\noption framerate 90000\n";
    concat.close();
    if (!concat) throw std::runtime_error("cannot write concat timeline");
    std::filesystem::rename(directory_ / "output.ffconcat.tmp", directory_ / "output.ffconcat");
    std::ofstream index(directory_ / "render.json.tmp");
    index << mapping_.dump(2) << '\n';
    index.close();
    if (!index) throw std::runtime_error("cannot write render index");
    std::filesystem::rename(directory_ / "render.json.tmp", directory_ / "render.json");
}

} // namespace mf
