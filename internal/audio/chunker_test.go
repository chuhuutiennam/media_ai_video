package audio

import (
	"encoding/binary"
	"testing"
)

func defaultCfg() ChunkerConfig {
	return ChunkerConfig{SampleRate: 16000, Channels: 1, ChunkMs: 500}
}

// 500ms @ 16kHz = 8000 samples per chunk.

func TestChunker_NoChunkUntilFull(t *testing.T) {
	c := NewChunker(defaultCfg(), "sess", "stream")
	chunks := c.Push(make([]int16, 7999), 0)
	if len(chunks) != 0 {
		t.Errorf("expected 0 chunks, got %d", len(chunks))
	}
	if c.Buffered() != 7999 {
		t.Errorf("Buffered() = %d, want 7999", c.Buffered())
	}
}

func TestChunker_ExactOneChunk(t *testing.T) {
	c := NewChunker(defaultCfg(), "s", "st")
	chunks := c.Push(make([]int16, 8000), 0)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if c.Buffered() != 0 {
		t.Errorf("buffer should be empty, got %d", c.Buffered())
	}
}

func TestChunker_TwoChunksInOnePush(t *testing.T) {
	c := NewChunker(defaultCfg(), "s", "st")
	chunks := c.Push(make([]int16, 16000), 0) // 1000ms = 2 chunks
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
}

func TestChunker_AccumulateAcrossPushes(t *testing.T) {
	c := NewChunker(defaultCfg(), "s", "st")
	c.Push(make([]int16, 4000), 0) // 250ms — no chunk yet
	chunks := c.Push(make([]int16, 4000), 160) // total 500ms → 1 chunk
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk after accumulation, got %d", len(chunks))
	}
}

func TestChunker_Remainder_StaysBuffered(t *testing.T) {
	c := NewChunker(defaultCfg(), "s", "st")
	chunks := c.Push(make([]int16, 9000), 0) // 8000 chunk + 1000 remain
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if c.Buffered() != 1000 {
		t.Errorf("Buffered() = %d, want 1000", c.Buffered())
	}
}

// --- Flush ---

func TestChunker_Flush_Partial(t *testing.T) {
	c := NewChunker(defaultCfg(), "s", "st")
	c.Push(make([]int16, 3000), 0)
	chunk := c.Flush()
	if chunk == nil {
		t.Fatal("Flush() should return partial chunk")
	}
	if len(chunk.PCM) != 3000*2 {
		t.Errorf("PCM len = %d, want %d", len(chunk.PCM), 3000*2)
	}
	if c.Buffered() != 0 {
		t.Error("buffer should be empty after Flush")
	}
}

func TestChunker_Flush_Empty(t *testing.T) {
	c := NewChunker(defaultCfg(), "s", "st")
	if c.Flush() != nil {
		t.Error("Flush() on empty buffer should return nil")
	}
}

func TestChunker_Flush_AfterFullChunk(t *testing.T) {
	c := NewChunker(defaultCfg(), "s", "st")
	c.Push(make([]int16, 8000), 0)  // emits 1 chunk
	c.Push(make([]int16, 2000), 160) // buffers 2000 remaining
	chunk := c.Flush()
	if chunk == nil {
		t.Fatal("expected partial chunk from Flush")
	}
	if len(chunk.PCM) != 4000 { // 2000 samples * 2 bytes
		t.Errorf("PCM len = %d, want 4000", len(chunk.PCM))
	}
}

// --- PCM S16LE encoding ---

func TestChunker_PCMEncoding(t *testing.T) {
	c := NewChunker(defaultCfg(), "s", "st")
	in := make([]int16, 8000)
	in[0] = 1000
	in[1] = -1
	in[7999] = 32767

	chunks := c.Push(in, 0)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk")
	}
	pcm := chunks[0].PCM
	if len(pcm) != 16000 {
		t.Fatalf("PCM len = %d, want 16000", len(pcm))
	}
	if v := int16(binary.LittleEndian.Uint16(pcm[0:])); v != 1000 {
		t.Errorf("pcm[0] = %d, want 1000", v)
	}
	if v := int16(binary.LittleEndian.Uint16(pcm[2:])); v != -1 {
		t.Errorf("pcm[1] = %d, want -1", v)
	}
	if v := int16(binary.LittleEndian.Uint16(pcm[7999*2:])); v != 32767 {
		t.Errorf("pcm[7999] = %d, want 32767", v)
	}
}

