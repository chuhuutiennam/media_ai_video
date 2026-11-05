#include "renderer.hpp"
#include <fstream>
#include <iostream>
#include <stdexcept>
#include <unistd.h>

using nlohmann::json;
void check(bool ok,const char* message){if(!ok)throw std::runtime_error(message);}
int main(){
    const auto root=std::filesystem::temp_directory_path()/("mf-gesture-test-"+std::to_string(getpid()));
    try {
        std::filesystem::create_directories(root/"source");
        std::filesystem::create_directories(root/"emoji");
        mf::Image source{32,32,4,std::vector<uint8_t>(32*32*4,50)};
        for(size_t i=3;i<source.pixels.size();i+=4)source.pixels[i]=255;
        json frames=json::array();
        const std::vector<int> times={0,100,700};
        for(int i=0;i<3;++i){
            const auto file="source/"+std::to_string(i)+".png";
            mf::write_png(root/file,source);
            frames.push_back({{"file",file},{"rtp_timestamp",times[i]*90},{"time_sec",times[i]/1000.0},{"width",32},{"height",32}});
        }
        std::ofstream(root/"frames.json")<<frames;
        mf::Image asset{8,8,4,std::vector<uint8_t>(8*8*4,0)};
        for(size_t i=0;i<asset.pixels.size();i+=4){asset.pixels[i]=255;asset.pixels[i+3]=128;}
        mf::write_png(root/"emoji/thumb_up.png",asset);
        auto mask=[](int id,int ts){return json{{"frame_id",id},{"rtp_timestamp",ts},{"mask_width",1},{"mask_height",1},{"rle_counts",{0,1}},{"status","ok"}};};
        json g={{"response_id","r1"},{"media_epoch","source1"},{"recognizer_epoch","task1"},{"frame_id",2},{"rtp_timestamp",9000},
            {"timestamp_ms",100},{"original_width",32},{"original_height",32},{"status","ok"},
            {"hands",json::array({{{"track_id",1},{"center",{0.5,0.8}},{"bbox",{0.2,0.7,0.8,0.9}},
                {"active_reaction",{{"event_id","e1"},{"emoji_key","thumb_up"},{"started_at_ms",100},{"valid_until_ms",400}}}}})}};
        {
            mf::Renderer renderer(root,"",(root/"emoji").string());
            renderer.accept(mask(3,63000));
            renderer.accept_gesture(g);
            renderer.accept(mask(1,0));
            check(renderer.accept_gesture(g).at("duplicate"),"retry was not idempotent");
            auto conflict=g;conflict["hands"][0]["center"][0]=0.2;
            bool rejected=false;try{renderer.accept_gesture(conflict);}catch(const std::invalid_argument&){rejected=true;}
            check(rejected,"conflicting response was accepted");
            auto before=renderer.render_for_test(0,100);
            auto during=renderer.render_for_test(1,200);
            auto after=renderer.render_for_test(2,800);
            check(before["stickers"].empty(),"future gesture used for earlier frame");
            check(during["stickers"].size()==1,"sticker missing");
            check(after["stickers"].empty(),"expired gesture remained visible");
            auto output=mf::read_image(root/"output/000002.png");
            const size_t pixel=(5*32+5)*4;
            check(output.pixels[pixel]==153 && output.pixels[pixel+1]==25,"wrong RGBA blend");
        }
        {
            mf::Renderer renderer(root,"",(root/"emoji").string());
            std::this_thread::sleep_for(std::chrono::milliseconds(10));
            renderer.accept(mask(1,0));renderer.accept_gesture(g);
            auto late=renderer.render_for_test(1,0);
            check(late["mask_frame_id"]==0 && late["stickers"].empty(),"post-deadline result leaked into output");
            auto image=mf::read_image(root/"output/000002.png");
            check(image.pixels[0]==50,"missing mask must fall back to source");
            // Acceptance must not rewrite any already-emitted frame.
            auto clear=g;clear["response_id"]="r2";clear["rtp_timestamp"]=63000;clear["frame_id"]=3;clear["hands"]=json::array();
            renderer.accept_gesture(clear);
            check(mf::read_image(root/"output/000002.png").pixels==image.pixels,"late callback rewrote output");
            check(renderer.render_for_test(2,1000)["mask_frame_id"]==0,"stale mask reused");
        }
        {
            // A late reaction already expired in source time must still be
            // visible on the first eligible current frame in arrival mode.
            mf::PlayoutConfig cfg;cfg.gesture_render_mode="arrival";
            mf::Renderer measured(root,"",(root/"emoji").string(),false,cfg);
            std::this_thread::sleep_for(std::chrono::milliseconds(650));
            const auto accepted=measured.accept_gesture(g);
            const auto arrived=accepted.at("accepted_at_ms").get<int64_t>();
            check(accepted.at("receive_lag_ms").get<int64_t>()>=550,"receive lag lost source time");
            check(measured.render_for_test(2,arrived-1)["stickers"].empty(),"arrival mode used an unreceived callback");
            const auto shown=measured.render_for_test(2,arrived+1);
            check(shown["stickers"].size()==1,"late arrival was not displayed on current frame");
            check(shown["gesture_trace"]["display_lag_ms"]==600,"wrong source/display trace");
            check(shown["gesture_trace"]["decision"]=="rendered","wrong arrival decision trace");
            check(measured.accept_gesture(g)["accepted_at_ms"]==arrived,"retry changed arrival time");
            check(measured.render_for_test(2,arrived+300)["stickers"].empty(),"arrival reaction did not expire");
            auto clear=g;clear["response_id"]="clear";clear["frame_id"]=3;clear["rtp_timestamp"]=63000;
            clear["timestamp_ms"]=700;clear["hands"]=json::array();
            const auto cleared=measured.accept_gesture(clear).at("accepted_at_ms").get<int64_t>();
            check(measured.render_for_test(2,cleared+1)["stickers"].empty(),"clear-hands snapshot left a sticker");
        }
        {
            mf::PlayoutConfig cfg;cfg.gesture_render_mode="arrival";cfg.gesture_max_age_ms=60000;
            mf::Renderer repeated(root,"",(root/"emoji").string(),false,cfg);
            auto held=g;held["hands"][0]["active_reaction"]["valid_until_ms"]=1600;
            const auto first=repeated.accept_gesture(held).at("accepted_at_ms").get<int64_t>();
            std::this_thread::sleep_for(std::chrono::milliseconds(5));
            held["response_id"]="r2";held["frame_id"]=3;held["rtp_timestamp"]=63000;held["timestamp_ms"]=700;
            repeated.accept_gesture(held);
            check(repeated.render_for_test(2,first+1500)["stickers"].empty(),"same event restarted its display lifetime");
        }
        {
            // Source export ignores wall arrival, preserves source-time expiry,
            // and can drain through a one-frame recorder without overflow.
            mf::PlayoutConfig cfg;cfg.gesture_render_mode="source";cfg.recording_capacity=1;
            mf::Renderer offline(root,"",(root/"emoji").string(),true,cfg);
            offline.start();
            std::this_thread::sleep_for(std::chrono::milliseconds(10));
            offline.accept_gesture(g);offline.accept(mask(2,9000));
            check(offline.status()["frames_rendered"]==0,"source mode rendered before callback seal");
            const auto restored=offline.render_for_test(1,0);
            check(restored["stickers"].size()==1 && restored["mask_frame_id"]==2,"late source results not restored");
            check(offline.render_for_test(0,0)["stickers"].empty(),"source export used future gesture");
            check(offline.render_for_test(2,0)["stickers"].empty(),"source export extended expired reaction");
            offline.finish();
            for(int i=0;i<300 && !offline.status()["finalized"].get<bool>() && offline.status()["error"]=="";++i)
                std::this_thread::sleep_for(std::chrono::milliseconds(10));
            check(offline.status()["finalized"].get<bool>(),"source export failed to drain bounded recorder");
            check(!offline.status()["realtime"].get<bool>(),"source export misreported as realtime");
            std::ifstream trace(root/"gesture-trace.jsonl");std::string line;int callbacks=0,output=0;
            while(std::getline(trace,line)) { const auto row=json::parse(line);
                check(row["mode"]=="source","wrong trace mode");
                if(row["event"]=="gesture_callback")++callbacks;else if(row["event"]=="gesture_frame")++output;
            }
            check(callbacks==1 && output==3,"incomplete callback/frame trace");
        }
        {
            mf::PlayoutConfig cfg;cfg.result_capacity=1;
            mf::Renderer bounded(root,"",(root/"emoji").string(),false,cfg);
            bounded.accept_gesture(g);
            auto other=g;other["response_id"]="other";other["rtp_timestamp"]=0;
            bool full=false;try{bounded.accept_gesture(other);}catch(const mf::CapacityError&){full=true;}
            check(full,"result capacity was not bounded");
            check(bounded.accept_gesture(g)["duplicate"],"retry consumes capacity");
        }
        {
            std::filesystem::remove_all(root/"output");
            mf::PlayoutConfig cfg;cfg.delay_ms=80;
            mf::Renderer live(root,"",(root/"emoji").string(),false,cfg);
            live.start();live.accept_gesture(g);
            for(int i=0;i<100 && live.status()["frames_written"]==0;++i)
                std::this_thread::sleep_for(std::chrono::milliseconds(5));
            check(live.status()["frames_written"].get<size_t>()>0,"realtime pixels were not captured");
            check(!std::filesystem::exists(root/"output/000001.png"),"PNG encoding ran during realtime capture");
            live.finish();
            check(!live.status()["finalized"].get<bool>(),"finalize fast-forwarded playout clock");
            for(int i=0;i<300 && !live.status()["finalized"].get<bool>();++i)
                std::this_thread::sleep_for(std::chrono::milliseconds(10));
            const auto status=live.status();
            if (status["error"]!="" || !status["finalized"].get<bool>()) throw std::runtime_error("async recording failed: " + status.dump());
            check(status["frames_rendered"]==3 && status["frames_written"]==3,"incomplete live recording");
            std::ifstream input(root/"render.json");const auto timeline=json::parse(input);
            check(timeline.size()==3 && timeline[1]["stickers"].size()==1,"recording differs from realtime decision");
            check(mf::read_image(root/"output/000001.png").pixels[0]==50,"gesture-only discarded source");
        }
        {
            mf::PlayoutConfig cfg;cfg.recording_bytes=1;
            mf::Renderer bounded(root,"",(root/"emoji").string(),false,cfg);
            bounded.start();bounded.finish();
            for(int i=0;i<100 && bounded.status()["error"]=="";++i)
                std::this_thread::sleep_for(std::chrono::milliseconds(10));
            check(bounded.status()["error"]!="","recorder overload hidden");
        }
        {
            auto raw_frames = frames;
            std::ofstream raw(root/"frames.rgba",std::ios::binary);
            for(size_t i=0;i<raw_frames.size();++i) {
                raw_frames[i]["rgba_offset"]=i*source.pixels.size();
                raw.write(reinterpret_cast<const char*>(source.pixels.data()),source.pixels.size());
            }
            raw.close(); std::ofstream(root/"frames.json")<<raw_frames;
            mf::Renderer raw_source(root,"",(root/"emoji").string(),false);
            raw_source.render_for_test(1,0);
            check(mf::read_image(root/"output/000002.png").pixels==source.pixels,"raw source adapter changed pixels");
        }
        std::filesystem::remove_all(root);
        std::cout<<"gesture renderer: PASS\n";return 0;
    }catch(const std::exception& e){std::cerr<<e.what()<<'\n';std::filesystem::remove_all(root);return 1;}
}
