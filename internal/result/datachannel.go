package result

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"

	pion "github.com/pion/webrtc/v4"

	"media-ai-gateway/internal/pipeline"
)

// dcSender is an unexported interface for testability — matches the subset of
// *pion.DataChannel methods that DataChannelSink actually uses.
type dcSender interface {
	ReadyState() pion.DataChannelState
	BufferedAmount() uint64
	SendText(s string) error
}

var (
	ErrDataChannelNotOpen    = errors.New("result: data channel not open")
	ErrDataChannelBufferFull = errors.New("result: data channel buffer full")
)

const defaultDCMaxBuffer uint64 = 16 * 1024

// DCMessage is the JSON envelope sent over the DataChannel to the UE.
type DCMessage struct {
	Type       string  `json:"type"`
	SessionID  string  `json:"session_id"`
	Text       string  `json:"text"`
	IsFinal    bool    `json:"is_final"`
	TsStart    int64   `json:"ts_start"`
	TsEnd      int64   `json:"ts_end"`
	Confidence float32 `json:"confidence,omitempty"`
	Language   string  `json:"language,omitempty"`
	Seq        uint64  `json:"seq"`
}

// DataChannelSink delivers RecognitionResults to a UE via a WebRTC DataChannel.
// It implements result.Sink.
type DataChannelSink struct {
	sessID    string
	dc        dcSender
	maxBuffer uint64
	sent      atomic.Uint64
	dropped   atomic.Uint64
}

// NewDataChannelSink creates a DataChannelSink backed by a real *pion.DataChannel.
func NewDataChannelSink(sessID string, dc *pion.DataChannel) *DataChannelSink {
	return newDataChannelSink(sessID, dc)
}

// newDataChannelSink creates a DataChannelSink backed by any dcSender (used in tests).
func newDataChannelSink(sessID string, dc dcSender) *DataChannelSink {
	return &DataChannelSink{
		sessID:    sessID,
		dc:        dc,
		maxBuffer: defaultDCMaxBuffer,
	}
}

// WithMaxBuffer overrides the backpressure threshold (bytes buffered in the DC send
// buffer). 0 disables the check. Returns the receiver for chaining.
func (s *DataChannelSink) WithMaxBuffer(n uint64) *DataChannelSink {
	s.maxBuffer = n
	return s
}

// Type satisfies result.Sink.
func (s *DataChannelSink) Type() string { return "datachannel" }

// Send encodes r as a DCMessage and writes it to the DataChannel.
// Returns ErrDataChannelNotOpen if the channel is not yet open, or
// ErrDataChannelBufferFull if backpressure is exceeded.
func (s *DataChannelSink) Send(_ context.Context, r pipeline.RecognitionResult) error {
	if s.dc.ReadyState() != pion.DataChannelStateOpen {
		s.dropped.Add(1)
		return ErrDataChannelNotOpen
	}
	if s.maxBuffer > 0 && s.dc.BufferedAmount() > s.maxBuffer {
		s.dropped.Add(1)
		return ErrDataChannelBufferFull
	}

	msgType := "transcript.partial"
	if r.IsFinal {
		msgType = "transcript.final"
	}

	msg := DCMessage{
		Type:       msgType,
		SessionID:  r.SessionID,
		Text:       r.Text,
		IsFinal:    r.IsFinal,
		TsStart:    r.TsStart,
		TsEnd:      r.TsEnd,
		Confidence: r.Confidence,
		Language:   r.Language,
		Seq:        r.Seq,
	}

	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	if err := s.dc.SendText(string(b)); err != nil {
		return err
	}

	s.sent.Add(1)
	return nil
}

// Sent returns the total number of messages successfully delivered.
func (s *DataChannelSink) Sent() uint64 { return s.sent.Load() }

// Dropped returns the total number of messages dropped (channel not open or buffer full).
func (s *DataChannelSink) Dropped() uint64 { return s.dropped.Load() }
