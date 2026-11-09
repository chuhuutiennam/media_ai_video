package ai

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"io"
	"testing"
	"time"

	"media-ai-gateway/internal/mockai"
	"media-ai-gateway/internal/pipeline"
)

func (c *fakeRecvClient) eof() { c.events <- fakeRecvEvent{err: io.EOF} }

// Worker kết thúc stream (OK/EOF) giữa phiên: lỗi, mở lại, kết quả chảy tiếp.
func TestVideoReconnect_WorkerEOFReopens(t *testing.T) {
	d := newFakeReconnectDialer()
	m, out, _ := openTestVideoStream(t, Config{MaxRetries: 3}, d)
	d.next(t).eof()
	d.next(t).result()
	receive(t, out)
	if st := m.Stats(); st.RecvErrors != 1 || st.StreamsStopped != 0 || m.Count() != 1 {
		t.Fatalf("worker EOF must reopen the stream: active=%d stats=%+v", m.Count(), st)
	}
}

// Worker cứ kết thúc ngay mà không có kết quả: dừng sau max_retry lần, có lý do.
func TestVideoReconnect_WorkerEOFWithoutResultsStops(t *testing.T) {
	logs := observeLogs(t)
	d := newFakeReconnectDialer()
	m, _, _ := openTestVideoStream(t, Config{MaxRetries: 2}, d)
	for range 3 {
		d.next(t).eof()
	}
	if !waitUntil(2*time.Second, func() bool { return m.Count() == 0 }) {
		t.Fatal("stream must stop after max_retry consecutive EOFs")
	}
	if got := stopReasons(logs); len(got) != 1 || got[0] != videoStopRetriesExhausted || d.dialCount() != 3 {
		t.Fatalf("reasons=%v dials=%d", got, d.dialCount())
	}
}

// Gateway đã kết thúc chiều gửi (đóng frame input / EOS) thì EOF của worker là bình thường.
func TestVideoReconnect_EOFAfterGatewayEOSIsNormal(t *testing.T) {
	logs := observeLogs(t)
	d := newFakeReconnectDialer()
	m := NewVideoManager(Config{MaxActiveStreams: 1, MaxRetries: 3, RetryBackoff: time.Millisecond}, d)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := make(chan pipeline.VideoFrame)
	s, err := m.Open(ctx, "s1", "s1", VideoStreamConfig{}, in, make(chan pipeline.RecognitionResult, 1))
	if err != nil {
		t.Fatal(err)
	}
	c := d.next(t)
	close(in)
	if !waitUntil(2*time.Second, s.eosSent.Load) {
		t.Fatal("closing frame input must mark EOS")
	}
	c.eof()
	if !waitUntil(2*time.Second, func() bool { return m.Count() == 0 }) {
		t.Fatal("stream must end after gateway EOS")
	}
	if st := m.Stats(); st.RecvErrors != 0 || st.StreamsStopped != 0 || d.dialCount() != 1 || len(stopReasons(logs)) != 0 {
		t.Fatalf("EOF after EOS must be silent: stats=%+v dials=%d", st, d.dialCount())
	}
}

// gRPC thật (mockai trong tiến trình): worker đóng stream OK sau mỗi 3 kết quả; gateway mở lại
// mỗi lần và nhận tiếp — trước khi sửa, kết quả dừng ở 3.
func TestVideoReconnect_RealWorkerEndsStream(t *testing.T) {
	pool := newJPEGTestPoolWithOptions(t, mockai.VideoOptions{EndStreamAfter: 3})
	registry := NewWorkerRegistry(0)
	registry.Register(WorkerInfo{ID: "seg", Addr: "passthrough:///mock", Tasks: []string{"video_segment"}, MediaTypes: []string{"video"},
		InputFormats: []string{"jpeg"}, Protocols: []string{"video_segment"}, MaxStreams: 2})
	m := NewVideoManager(Config{MaxActiveStreams: 2, MaxRetries: 2, RetryBackoff: time.Millisecond}, NewVideoRoutingDialer(registry, pool.VideoDialFunc()))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	in := make(chan pipeline.VideoFrame, 4)
	out := make(chan pipeline.RecognitionResult, 16)
	cfg := VideoStreamConfig{EffectType: "bg_replace", InferWidth: 4, InferHeight: 4, InferFPS: 5, SelectedService: "change_background"}
	if _, err := m.Open(ctx, "call-tcore", "call-tcore", cfg, in, out); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewYCbCr(image.Rect(0, 0, 4, 4), image.YCbCrSubsampleRatio420), nil); err != nil {
		t.Fatal(err)
	}
	go func() { // nguồn frame liên tục như pipeline (frame gửi vào stream sắp đóng có thể mất)
		for id := int64(1); ctx.Err() == nil; id++ {
			select {
			case in <- pipeline.VideoFrame{SessionID: "call-tcore", StreamID: "call-tcore", FrameID: id, TimestampMs: id * 200,
				RTPTimestamp: uint32(id * 18000), OriginalWidth: 4, OriginalHeight: 4, Width: 4, Height: 4, JPEG: buf.Bytes()}:
			case <-ctx.Done():
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	results := 0
	for results < 7 {
		select {
		case r := <-out:
			if r.VideoMask == nil || r.VideoMask.Status != "ok" {
				t.Fatalf("unexpected result %+v", r)
			}
			results++
		case <-ctx.Done():
			t.Fatalf("only %d results: stream did not survive worker EOF (stats=%+v)", results, m.Stats())
		}
	}
	if st := m.Stats(); st.RecvErrors < 2 || st.StreamsStopped != 0 || m.Count() != 1 {
		t.Fatalf("want ≥2 worker EOFs handled, no stop: active=%d stats=%+v", m.Count(), st)
	}
}