func TestChunker_PCMEncoding_MinInt16(t *testing.T) {
	c := NewChunker(defaultCfg(), "s", "st")
	in := make([]int16, 8000)
	in[0] = -32768 // math.MinInt16
	chunks := c.Push(in, 0)
	pcm := chunks[0].PCM
	if v := int16(binary.LittleEndian.Uint16(pcm[0:])); v != -32768 {
		t.Errorf("pcm[0] = %d, want -32768", v)
	}
}

// --- RTP Timestamp ---

func TestChunker_RTPTimestamp_SinglePush(t *testing.T) {
	c := NewChunker(defaultCfg(), "s", "st")
	const rtpTs uint32 = 12345
	chunks := c.Push(make([]int16, 8000), rtpTs)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk")
	}
	if chunks[0].RTPTimestamp != rtpTs {
		t.Errorf("RTPTimestamp = %d, want %d", chunks[0].RTPTimestamp, rtpTs)
	}
}

func TestChunker_RTPTimestamp_AccumulateUsesFirst(t *testing.T) {
	// Hai push, chunk emit ở push thứ 2 — RTPTimestamp phải là của push đầu (packet đầu điền buffer).
	c := NewChunker(defaultCfg(), "s", "st")
	c.Push(make([]int16, 4000), 1000) // buffer rỗng → curRTPTs = 1000
	chunks := c.Push(make([]int16, 4000), 1160)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk")
	}
	if chunks[0].RTPTimestamp != 1000 {
		t.Errorf("RTPTimestamp = %d, want 1000 (first packet that filled buffer)", chunks[0].RTPTimestamp)
	}
}

func TestChunker_RTPTimestamp_TwoChunksInOnePush(t *testing.T) {
	// 2 chunks trong 1 push từ cùng 1 packet → cả 2 dùng cùng rtpTs.
	c := NewChunker(defaultCfg(), "s", "st")
	const rtpTs uint32 = 8000
	chunks := c.Push(make([]int16, 16000), rtpTs)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
	if chunks[0].RTPTimestamp != rtpTs {
		t.Errorf("chunk[0].RTPTimestamp = %d, want %d", chunks[0].RTPTimestamp, rtpTs)
	}
	if chunks[1].RTPTimestamp != rtpTs {
		t.Errorf("chunk[1].RTPTimestamp = %d, want %d", chunks[1].RTPTimestamp, rtpTs)
	}
}

func TestChunker_RTPTimestamp_OldSamplesSpanMultipleChunks(t *testing.T) {
	// Bug case: buffer có 10000 sample cũ (pkt0), push thêm 8000 sample (pkt1).
	// Emit 2 chunk: chunk1=[8000 pkt0], chunk2=[2000 pkt0 + 6000 pkt1].
	// Cả 2 chunk bắt đầu bởi pkt0 → RTPTimestamp phải là ts của pkt0.
	c := NewChunker(defaultCfg(), "s", "st")
	const ts0 uint32 = 0
	const ts1 uint32 = 800

	// Fill buffer với 10000 sample từ pkt0 (chưa đủ 2 chunk).
	c.Push(make([]int16, 10000), ts0) // 10000 < 8000*2=16000 → 1 chunk emitted
	// Sau push này buf có 10000-8000=2000 sample từ pkt0, curRTPTs=ts0.

	// Thực ra 10000 >= 8000 nên 1 chunk đã được emit, còn 2000 trong buf.
	// Reset để tạo scenario đúng: cần buf có 10000 trước khi push pkt1.
	// Dùng chunkMs=1000 để chunkSamps=16000 > 10000 → không emit.
	cfg := ChunkerConfig{SampleRate: 16000, Channels: 1, ChunkMs: 1000} // chunkSamps=16000
	c2 := NewChunker(cfg, "s", "st")

	// Push 10000 samples từ pkt0, chưa đủ 1 chunk (16000).
	c2.Push(make([]int16, 10000), ts0) // buffered, không emit

	// Push 10000 samples từ pkt1 → tổng 20000 >= 16000 → emit 1 chunk.
	chunks := c2.Push(make([]int16, 10000), ts1)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	// Chunk bắt đầu bởi 10000 sample pkt0 → RTPTimestamp = ts0.
	if chunks[0].RTPTimestamp != ts0 {
		t.Errorf("RTPTimestamp = %d, want ts0=%d (chunk starts with pkt0 samples)", chunks[0].RTPTimestamp, ts0)
	}
	// Buffer còn 4000 sample từ pkt1. Chunk kế tiếp bắt đầu bởi pkt1 → curRTPTs = ts1.
	// Verify bằng cách flush.
	flushed := c2.Flush()
	if flushed == nil {
		t.Fatal("expected flushed partial chunk")
	}
	if flushed.RTPTimestamp != ts1 {
		t.Errorf("flushed RTPTimestamp = %d, want ts1=%d", flushed.RTPTimestamp, ts1)
	}
}

