// Package result nhận RecognitionResult từ AI Stream Manager và phân phối sang client.
package result

import (
	"context"

	"media-ai-gateway/internal/pipeline"
)

// Sink abstracts một cơ chế delivery cho RecognitionResult.
//
// Các implementation cụ thể: HTTPCallbackSink, WebSocketSink, DataChannelSink, KafkaSink.
// Send nên trả về nhanh; retry/buffer là trách nhiệm của implementation.
type Sink interface {
	// Send gửi một result. Phải context-aware để tránh block dispatcher.
	Send(ctx context.Context, r pipeline.RecognitionResult) error
	// Type trả về tên ngắn cho logging/metrics (vd: "http_callback", "websocket").
	Type() string
}

// SinkFunc bọc một function thành Sink. Hữu ích cho test và prototyping.
type SinkFunc struct {
	kind string
	fn   func(ctx context.Context, r pipeline.RecognitionResult) error
}

// NewSinkFunc tạo Sink từ một function.
func NewSinkFunc(kind string, fn func(ctx context.Context, r pipeline.RecognitionResult) error) Sink {
	return &SinkFunc{kind: kind, fn: fn}
}

func (s *SinkFunc) Send(ctx context.Context, r pipeline.RecognitionResult) error {
	return s.fn(ctx, r)
}

func (s *SinkFunc) Type() string { return s.kind }
