package ai

import (
	"context"
	"io"

	"media-ai-gateway/internal/pipeline"
)

// NullDialer là Dialer dành cho dev/test khi chưa có AI backend thật.
//
// Mỗi stream do NullDialer tạo ra:
//   - Chấp nhận mọi AudioChunk.Send() mà không làm gì.
//   - Block trên Recv() cho đến khi context bị cancel, rồi trả về io.EOF.
//
// Hiệu ứng: session chạy đầy đủ pipeline (jitter → worker pool → audio chunk)
// nhưng không sinh ra RecognitionResult nào. Thích hợp để smoke-test ingress.
type NullDialer struct{}

func (NullDialer) Dial(ctx context.Context, _, _, _, _ string) (StreamClient, error) {
	return &nullClient{ctx: ctx}, nil
}

type nullClient struct{ ctx context.Context }

func (c *nullClient) Send(_ pipeline.AudioChunk) error {
	select {
	case <-c.ctx.Done():
		return c.ctx.Err()
	default:
		return nil
	}
}

// Recv block cho đến khi context bị cancel, rồi báo EOF (stream kết thúc sạch).
func (c *nullClient) Recv() (pipeline.RecognitionResult, error) {
	<-c.ctx.Done()
	return pipeline.RecognitionResult{}, io.EOF
}

func (c *nullClient) CloseSend() error { return nil }
