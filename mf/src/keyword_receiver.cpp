#include "keyword_receiver.hpp"

#include <stdexcept>
#include <vector>

namespace mf {
namespace {
std::string decode(const std::string& text) {
    std::string out;
    auto hex = [](char c) -> int {
        if (c >= '0' && c <= '9') return c - '0';
        if (c >= 'a' && c <= 'f') return c - 'a' + 10;
        if (c >= 'A' && c <= 'F') return c - 'A' + 10;
        return -1;
    };
    for (size_t i = 0; i < text.size(); ++i) {
        unsigned char c = text[i];
        if (c == '%') {
            if (i + 2 >= text.size() || hex(text[i+1]) < 0 || hex(text[i+2]) < 0)
                throw std::invalid_argument("invalid URL escape");
            c = static_cast<unsigned char>(hex(text[i+1]) * 16 + hex(text[i+2])); i += 2;
        }
        if (c < 32 || c == 127) throw std::invalid_argument("invalid resource character");
        out.push_back(static_cast<char>(c));
    }
    if (out.empty() || out == "." || out == "..") throw std::invalid_argument("empty/invalid resource ID");
    return out;
}
std::vector<std::string> segments(const std::string& path) {
    std::vector<std::string> parts;
    size_t start = 1;
    while (start <= path.size()) {
        const size_t end = path.find('/', start);
        parts.push_back(path.substr(start, end == std::string::npos ? end : end - start));
        if (end == std::string::npos) break;
        start = end + 1;
    }
    return parts;
}
std::string effect(const std::string& topic, const std::string& info) {
    if (topic != "EMOTION") return "unmapped";
    const std::map<std::string, std::string> effects = {
        {"congrat", "fireworks"}, {"approve", "thumbs_up"}, {"disapprove", "thumbs_down"},
        {"celebrate", "confetti"}, {"love", "hearts"}, {"determination", "sparks"},
        {"hello", "greeting"}, {"attention", "highlight"}
    };
    const auto it = effects.find(info);
    return it == effects.end() ? "unmapped" : it->second;
}
} // namespace

bool KeywordReceiver::matches(const std::string& path) {
    return path.rfind("/nmf-mrm/", 0) == 0;
}

std::pair<int, KeywordReceiver::json> KeywordReceiver::handle(
    const std::string& method, const std::string& path, const std::string& body,
    const std::string& key, const Emit& emit) {
    // Match URL structure before decoding resource IDs (an escaped slash is data).
    const auto parts = segments(path);
    if (parts.size() != 7 || parts[0] != "nmf-mrm" || parts[1] != "v2" || parts[2] != "contexts" ||
        parts[4] != "termination" || parts[6] != "keyword")
        return {404, {{"error", "unknown keyword path"}}};
    if (method != "POST") return {405, {{"error", "POST required"}}};
    json value;
    std::string context, termination;
    try {
        context = decode(parts[3]); termination = decode(parts[5]);
        if (body.size() > 4096 || key.size() > 256) throw std::invalid_argument("keyword request too large");
        value = json::parse(body);
        if (!value.is_object() || value.size() != 2) throw std::invalid_argument("expected topic_label and topic_info only");
        for (const char* field : {"topic_label", "topic_info"}) {
            const auto& item = value.at(field);
            if (!item.is_string()) throw std::invalid_argument("keyword fields must be strings");
            const auto text = item.get<std::string>();
            if (text.empty() || text.size() > 128 || text.find_first_not_of(" \t\r\n") == std::string::npos)
                throw std::invalid_argument("keyword fields must be nonempty and at most 128 bytes");
        }
    } catch (const std::exception& error) { return {400, {{"error", error.what()}}}; }

    const auto now = Clock::now();
    while (!order_.empty() && seen_.at(order_.front()).expires <= now) {
        seen_.erase(order_.front()); order_.pop_front();
    }
    // JSON encoding prevents delimiter collisions in opaque IDs and keys.
    const std::string identity = json::array({context, termination, key}).dump();
    if (!key.empty()) {
        const auto it = seen_.find(identity);
        if (it != seen_.end()) {
            if (it->second.body != value) return {409, {{"error", "conflicting idempotency key"}}};
            ++duplicates_;
            return {200, {{"ok", true}, {"duplicate", true}}};
        }
    }
    json event = value;
    event.update({{"event", "keyword_received"}, {"contextId", context}, {"terminationId", termination},
                  {"path", path}, {"protocol", "HTTP/2.0"}, {"idempotency_key", key},
                  {"effect", effect(value.at("topic_label"), value.at("topic_info"))},
                  {"render_mode", "mock"}, {"rendered", false}});
    // Persist the log before ACK and before marking the key as delivered.
    emit(event);
    if (!key.empty()) {
        if (seen_.size() >= 4096) { seen_.erase(order_.front()); order_.pop_front(); }
        seen_.emplace(identity, Entry{value, now + std::chrono::minutes(5)});
        order_.push_back(identity);
    }
    ++received_;
    return {200, {{"ok", true}, {"duplicate", false}}};
}
} // namespace mf
