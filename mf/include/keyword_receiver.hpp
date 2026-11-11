#pragma once

#include <chrono>
#include <deque>
#include <functional>
#include <map>
#include <nlohmann/json.hpp>
#include <string>

namespace mf {
// Keyword mock logs effect selection. It does not need frames, masks or assets.
class KeywordReceiver {
public:
    using json = nlohmann::json;
    using Emit = std::function<void(const json&)>;
    static bool matches(const std::string& path);
    std::pair<int, json> handle(const std::string& method, const std::string& path,
                               const std::string& body, const std::string& key, const Emit& emit);
    size_t received() const { return received_; }
    size_t duplicates() const { return duplicates_; }
private:
    using Clock = std::chrono::steady_clock;
    struct Entry { json body; Clock::time_point expires; };
    std::map<std::string, Entry> seen_;
    std::deque<std::string> order_;
    size_t received_ = 0, duplicates_ = 0;
};
} // namespace mf
