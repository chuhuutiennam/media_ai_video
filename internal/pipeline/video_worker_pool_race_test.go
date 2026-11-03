package pipeline

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestVideoWorkerPool_ConcurrentSubmitAndUnregister dồn dập Submit() +
// UnregisterSession() cho CÙNG 1 session từ nhiều goroutine đồng thời,
// nhiều vòng lặp — nếu còn race, `go test -race` sẽ report, hoặc chương
// trình panic/crash do gọi cgo trên handle đã destroy.
func TestVideoWorkerPool_ConcurrentSubmitAndUnregister(t *testing.T) {
	const rounds = 50

	for i := 0; i < rounds; i++ {
		wp := NewVideoWorkerPool(VideoWorkerPoolConfig{Workers: 4, QueueSize: 64})
		ctx, cancel := context.WithCancel(context.Background())
		go wp.Run(ctx)

		sessionID := "race-session"
		frameOut := make(chan VideoFrame, 8)

		// RegisterSession dùng decoder giả (không cần ffmpeg/H264 thật —
		// chỉ cần chứng minh không use-after-free ở TẦNG Go, không quan tâm
		// decode có ra frame hay không trong test race này).
		if err := wp.RegisterSession(ctx, VideoSessionConfig{SessionID: sessionID}, frameOut); err != nil {
			t.Fatalf("round %d: RegisterSession: %v", i, err)
		}

		var wg sync.WaitGroup
		wg.Add(2)

		// Goroutine 1: dồn dập Submit() job cho session này.
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = wp.Submit(ctx, VideoJob{
					SessionID: sessionID,
					Packet:    MediaPacket{SessionID: sessionID, Sequence: uint16(j), Payload: []byte{0x67, 0x00}}, // NAL giả tối thiểu, không cần hợp lệ H264 thật cho test race này
				})
			}
		}()

		// Goroutine 2: UnregisterSession gần như ngay lập tức — mô phỏng
		// đúng kịch bản race thật (DCSF RELEASE tới trong lúc packet vẫn
		// đang được xử lý).
		go func() {
			defer wg.Done()
			time.Sleep(time.Microsecond) // nhường 1 nhịp cho vài job kịp vào queue
			wp.UnregisterSession(sessionID)
		}()

		wg.Wait()
		cancel()
	}
}
