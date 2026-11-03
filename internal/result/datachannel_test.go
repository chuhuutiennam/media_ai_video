package result

import (
	"context"
	"encoding/json"
	"testing"

	pion "github.com/pion/webrtc/v4"

	"media-ai-gateway/internal/pipeline"
)

// mockDC is an in-test dcSender implementation.
type mockDC struct {
	state    pion.DataChannelState
	buffered uint64
	sent     []string
	sendErr  error
}

func (m *mockDC) ReadyState() pion.DataChannelState { return m.state }
func (m *mockDC) BufferedAmount() uint64            { return m.buffered }
func (m *mockDC) SendText(s string) error {
	if m.sendErr != nil {
		return m.sendErr
	}
	m.sent = append(m.sent, s)
	return nil
}

// sampleResult returns a RecognitionResult for use in tests.
func sampleResult(isFinal bool) pipeline.RecognitionResult {
	return pipeline.RecognitionResult{
		SessionID:  "sess-1",
		StreamID:   "stream-1",
		SourceType: "webrtc",
		Text:       "hello world",
		IsFinal:    isFinal,
		TsStart:    100,
		TsEnd:      500,
		Confidence: 0.95,
		Language:   "en",
		Seq:        42,
	}
}

func TestDataChannelSink_Type(t *testing.T) {
	dc := &mockDC{state: pion.DataChannelStateOpen}
	s := newDataChannelSink("sess-1", dc)
	if got := s.Type(); got != "datachannel" {
		t.Errorf("Type() = %q, want %q", got, "datachannel")
	}
}

func TestDataChannelSink_Send_NotOpen(t *testing.T) {
	dc := &mockDC{state: pion.DataChannelStateConnecting}
	s := newDataChannelSink("sess-1", dc)
	err := s.Send(context.Background(), sampleResult(false))
	if err != ErrDataChannelNotOpen {
		t.Errorf("Send() error = %v, want ErrDataChannelNotOpen", err)
	}
}

func TestDataChannelSink_Send_DroppedCounter(t *testing.T) {
	dc := &mockDC{state: pion.DataChannelStateConnecting}
	s := newDataChannelSink("sess-1", dc)

	_ = s.Send(context.Background(), sampleResult(false))
	_ = s.Send(context.Background(), sampleResult(false))
	_ = s.Send(context.Background(), sampleResult(true))

	if got := s.Dropped(); got != 3 {
		t.Errorf("Dropped() = %d, want 3", got)
	}
	if got := s.Sent(); got != 0 {
		t.Errorf("Sent() = %d, want 0", got)
	}
}

func TestDataChannelSink_Send_BufferFull(t *testing.T) {
	dc := &mockDC{
		state:    pion.DataChannelStateOpen,
		buffered: 1000,
	}
	s := newDataChannelSink("sess-1", dc).WithMaxBuffer(500)
	err := s.Send(context.Background(), sampleResult(false))
	if err != ErrDataChannelBufferFull {
		t.Errorf("Send() error = %v, want ErrDataChannelBufferFull", err)
	}
	if got := s.Dropped(); got != 1 {
		t.Errorf("Dropped() = %d, want 1", got)
	}
}

func TestDataChannelSink_Send_Open_Partial(t *testing.T) {
	dc := &mockDC{state: pion.DataChannelStateOpen}
	s := newDataChannelSink("sess-1", dc)

	if err := s.Send(context.Background(), sampleResult(false)); err != nil {
		t.Fatalf("Send() unexpected error: %v", err)
	}

	if len(dc.sent) != 1 {
		t.Fatalf("expected 1 message sent, got %d", len(dc.sent))
	}

	var msg DCMessage
	if err := json.Unmarshal([]byte(dc.sent[0]), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.Type != "transcript.partial" {
		t.Errorf("Type = %q, want %q", msg.Type, "transcript.partial")
	}
}

func TestDataChannelSink_Send_Open_Final(t *testing.T) {
	dc := &mockDC{state: pion.DataChannelStateOpen}
	s := newDataChannelSink("sess-1", dc)

	if err := s.Send(context.Background(), sampleResult(true)); err != nil {
		t.Fatalf("Send() unexpected error: %v", err)
	}

	if len(dc.sent) != 1 {
		t.Fatalf("expected 1 message sent, got %d", len(dc.sent))
	}

	var msg DCMessage
	if err := json.Unmarshal([]byte(dc.sent[0]), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.Type != "transcript.final" {
		t.Errorf("Type = %q, want %q", msg.Type, "transcript.final")
	}
}

func TestDataChannelSink_Send_JSONPayload(t *testing.T) {
	dc := &mockDC{state: pion.DataChannelStateOpen}
	s := newDataChannelSink("sess-1", dc)
	r := sampleResult(true)

	if err := s.Send(context.Background(), r); err != nil {
		t.Fatalf("Send() unexpected error: %v", err)
	}

	var msg DCMessage
	if err := json.Unmarshal([]byte(dc.sent[0]), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if msg.SessionID != r.SessionID {
		t.Errorf("SessionID = %q, want %q", msg.SessionID, r.SessionID)
	}
	if msg.Text != r.Text {
		t.Errorf("Text = %q, want %q", msg.Text, r.Text)
	}
	if msg.IsFinal != r.IsFinal {
		t.Errorf("IsFinal = %v, want %v", msg.IsFinal, r.IsFinal)
	}
	if msg.TsStart != r.TsStart {
		t.Errorf("TsStart = %d, want %d", msg.TsStart, r.TsStart)
	}
	if msg.TsEnd != r.TsEnd {
		t.Errorf("TsEnd = %d, want %d", msg.TsEnd, r.TsEnd)
	}
	if msg.Confidence != r.Confidence {
		t.Errorf("Confidence = %v, want %v", msg.Confidence, r.Confidence)
	}
	if msg.Language != r.Language {
		t.Errorf("Language = %q, want %q", msg.Language, r.Language)
	}
	if msg.Seq != r.Seq {
		t.Errorf("Seq = %d, want %d", msg.Seq, r.Seq)
	}
}

func TestDataChannelSink_Sent_Counter(t *testing.T) {
	dc := &mockDC{state: pion.DataChannelStateOpen}
	s := newDataChannelSink("sess-1", dc)

	for range 5 {
		if err := s.Send(context.Background(), sampleResult(false)); err != nil {
			t.Fatalf("Send() unexpected error: %v", err)
		}
	}

	if got := s.Sent(); got != 5 {
		t.Errorf("Sent() = %d, want 5", got)
	}
	if got := s.Dropped(); got != 0 {
		t.Errorf("Dropped() = %d, want 0", got)
	}
}

func TestDataChannelSink_ImplementsSink(t *testing.T) {
	var _ Sink = (*DataChannelSink)(nil)
}
