#include "callback_server.hpp"
#include "renderer.hpp"
#include <fstream>
#include <nlohmann/json.hpp>

#include <nghttp2/nghttp2.h>
#include <arpa/inet.h>
#include <fcntl.h>
#include <poll.h>
#include <sys/socket.h>
#include <unistd.h>

#include <algorithm>
#include <cerrno>
#include <csignal>
#include <cstring>
#include <cmath>
#include <set>
#include <iostream>
#include <map>
#include <memory>
#include <stdexcept>
#include <vector>

namespace mf {
namespace {
using json = nlohmann::json;
volatile std::sig_atomic_t stopped = 0;
void stop(int) { stopped = 1; }

// Match the current RTPGW HTTP result contract. The design's frame identity
// stays intact in the log and, in render mode, selects the original frame.
uint64_t integer(const json& value, uint64_t maximum, uint64_t minimum = 0) {
    if (!value.is_number_integer() || (!value.is_number_unsigned() && value.get<int64_t>() < 0))
        throw std::runtime_error("expected a nonnegative integer");
    const auto number = value.get<uint64_t>();
    if (number < minimum || number > maximum) throw std::runtime_error("integer out of range");
    return number;
}

void validate_mask(const json& mask) {
    integer(mask.at("frame_id"), UINT64_MAX, 1);
    integer(mask.at("rtp_timestamp"), UINT32_MAX);
    const auto width = integer(mask.at("mask_width"), 4096, 1);
    const auto height = integer(mask.at("mask_height"), 4096, 1);
    const auto& counts = mask.at("rle_counts");
    if (mask.at("status") != "ok" || !counts.is_array() || counts.empty())
        throw std::runtime_error("invalid mask status/RLE");
    uint64_t remaining = width * height;
    for (const auto& count : counts) {
        const auto length = integer(count, UINT32_MAX);
        if (length > remaining) throw std::runtime_error("RLE exceeds mask dimensions");
        remaining -= length;
    }
    if (remaining) throw std::runtime_error("RLE does not fill mask dimensions");
}

void validate_gesture(const json& g) {
    integer(g.at("frame_id"),UINT64_MAX,1);integer(g.at("rtp_timestamp"),UINT32_MAX);
    integer(g.at("timestamp_ms"),INT64_MAX);integer(g.at("extended_rtp_timestamp"),UINT64_MAX);
    integer(g.at("original_width"),16384,1);integer(g.at("original_height"),16384,1);
    for(const char* key:{"media_epoch","recognizer_epoch","response_id"})
        if(!g.at(key).is_string() || g.at(key).get<std::string>().empty()) throw std::runtime_error("missing gesture identity");
    if(g.at("status")!="ok" || !g.at("hands").is_array() || g.at("hands").size()>8) throw std::runtime_error("invalid gesture snapshot");
    std::set<uint64_t> tracks;
    for(const auto& hand:g.at("hands")) {
        if(!tracks.insert(integer(hand.at("track_id"),UINT64_MAX,1)).second) throw std::runtime_error("duplicate hand track");
        for(const auto& field:std::vector<std::pair<std::string,size_t>>{{"center",2},{"bbox",4}}) {
            const auto& values=hand.at(field.first);
            if(!values.is_array()||values.size()!=field.second) throw std::runtime_error("invalid hand coordinates");
            for(const auto& value:values) {
                if(!value.is_number()) throw std::runtime_error("invalid hand coordinate");
                const double n=value.get<double>();if(!std::isfinite(n)||n<0||n>1) throw std::runtime_error("hand coordinate out of bounds");
            }
        }
        if(hand.at("bbox")[0]>=hand.at("bbox")[2] || hand.at("bbox")[1]>=hand.at("bbox")[3]) throw std::runtime_error("invalid hand bbox");
        if(hand.contains("active_reaction") && !hand.at("active_reaction").is_null()) {
            const auto& a=hand.at("active_reaction");
            const auto start=integer(a.at("started_at_ms"),INT64_MAX),end=integer(a.at("valid_until_ms"),INT64_MAX);
            if(start>g.at("timestamp_ms").get<uint64_t>()||end<=g.at("timestamp_ms").get<uint64_t>()||a.at("event_id").get<std::string>().empty()) throw std::runtime_error("invalid reaction interval");
            const std::set<std::string> keys={"thumb_up","thumb_down","wave","victory","heart","point_up","fist"};
            if(!keys.count(a.at("emoji_key").get<std::string>())) throw std::runtime_error("unknown emoji key");
        }
    }
}

struct Service {
    std::string call_id, frames_root, background;
    std::string emoji_directory;
    bool require_masks=true;
    PlayoutConfig playout;
    std::ofstream events;
    std::map<std::string, std::unique_ptr<Renderer>> renderers;
    size_t received = 0, errors = 0;
    bool finalized = false;

