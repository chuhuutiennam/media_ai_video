// Package audio xử lý PCM: resample, mix stereo→mono, chunk.
package audio

// Chunk là một đoạn PCM có độ dài cố định được tạo bởi Chunker.
// pipeline.AudioChunk được tạo từ Chunk này (tách để tránh import cycle).
type Chunk struct {
	SessionID    string
	StreamID     string
	PCM          []byte
	SampleRate   int
	Channels     int
	RTPTimestamp uint32 // RTP timestamp của packet đầu tiên trong chunk
	RTPClockRate int    // clock rate của RTP stream gốc (Hz, e.g. 8000, 48000); 0 = unknown
	DurationMs   int64
	ChunkSeq     uint64 // monotonic sequence per stream, 0-based; dùng để detect lost/reorder chunk
}

// ChunkerConfig định nghĩa tham số chunk đầu ra.
type ChunkerConfig struct {
	SampleRate   int // Hz đầu ra (ví dụ: 16000)
	Channels     int // kênh đầu ra (1 = mono)
	ChunkMs      int // độ dài chunk tính bằng ms (ví dụ: 500)
	RTPClockRate int // clock rate của RTP stream gốc (Hz); 0 = unknown
}

// Chunker tích lũy PCM int16 frames và phát ra Chunk có độ dài cố định.
//
// RTPTimestamp của mỗi chunk = RTP timestamp của packet RTP đầu tiên đóng góp
// sample vào chunk đó (lấy trực tiếp từ RTP header, đơn vị: sample).
//
// ChunkSeq tăng đơn điệu từ 0 per stream, độc lập với RTPTimestamp —
// dùng để phát hiện lost/reorder chunk giữa RTPGW và AI.
//
// Không goroutine-safe: dùng một Chunker per session/goroutine.
type Chunker struct {
	cfg       ChunkerConfig
	sessionID string
	streamID  string

	buf        []int16 // accumulation buffer
	chunkSamps int     // samples/chunk = SampleRate * ChunkMs / 1000

	seq        uint64 // monotonic chunk counter
	hasStartTS bool   // true khi curRTPTs đã được set cho chunk đang hình thành
	curRTPTs   uint32 // RTP timestamp của packet đầu tiên trong chunk hiện tại
}

// NewChunker tạo Chunker mới.
func NewChunker(cfg ChunkerConfig, sessionID, streamID string) *Chunker {
	if cfg.Channels <= 0 {
		cfg.Channels = 1
	}
	chunkSamps := cfg.SampleRate * cfg.ChunkMs / 1000
	return &Chunker{
		cfg:        cfg,
		sessionID:  sessionID,
		streamID:   streamID,
		buf:        make([]int16, 0, chunkSamps*2),
		chunkSamps: chunkSamps,
	}
}

// Buffered trả về số PCM samples đang giữ trong buffer.
func (c *Chunker) Buffered() int { return len(c.buf) }

// Push nối pcm vào buffer và trả về các chunk hoàn chỉnh (nếu có).
// rtpTs là RTP timestamp của packet RTP hiện tại (đơn vị: sample từ RTP header).
// Trả về nil nếu chưa đủ data.
func (c *Chunker) Push(pcm []int16, rtpTs uint32) []Chunk {
	if len(pcm) == 0 {
		return nil
	}

	// Ghi nhận timestamp của packet đầu tiên đóng góp vào chunk đang hình thành.
	if !c.hasStartTS {
		c.curRTPTs = rtpTs
		c.hasStartTS = true
	}
	c.buf = append(c.buf, pcm...)

	if len(c.buf) < c.chunkSamps {
		return nil
	}

	var chunks []Chunk
	for len(c.buf) >= c.chunkSamps {
		chunks = append(chunks, c.emit(c.buf[:c.chunkSamps]))
		c.buf = c.buf[c.chunkSamps:]
		if len(c.buf) == 0 {
			// Buffer rỗng sau emit — chunk tiếp theo bắt đầu bởi packet kế tiếp.
			c.hasStartTS = false
		} else {
			// Remainder còn lại đến từ packet hiện tại (hoặc sau này nếu packet rất lớn).
			// Cập nhật timestamp để chunk kế tiếp có timestamp chính xác nhất có thể.
			c.curRTPTs = rtpTs
			c.hasStartTS = true
		}
	}
	return chunks
}

// Flush phát phần còn lại trong buffer như một partial chunk.
// Trả về nil nếu buffer rỗng.
func (c *Chunker) Flush() *Chunk {
	if len(c.buf) == 0 {
		return nil
	}
	chunk := c.emit(c.buf)
	c.buf = c.buf[:0]
	c.hasStartTS = false
	return &chunk
}

// emit tạo Chunk từ samples dùng RTP timestamp và sequence hiện tại.
func (c *Chunker) emit(samples []int16) Chunk {
	durMs := int64(len(samples)) * 1000 / int64(c.cfg.SampleRate)
	seq := c.seq
	c.seq++
	return Chunk{
		SessionID:    c.sessionID,
		StreamID:     c.streamID,
		PCM:          Int16ToS16LE(samples),
		SampleRate:   c.cfg.SampleRate,
		Channels:     c.cfg.Channels,
		RTPTimestamp: c.curRTPTs,
		RTPClockRate: c.cfg.RTPClockRate,
		DurationMs:   durMs,
		ChunkSeq:     seq,
	}
}
