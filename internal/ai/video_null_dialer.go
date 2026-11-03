package ai

import (
	"context"
	"io"

	"media-ai-gateway/internal/pipeline"
)

// NullVideoDialer là VideoDialer dành cho dev/test khi chưa có AI video backend thật.
// Đối xứng với NullDialer (audio) — xem null_dialer.go.
type NullVideoDialer struct{}

func (NullVideoDialer) Dial(ctx context.Context, _, _ string, _ VideoStreamConfig) (VideoStreamClient, error) {
	return &nullVideoClient{ctx: ctx}, nil
}

type nullVideoClient struct{ ctx context.Context }

func (c *nullVideoClient) Send(_ pipeline.VideoFrame) error {
	select {
	case <-c.ctx.Done():
		return c.ctx.Err()
	default:
		return nil
	}
}

func (c *nullVideoClient) Recv() (pipeline.RecognitionResult, error) {
	<-c.ctx.Done()
	return pipeline.RecognitionResult{}, io.EOF
}

func (c *nullVideoClient) CloseSend() error { return nil }