    json status() const {
        json legs = json::object();
        bool done = finalized && renderers.size() == playout.legs;
        size_t failures = errors;
        for (const auto& item : renderers) {
            legs[item.first] = item.second->status();
            done = done && legs[item.first].at("finalized").get<bool>();
            if (!legs[item.first].at("error").get<std::string>().empty()) ++failures;
        }
        return {{"legs", legs}, {"received", received}, {"errors", failures}, {"finalized", done}, {"realtime", playout.gesture_render_mode != "source"}, {"gesture_render_mode", playout.gesture_render_mode}};
    }

    void emit(const json& event) {
        std::ostream& out = events.is_open() ? events : std::cout;
        out << event.dump() << std::endl;
        if (!out) throw std::runtime_error("cannot write MF event log");
    }
    std::pair<int, json> handle(const std::string& method, const std::string& path, const std::string& body) {
        if (method == "HEAD" && path == "/") return {200, json::object()};
        if (method == "GET" && path == "/status") return {200, status()};
        if (method == "POST" && (path == "/start/tCore" || path == "/start/tAccess")) {
            if (frames_root.empty()) return {409, {{"error", "source playout requires frames-root"}}};
            const std::string leg = path.substr(7), suffix = leg == "tCore" ? "tcore" : "taccess";
            try {
                if (!renderers.count(leg)) {
                    auto renderer = std::make_unique<Renderer>(std::filesystem::path(frames_root) / suffix,
                        background, emoji_directory, require_masks, playout);
                    renderer->start(); renderers.emplace(leg, std::move(renderer));
                }
                return {200, renderers.at(leg)->status()};
            } catch (const std::exception& e) { return {500, {{"error", e.what()}}}; }
        }
        if (method == "POST" && path == "/finalize") {
            if (frames_root.empty()) return {404, {{"error", "render mode is disabled"}}};
            try {
                if (renderers.size() != playout.legs || errors)
                    return {409, {{"error", "finalize requires " + std::to_string(playout.legs) +
                                            " rendered leg(s) and zero callback errors"}}};
                for (auto& item : renderers) item.second->finish();
                finalized = true;
                emit({{"event", "input_sealed"}, {"status", status()}});
                return {200, status()};
            } catch (const std::exception& error) {
                ++errors;
                emit({{"event", "error"}, {"detail", error.what()}});
                return {500, {{"error", error.what()}}};
            }
        }
        if (method != "POST") return {405, {{"error", "POST required"}}};
        std::string leg, suffix;
        if (path == "/results/tCore") { leg = "tCore"; suffix = "tcore"; }
        else if (path == "/results/tAccess") { leg = "tAccess"; suffix = "taccess"; }
        else return {404, {{"error", "unknown callback path"}}};
        json callback;
        try {
            callback = json::parse(body);
            // Dịch vụ 1 leg (--legs 1): gateway chỉ có session tcore; ctrl-result single-leg gửi
            // kết quả của nó về callback của leg kia (thường tAccess), kèm resource của cả hai leg.
            const std::string session_suffix = playout.legs == 1 ? "tcore" : suffix;
            const std::string session = call_id + "-" + session_suffix;
            const bool gesture=callback.value("event_type","")=="video.gesture.result" && callback.value("kind","")=="video_gesture";
            const bool mask=callback.value("event_type","")=="video.mask.result" && callback.value("kind","")=="video_mask";
            if ((!mask && !gesture) || callback.at("session_id")!=session ||
                (mask && (callback.at("is_final")!=true || callback.at("stream_id")!=session)) ||
                (gesture && (callback.at("stream_id")!=call_id+":"+session_suffix+":video" || callback.value("ai_service","")!="gesture_recognition")))
                throw std::runtime_error("invalid callback identity/event");
            const auto& resource = callback.at("mediaResources").at(leg);
            if (!resource.at("contextId").is_string() || resource.at("contextId").get<std::string>().empty() ||
                !resource.at("terminationId").is_string() || resource.at("terminationId").get<std::string>().empty())
                throw std::runtime_error("invalid callback media resource");
            if(gesture) validate_gesture(callback.at("gesture_result"));else validate_mask(callback.at("video_mask"));
        } catch (const std::exception& error) {
            ++errors;
            emit({{"event", "error"}, {"path", path}, {"detail", error.what()}});
            return {400, {{"error", error.what()}}};
        }
        if (!frames_root.empty()) {
            try {
                if (!renderers.count(leg)) return {409, {{"error", "source must start before callbacks"}}};
                callback["render"] = callback.at("kind")=="video_gesture" ? renderers.at(leg)->accept_gesture(callback.at("gesture_result")) : renderers.at(leg)->accept(callback.at("video_mask"));
            } catch (const CapacityError& error) {
                return {503, {{"error", error.what()}}};
            } catch (const std::exception& error) {
                ++errors;
                emit({{"event", "error"}, {"path", path}, {"detail", error.what()}});
                const bool bad_request = dynamic_cast<const std::invalid_argument*>(&error) != nullptr;
                return {bad_request ? 400 : 500, {{"error", error.what()}}};
            }
        }
        callback["event"] = "callback";
        callback["path"] = path;
        callback["protocol"] = "HTTP/2.0";
        emit(callback);
        ++received;
        return {200, callback.contains("render") ? callback.at("render") : json{{"ok", true}}};
    }
};

struct Request {
    std::string method, path, body, response;
    size_t sent = 0, header_bytes = 0;
    bool too_large = false, responded = false;
};

struct Connection {
    int fd;
    nghttp2_session* session = nullptr;
    Service& service;
    std::map<int32_t, Request> requests;
    Connection(int socket, Service& owner) : fd(socket), service(owner) {}
    ~Connection() { if (session) nghttp2_session_del(session); ::close(fd); }
};

ssize_t send_bytes(nghttp2_session*, const uint8_t* data, size_t size, int, void* user) {
    const auto& connection = *static_cast<Connection*>(user);
    ssize_t count;
    do { count = ::send(connection.fd, data, size, MSG_NOSIGNAL); } while (count < 0 && errno == EINTR);
    if (count < 0 && (errno == EAGAIN || errno == EWOULDBLOCK)) return NGHTTP2_ERR_WOULDBLOCK;
    return count < 0 ? static_cast<ssize_t>(NGHTTP2_ERR_CALLBACK_FAILURE) : count;
}

int begin_headers(nghttp2_session*, const nghttp2_frame* frame, void* user) {
    try {
        if (frame->hd.type == NGHTTP2_HEADERS && frame->headers.cat == NGHTTP2_HCAT_REQUEST) {
            auto& connection = *static_cast<Connection*>(user);
            if (connection.requests.size() >= 16) return NGHTTP2_ERR_CALLBACK_FAILURE;
            connection.requests.emplace(frame->hd.stream_id, Request{});
        }
        return 0;
    } catch (...) { return NGHTTP2_ERR_CALLBACK_FAILURE; }
}

int header(nghttp2_session*, const nghttp2_frame* frame, const uint8_t* name, size_t namelen,
           const uint8_t* value, size_t valuelen, uint8_t, void* user) {
    try {
        auto& requests = static_cast<Connection*>(user)->requests;
        auto found = requests.find(frame->hd.stream_id);
        if (found == requests.end()) return 0;
        auto& request = found->second;
        request.header_bytes += namelen + valuelen;
        if (request.header_bytes > 8192) return NGHTTP2_ERR_TEMPORAL_CALLBACK_FAILURE;
        const std::string key(reinterpret_cast<const char*>(name), namelen);
        if (key == ":path") request.path.assign(reinterpret_cast<const char*>(value), valuelen);
        if (key == ":method") request.method.assign(reinterpret_cast<const char*>(value), valuelen);
        return 0;
    } catch (...) { return NGHTTP2_ERR_CALLBACK_FAILURE; }
}

int data_chunk(nghttp2_session*, uint8_t, int32_t id, const uint8_t* data, size_t size, void* user) {
    try {
        auto& requests = static_cast<Connection*>(user)->requests;
        auto found = requests.find(id);
        if (found == requests.end()) return 0;
        auto& request = found->second;
        if (request.body.size() + size > 16 * 1024 * 1024) request.too_large = true;
        if (!request.too_large) request.body.append(reinterpret_cast<const char*>(data), size);
        return 0;
    } catch (...) { return NGHTTP2_ERR_CALLBACK_FAILURE; }
}

ssize_t response_data(nghttp2_session*, int32_t, uint8_t* buffer, size_t length,
                      uint32_t* flags, nghttp2_data_source* source, void*) {
    auto& request = *static_cast<Request*>(source->ptr);
    const size_t size = std::min(length, request.response.size() - request.sent);
    std::memcpy(buffer, request.response.data() + request.sent, size);
    request.sent += size;
    if (request.sent == request.response.size()) *flags |= NGHTTP2_DATA_FLAG_EOF;
    return static_cast<ssize_t>(size);
}

int frame_received(nghttp2_session* session, const nghttp2_frame* frame, void* user) {
    if ((frame->hd.type != NGHTTP2_DATA && frame->hd.type != NGHTTP2_HEADERS) ||
        !(frame->hd.flags & NGHTTP2_FLAG_END_STREAM)) return 0;
    try {
        auto& connection = *static_cast<Connection*>(user);
        auto found = connection.requests.find(frame->hd.stream_id);
        if (found == connection.requests.end() || found->second.responded) return 0;
        auto& request = found->second;
        request.responded = true;
        auto response = request.too_large
            ? std::make_pair(413, json{{"error", "callback body too large"}})
            : connection.service.handle(request.method, request.path, request.body);
        request.response = response.second.dump();
        const std::string status = std::to_string(response.first);
        auto nv = [](const char* name, const char* value) {
            return nghttp2_nv{reinterpret_cast<uint8_t*>(const_cast<char*>(name)),
                              reinterpret_cast<uint8_t*>(const_cast<char*>(value)),
                              std::strlen(name), std::strlen(value), NGHTTP2_NV_FLAG_NONE};
        };
        nghttp2_nv headers[] = {nv(":status", status.c_str()), nv("content-type", "application/json")};
        nghttp2_data_provider provider{};
        provider.source.ptr = &request;
        provider.read_callback = response_data;
        return nghttp2_submit_response(session, frame->hd.stream_id, headers, 2,
                                       request.method == "HEAD" ? nullptr : &provider);
    } catch (...) { return NGHTTP2_ERR_CALLBACK_FAILURE; }
}

int stream_closed(nghttp2_session*, int32_t id, uint32_t, void* user) {
    static_cast<Connection*>(user)->requests.erase(id);
    return 0;
}

void check(int result, const char* operation) {
    if (result < 0) throw std::runtime_error(std::string(operation) + ": " + nghttp2_strerror(result));
}
} // namespace

void serve_callbacks(int port, const std::string& call_id, const std::string& frames_root,
                     const std::string& background, const std::string& events,
                     const std::string& emoji_directory, bool require_masks, PlayoutConfig config) {
    Service service;
    service.call_id=call_id;service.frames_root=frames_root;service.background=background;
    service.emoji_directory=emoji_directory;service.require_masks=require_masks;
    service.playout=config;
    if (!events.empty()) {
        service.events.open(events);
        if (!service.events) throw std::runtime_error("cannot open MF event log");
    }
    const int listener = ::socket(AF_INET, SOCK_STREAM | SOCK_NONBLOCK | SOCK_CLOEXEC, 0);
    if (listener < 0) throw std::runtime_error("cannot create callback socket");
    // Use RAII for listener as well as accepted connections.
    Connection listener_guard(listener, service);
    sockaddr_in address{};
    address.sin_family = AF_INET;
    address.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    address.sin_port = htons(static_cast<uint16_t>(port));
    if (::bind(listener, reinterpret_cast<sockaddr*>(&address), sizeof(address)) < 0 || ::listen(listener, 32) < 0)
        throw std::runtime_error(std::string("cannot bind callback listener: ") + std::strerror(errno));
    socklen_t size = sizeof(address);
    if (::getsockname(listener, reinterpret_cast<sockaddr*>(&address), &size) < 0)
        throw std::runtime_error("cannot read callback port");
    nghttp2_session_callbacks* raw_callbacks = nullptr;
    check(nghttp2_session_callbacks_new(&raw_callbacks), "HTTP/2 callbacks");
    std::unique_ptr<nghttp2_session_callbacks, decltype(&nghttp2_session_callbacks_del)> callbacks(
        raw_callbacks, nghttp2_session_callbacks_del);
    nghttp2_session_callbacks_set_send_callback(callbacks.get(), send_bytes);
    nghttp2_session_callbacks_set_on_begin_headers_callback(callbacks.get(), begin_headers);
    nghttp2_session_callbacks_set_on_header_callback(callbacks.get(), header);
    nghttp2_session_callbacks_set_on_data_chunk_recv_callback(callbacks.get(), data_chunk);
    nghttp2_session_callbacks_set_on_frame_recv_callback(callbacks.get(), frame_received);
    nghttp2_session_callbacks_set_on_stream_close_callback(callbacks.get(), stream_closed);
    stopped = 0;
    std::signal(SIGTERM, stop);
    std::signal(SIGINT, stop);
    service.emit({{"event", "ready"}, {"port", ntohs(address.sin_port)}, {"protocol", "HTTP/2.0"}, {"mode", "cpp"}});
    std::vector<std::unique_ptr<Connection>> connections;
    while (!stopped) {
        std::vector<pollfd> polls{{listener, POLLIN, 0}};
        for (const auto& connection : connections) {
            const short events = POLLIN | (nghttp2_session_want_write(connection->session) ? POLLOUT : 0);
            polls.push_back({connection->fd, events, 0});
        }
        const int result = ::poll(polls.data(), polls.size(), 200);
        if (result < 0) {
            if (errno == EINTR) continue;
            throw std::runtime_error("callback poll failed");
        }
        // Process the snapshot before accepting additional connections.
        for (size_t i = connections.size(); i > 0; --i) {
            auto& connection = connections[i - 1];
            bool failed = polls[i].revents & (POLLERR | POLLNVAL | POLLHUP);
            if (!failed && polls[i].revents & POLLIN) {
                uint8_t data[65536];
                const auto count = ::recv(connection->fd, data, sizeof(data), 0);
                if (count > 0) {
                    const auto consumed = nghttp2_session_mem_recv(connection->session, data, static_cast<size_t>(count));
                    failed = consumed != count;
                } else failed = count == 0 || (errno != EAGAIN && errno != EWOULDBLOCK && errno != EINTR);
            }
            if (!failed) failed = nghttp2_session_send(connection->session) < 0;
            if (!failed) failed = !nghttp2_session_want_read(connection->session) && !nghttp2_session_want_write(connection->session);
            if (failed) connections.erase(connections.begin() + static_cast<std::ptrdiff_t>(i - 1));
        }
        if (polls[0].revents & POLLIN) {
            const int socket = ::accept4(listener, nullptr, nullptr, SOCK_NONBLOCK | SOCK_CLOEXEC);
            if (socket >= 0) {
                auto connection = std::make_unique<Connection>(socket, service);
                if (connections.size() >= 32) continue;
                check(nghttp2_session_server_new(&connection->session, callbacks.get(), connection.get()), "HTTP/2 session");
                const nghttp2_settings_entry settings[] = {{NGHTTP2_SETTINGS_MAX_CONCURRENT_STREAMS, 16}};
                check(nghttp2_submit_settings(connection->session, NGHTTP2_FLAG_NONE, settings, 1), "HTTP/2 settings");
                connections.push_back(std::move(connection));
            }
        }
    }
    service.emit({{"event", "summary"}, {"received", service.received}, {"errors", service.errors}});
}
} // namespace mf
