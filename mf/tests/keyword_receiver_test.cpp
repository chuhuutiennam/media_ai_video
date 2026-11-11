#include "keyword_receiver.hpp"

#include <iostream>
#include <stdexcept>
#include <vector>

int main() {
    using json = nlohmann::json;
    try {
        mf::KeywordReceiver receiver;
        std::vector<json> events;
        const auto emit = [&](const json& event) { events.push_back(event); };
        const std::string path = "/nmf-mrm/v2/contexts/ctx-001/termination/t-1/keyword";
        const std::string body = R"({"topic_label":"EMOTION","topic_info":"congrat"})";
        auto check = [](bool ok) { if (!ok) throw std::runtime_error("keyword test failed"); };
        auto response = receiver.handle("POST", path, body, "event-1", emit);
        check(response.first == 200 && response.second.at("duplicate") == false);
        check(events.size() == 1 && events[0].at("effect") == "fireworks" && events[0].at("rendered") == false);
        check(events[0].at("contextId") == "ctx-001" && events[0].at("terminationId") == "t-1");
        check(receiver.handle("POST", path, body, "event-1", emit).second.at("duplicate") == true);
        check(events.size() == 1 && receiver.duplicates() == 1);
        const std::string other = R"({"topic_label":"WEATHER","topic_info":"rain"})";
        check(receiver.handle("POST", path, other, "event-1", emit).first == 409);
        check(receiver.handle("POST", path, other, "event-2", emit).first == 200);
        // Same key on another termination is independent; resource IDs are decoded.
        const std::string escaped = "/nmf-mrm/v2/contexts/ctx%2F002/termination/t%202/keyword";
        check(receiver.handle("POST", escaped, body, "event-1", emit).first == 200);
        check(events.back().at("contextId") == "ctx/002" && events.back().at("terminationId") == "t 2");
        check(receiver.handle("GET", path, body, "", emit).first == 405);
        check(receiver.handle("POST", path + "/extra", body, "", emit).first == 404);
        for (const std::string invalid : {"{", "null", "[]", "{}",
             R"({"topic_label":"EMOTION","topic_info":1})",
             R"({"topic_label":"EMOTION","topic_info":" "})",
             R"({"topic_label":"EMOTION","topic_info":"congrat","hands":[]})"})
            check(receiver.handle("POST", path, invalid, "", emit).first == 400);
        // An emitter failure must not consume the idempotency key.
        bool failed = false;
        try { receiver.handle("POST", path, body, "write-error", [](const json&) { throw std::runtime_error("disk error"); }); }
        catch (const std::exception&) { failed = true; }
        check(failed);
        check(receiver.handle("POST", path, body, "write-error", emit).second.at("duplicate") == false);
        check(receiver.received() == 4);
        std::cout << "keyword receiver tests passed\n";
    } catch (const std::exception& error) { std::cerr << error.what() << '\n'; return 1; }
}
