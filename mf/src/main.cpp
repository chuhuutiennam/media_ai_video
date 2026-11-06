#include "callback_server.hpp"

#include <iostream>
#include <stdexcept>

int main(int argc, char** argv) {
    try {
        std::string call_id, port = "0", frames_root, background, events,emoji_directory;
        bool require_masks=true;
        mf::PlayoutConfig playout;
        for (int i = 1; i < argc; ++i) {
            const std::string option = argv[i];
            if (option == "--playout-delay-ms" || option == "--mask-max-age-ms" || option == "--gesture-max-age-ms" ||
                option == "--result-capacity" || option == "--recording-capacity") {
                if (++i == argc) throw std::runtime_error("missing value for " + option);
                size_t used = 0; const int n = std::stoi(argv[i], &used);
                if (used != std::string(argv[i]).size() || n < 0 || n > 60000 || (option != "--playout-delay-ms" && !n))
                    throw std::runtime_error("invalid value for " + option);
                if (option == "--playout-delay-ms") playout.delay_ms = n;
                else if (option == "--mask-max-age-ms") playout.mask_max_age_ms = n;
                else if (option == "--gesture-max-age-ms") playout.gesture_max_age_ms = n;
                else if (option == "--result-capacity") playout.result_capacity = n;
                else playout.recording_capacity = n;
                continue;
            }
            if (option == "--gesture-render-mode") {
                if (++i == argc) throw std::runtime_error("missing value for " + option);
                playout.gesture_render_mode = argv[i];
                if (playout.gesture_render_mode != "realtime" && playout.gesture_render_mode != "arrival" && playout.gesture_render_mode != "source")
                    throw std::runtime_error("invalid --gesture-render-mode (realtime|arrival|source)");
                continue;
            }
            if(option=="--gesture-only") { require_masks=false;continue; }
            if (option == "--legs") {
                if (++i == argc) throw std::runtime_error("missing value for " + option);
                const std::string legs = argv[i];
                if (legs != "1" && legs != "2") throw std::runtime_error("invalid --legs (1|2)");
                playout.legs = legs == "1" ? 1 : 2;
                continue;
            }
            if (option == "--help" || option == "-h") {
                std::cout << "Usage: mf_cpp --call-id ID [--port PORT]\n"
                             "       [--frames-root DIR --background IMAGE] [--events FILE]\n"
                             "       [--emoji-dir DIR] [--gesture-only] [--legs 1|2]\n"
                             "       [--playout-delay-ms 300 --mask-max-age-ms 500 --gesture-max-age-ms 500]\n"
                             "       [--gesture-render-mode realtime|arrival|source]\n"
                             "       [--result-capacity 4096 --recording-capacity 32]\n"
                             "POST /start/{tCore,tAccess} before RTP. ACK results without rendering.\n"
                             "--legs: legs that must render before /finalize (default 2; 1 for single-leg services:\n"
                             "        one gateway session <call>-tcore whose results arrive on the routed MF leg).\n"
                             "Realtime output frames are immutable and recorded once.\n"
                             "Features: raw-capture deferred-source-png gesture-render-modes\n";
                return 0;
            }
            std::string* value = nullptr;
            if (option == "--call-id") value = &call_id;
            else if (option == "--port") value = &port;
            else if (option == "--frames-root") value = &frames_root;
            else if (option == "--background") value = &background;
            else if (option == "--events") value = &events;
            else if (option == "--emoji-dir") value = &emoji_directory;
            else throw std::runtime_error("unknown option: " + option);
            if (++i == argc) throw std::runtime_error("missing value for " + option);
            *value = argv[i];
        }
        if (call_id.empty()) throw std::runtime_error("--call-id is required");
        size_t parsed = 0;
        const int number = std::stoi(port, &parsed);
        if (parsed != port.size() || number < 0 || number > 65535) throw std::runtime_error("invalid port");
        if (!background.empty() && frames_root.empty()) throw std::runtime_error("--background requires --frames-root");
        mf::serve_callbacks(number, call_id, frames_root, background, events,emoji_directory,require_masks,playout);
    } catch (const std::exception& error) {
        std::cerr << "mf mock: " << error.what() << '\n';
        return 1;
    }
    return 0;
}
