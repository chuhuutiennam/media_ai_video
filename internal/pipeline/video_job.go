package pipeline

// VideoJob là đơn vị công việc gửi vào Video Pipeline Worker Pool.
// Đại diện cho một MediaPacket (RTP video, H264) đã qua jitter buffer,
// sẵn sàng đưa vào decode chain (AU assembler → depacketizer → ffmpeg decode).
type VideoJob struct {
	SessionID string
	Packet    MediaPacket
}

// VideoFrame là một frame đã decode, resize về kích thước inference và
// JPEG-encode, sẵn sàng gửi sang AI Worker để chạy segmentation hoặc gesture.
type VideoFrame struct {
	SessionID            string
	StreamID             string
	FrameID              int64
	RTPTimestamp         uint32
	ExtendedRTPTimestamp uint64
	TimestampMs          int64
	OriginalWidth        int32
	OriginalHeight       int32
	JPEG                 []byte
	Width, Height        int    // Encoded JPEG dimensions.
	EffectType           string // "bg_blur" | "bg_replace" | "bg_remove"
	EndOfStream          bool
}