func TestChunker_RTPTimestamp_Flush(t *testing.T) {
	c := NewChunker(defaultCfg(), "s", "st")
	const rtpTs uint32 = 9999
	c.Push(make([]int16, 3000), rtpTs)
	chunk := c.Flush()
	if chunk == nil {
		t.Fatal("expected partial chunk from Flush")
	}
	if chunk.RTPTimestamp != rtpTs {
		t.Errorf("Flush RTPTimestamp = %d, want %d", chunk.RTPTimestamp, rtpTs)
	}
}

func TestChunker_DurationMs(t *testing.T) {
	c := NewChunker(defaultCfg(), "s", "st")
	chunks := c.Push(make([]int16, 8000), 0)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk")
	}
	if chunks[0].DurationMs != 500 {
		t.Errorf("DurationMs = %d, want 500", chunks[0].DurationMs)
	}
}

// --- Metadata ---

func TestChunker_Metadata(t *testing.T) {
	c := NewChunker(defaultCfg(), "session-abc", "stream-xyz")
	chunks := c.Push(make([]int16, 8000), 0)
	if len(chunks) != 1 {
		t.Fatal("expected 1 chunk")
	}
	ch := chunks[0]
	if ch.SessionID != "session-abc" {
		t.Errorf("SessionID = %q, want %q", ch.SessionID, "session-abc")
	}
	if ch.StreamID != "stream-xyz" {
		t.Errorf("StreamID = %q, want %q", ch.StreamID, "stream-xyz")
	}
	if ch.SampleRate != 16000 {
		t.Errorf("SampleRate = %d, want 16000", ch.SampleRate)
	}
	if ch.Channels != 1 {
		t.Errorf("Channels = %d, want 1", ch.Channels)
	}
}

// --- Edge cases ---

func TestChunker_Push_Nil(t *testing.T) {
	c := NewChunker(defaultCfg(), "s", "st")
	if c.Push(nil, 0) != nil {
		t.Error("Push(nil) should return nil")
	}
}

func TestChunker_Push_Empty(t *testing.T) {
	c := NewChunker(defaultCfg(), "s", "st")
	if c.Push([]int16{}, 0) != nil {
		t.Error("Push(empty) should return nil")
	}
}

func TestChunker_1000msChunk(t *testing.T) {
	cfg := ChunkerConfig{SampleRate: 16000, Channels: 1, ChunkMs: 1000}
	c := NewChunker(cfg, "s", "st")
	chunks := c.Push(make([]int16, 16000), 0)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for 1000ms, got %d", len(chunks))
	}
	if chunks[0].DurationMs != 1000 {
		t.Errorf("DurationMs = %d, want 1000", chunks[0].DurationMs)
	}
	if len(chunks[0].PCM) != 32000 {
		t.Errorf("PCM len = %d, want 32000", len(chunks[0].PCM))
	}
}
