package pipeline

// MediaPacket là model chuẩn hóa từ Raw RTP và WebRTC ingress.
type MediaPacket struct {
	SessionID   string
	SourceType  string // "raw_rtp" | "webrtc"

	SSRC        uint32
	PayloadType uint8
	Sequence    uint16
	Timestamp   uint32
	Marker      bool

	Payload      []byte
	ReceivedAtMs int64

	Codec      string
	SampleRate int
	Channels   int
}

// AudioChunk là output của audio pipeline, sẵn sàng gửi sang AI.
type AudioChunk struct {
	SessionID    string
	StreamID     string
	PCM          []byte
	SampleRate   int
	Channels     int
	RTPTimestamp uint32 // RTP timestamp của packet đầu tiên trong chunk (đơn vị: RTPClockRate)
	RTPClockRate int    // clock rate của RTP stream gốc (Hz, e.g. 8000, 48000); 0 = unknown
	DurationMs   int64
	ChunkSeq     uint64 // monotonic sequence per stream, 0-based; detect lost/reorder chunk
	EndOfStream  bool   // true = chunk cuối của stream, AI worker đóng nhận dạng sau chunk này
}
