# Mock video AI Engine

`cmd/mock-ai-worker` registers `/video.VideoStream/Segment` for segmentation and
gesture, alongside the existing speech RPC. Both video tasks use the same
`VideoRequest` / `VideoResponse` schema from
`internal/ai/proto/video.proto`.

The first request contains config; `ai_service=gesture_recognition` selects gesture,
while empty or `segmentation` selects the existing luminance segmentation mock.
Following frames contain `encoding=jpeg` and JPEG `image_data`. EOS uses `is_eos` or
client half-close. RGB buffers are not accepted as gesture input.

The gesture mock validates JPEG content/dimensions and returns two moving hands,
Thumb_Up, Victory and Wave reactions, then an empty snapshot every three media
seconds. Results are scripted; they test transport and composition, not accuracy.

Run `bash scripts/test-video-e2e.sh --ai-mode mock` (change_background, segmentation) or
`bash scripts/test-video-e2e.sh --gesture-only --gesture-mode mock --output-video --duration 3`
(video_effects, gesture). No model, webcam or GPU is needed. Input can be video, PCAP/PCAPNG
or a generated test pattern. Real AI services live in the separate `mediapipe` and `gesture` repos.
See [video testing](../../docs/video-testing.md) for artifacts and scope.
