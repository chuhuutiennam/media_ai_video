package pipeline

import (
	"bytes"
	"image/jpeg"
	"media-ai-gateway/internal/codec/video"
	"testing"
	"time"
)

func TestGestureFanoutSamplesMediaTimeAndIsolatesQueues(t *testing.T) {
	mask, gesture := make(chan VideoFrame, 1), make(chan VideoFrame, 8)
	sp := &sessionVideoPipeline{cfg: VideoSessionConfig{InferWidth: 8, InferHeight: 4, JPEGQuality: 80, Gesture: &GestureOutputConfig{FPS: 10, MaxDimension: 4, StreamID: "call:tcore:video", Output: gesture}},
		frameOut: mask, aiInterval: 200 * time.Millisecond}
	f := &video.Frame{Width: 8, Height: 4, PixelFormat: video.PixelFormatYUV420P, StrideY: 8, StrideCb: 4, StrideCr: 4, Y: make([]byte, 32), Cb: make([]byte, 8), Cr: make([]byte, 8)}
	for i := range 5 {
		f.TimestampMs = int64(i * 100)
		f.RTPTimestamp = uint32(i * 9000)
		sp.handleDecodedFrame(f)
	}
	if len(gesture) != 5 || len(mask) != 1 {
		t.Fatalf("queues competed or sampled CPU time: gesture=%d mask=%d", len(gesture), len(mask))
	}
	last := <-mask
	if last.FrameID != 5 {
		t.Fatal("segmentation did not retain newest source frame")
	}
	for i := range 5 {
		g := <-gesture
		img, err := jpeg.Decode(bytes.NewReader(g.JPEG))
		if err != nil {
			t.Fatal(err)
		}
		if img.Bounds().Dx() != 4 || img.Bounds().Dy() != 2 {
			t.Fatal("gesture JPEG lost source aspect ratio")
		}
		if g.OriginalWidth != 8 || g.OriginalHeight != 4 {
			t.Fatal("original dimensions lost")
		}
		if g.FrameID != int64(i+1) || g.Width != 4 || g.Height != 2 || len(g.JPEG) == 0 {
			t.Fatalf("bad JPEG/identity: %+v", g)
		}
	}
}

func TestGestureOnlySkipsSegmentationPreprocessing(t *testing.T) {
	mask, gesture := make(chan VideoFrame, 1), make(chan VideoFrame, 8)
	sp := &sessionVideoPipeline{cfg: VideoSessionConfig{DisableSegmentation: true, JPEGQuality: 80,
		Gesture: &GestureOutputConfig{FPS: 10, MaxDimension: 8, StreamID: "call:tcore:video", Output: gesture}}, frameOut: mask}
	f := &video.Frame{Width: 8, Height: 4, PixelFormat: video.PixelFormatYUV420P, StrideY: 8, StrideCb: 4, StrideCr: 4,
		Y: make([]byte, 32), Cb: make([]byte, 8), Cr: make([]byte, 8)}
	for i := range 5 {
		f.TimestampMs = int64(i * 100)
		sp.handleDecodedFrame(f)
	}
	if len(gesture) != 5 || len(mask) != 0 || sp.hasMediaSample || sp.aiFrameSeq.Load() != 0 || sp.framesOut.Load() != 5 {
		t.Fatalf("gesture-only generated mask work: gesture=%d masks=%d frames=%d", len(gesture), len(mask), sp.framesOut.Load())
	}
	if sp.decodeErrs.Load() != 0 {
		t.Fatal("gesture-only JPEG preprocessing failed")
	}
}

func TestEngineSourceEncodesOneAspectPreservingOutput(t *testing.T) {
	out, unusedGesture := make(chan VideoFrame, 8), make(chan VideoFrame, 8)
	sp := &sessionVideoPipeline{cfg: VideoSessionConfig{SessionID: "call-tcore", StreamID: "call:tcore:video", SourceMaxDimension: 4, JPEGQuality: 80,
		Gesture: &GestureOutputConfig{FPS: 15, MaxDimension: 4, Output: unusedGesture}}, frameOut: out, aiInterval: 100 * time.Millisecond}
	f := &video.Frame{Width: 8, Height: 4, PixelFormat: video.PixelFormatYUV420P, StrideY: 8, StrideCb: 4, StrideCr: 4, Y: make([]byte, 32), Cb: make([]byte, 8), Cr: make([]byte, 8)}
	for i := 0; i < 5; i++ {
		f.TimestampMs = int64(i * 50)
		sp.handleDecodedFrame(f)
	}
	if len(out) != 3 || len(unusedGesture) != 0 {
		t.Fatalf("source still fans out at gateway: source=%d gesture=%d", len(out), len(unusedGesture))
	}
	for _, id := range []int64{1, 3, 5} {
		frame := <-out
		if frame.FrameID != id || frame.Width != 4 || frame.Height != 2 || frame.OriginalWidth != 8 || frame.OriginalHeight != 4 || len(frame.JPEG) == 0 {
			t.Fatalf("source identity/aspect: %+v", frame)
		}
	}
}

func TestEngineSourceSamplingToleratesMillisecondTimestamps(t *testing.T) {
	out := make(chan VideoFrame, 32)
	sp := &sessionVideoPipeline{cfg: VideoSessionConfig{SourceMaxDimension: 4, JPEGQuality: 80}, frameOut: out, aiInterval: time.Second / 15}
	f := &video.Frame{Width: 8, Height: 4, PixelFormat: video.PixelFormatYUV420P, StrideY: 8, StrideCb: 4, StrideCr: 4, Y: make([]byte, 32), Cb: make([]byte, 8), Cr: make([]byte, 8)}
	for i := 0; i < 30; i++ {
		f.TimestampMs = int64(i * 1000 / 30)
		sp.handleDecodedFrame(f)
	}
	if len(out) != 15 {
		t.Fatalf("15 FPS source degraded due to timestamp rounding: %d", len(out))
	}
}
