#pragma once
#include <string>
#include "renderer.hpp"

namespace mf {
// HTTP/2 prior-knowledge cleartext receiver. Empty frames_root selects
// callback-only mode; otherwise receive AI results independently of realtime
// playout. /start anchors each source; /finalize seals input; /status reports
// asynchronous recording completion.
void serve_callbacks(int port, const std::string& call_id, const std::string& frames_root = "",
                     const std::string& background = "", const std::string& events = "",
                     const std::string& emoji_directory = "", bool require_masks = true, PlayoutConfig config = {});
} // namespace mf
