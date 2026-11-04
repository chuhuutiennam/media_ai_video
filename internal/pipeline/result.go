package pipeline

// MediaResource identifies an H.248/MEGACO media termination.
type MediaResource struct {
	ContextID     string `json:"contextId,omitempty"`
	TerminationID string `json:"terminationId,omitempty"`
}

// MediaResources holds the core-side and access-side H.248 terminations for a session.
// Each session only populates one side (TCore or TAccess); omitempty ensures the absent
// side does not serialize as {"contextId":"","terminationId":""} in the callback payload.
type MediaResources struct {
	TCore   *MediaResource `json:"tCore,omitempty"`
	TAccess *MediaResource `json:"tAccess,omitempty"`
}

// VideoMask là kết quả segmentation trả về từ AI Worker cho một frame video,
// mã hóa RLE (run-length encoding) theo cùng quy ước với mcf's VideoMask.
type VideoMask struct {
	FrameID      int64    `json:"frame_id"`
	RTPTimestamp uint32   `json:"rtp_timestamp"`
	MaskWidth    int32    `json:"mask_width"`
	MaskHeight   int32    `json:"mask_height"`
	RLECounts    []uint32 `json:"rle_counts"`
	LatencyMs    float32  `json:"latency_ms,omitempty"`
	Status       string   `json:"status,omitempty"`
	ErrorMessage string   `json:"error_message,omitempty"`
}

// RecognitionResult là transcript/kết quả trả về từ AI Worker.
// Nếu AudioPayload != nil, coordinator sẽ gửi payload này về MF qua RTP egress
// thay vì (hoặc song song với) dispatch text sang callback.
type RecognitionResult struct {
	SessionID      string          `json:"session_id"`
	StreamID       string          `json:"stream_id"`
	SourceType     string          `json:"source_type"`
	Kind           string          `json:"kind,omitempty"` // "" | "transcript" | "video_mask"
	Text           string          `json:"text,omitempty"`
	IsFinal        bool            `json:"is_final"`
	TsStart        int64           `json:"ts_start,omitempty"`
	TsEnd          int64           `json:"ts_end,omitempty"`
	Confidence     float32         `json:"confidence,omitempty"`
	Language       string          `json:"language,omitempty"`
	Seq            uint64          `json:"seq"`
	MediaResources *MediaResources `json:"mediaResources,omitempty"`
	VideoMask      *VideoMask      `json:"video_mask,omitempty"`

	// AudioPayload chứa audio đã encode (cùng codec với session, e.g. PCMU)
	// do AI Worker trả về để gateway gửi ngược lại MF qua RTP.
	// Nil = text-only result (hành vi hiện tại).
	AudioPayload []byte `json:"audio_payload,omitempty"`
	// AudioPT là RTP payload type của AudioPayload (e.g. 0 cho PCMU).
	AudioPT uint8 `json:"audio_pt,omitempty"`

	GestureResult *GestureFrameResult `json:"gesture_result,omitempty"`
	AIService     string              `json:"ai_service,omitempty"`
}

// IsVideoMask trả về true nếu result mang theo video mask (thay vì transcript).
func (r RecognitionResult) IsVideoMask() bool {
	return r.Kind == "video_mask" || r.VideoMask != nil
}

func (r RecognitionResult) IsVideoGesture() bool {
	return r.Kind == "video_gesture" || r.GestureResult != nil
}
