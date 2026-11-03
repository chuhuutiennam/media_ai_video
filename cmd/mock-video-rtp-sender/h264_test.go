package main

import (
	"context"
	"testing"
	"time"

	"media-ai-gateway/internal/codec/video"
)

func TestH264FramesDecode(t *testing.T) {
	frames, err := h264Frames(context.Background(), 320, 240, 10, time.Second, "", 88001)
	if err != nil {
		t.Fatal(err)
	}
	decoder, err := video.NewDecoder(video.Config{MaxWidth: 1920, MaxHeight: 1080})
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	count := 0
	for _, packets := range frames {
		for _, p := range packets {
			if _, err := decoder.PushRTP(p.SequenceNumber, p.Timestamp, p.Marker, p.Payload); err != nil {
				t.Fatal(err)
			}
			for {
				frame, ok := decoder.ReceiveFrame()
				if !ok {
					break
				}
				if frame.Width != 320 || frame.Height != 240 {
					t.Fatalf("wrong dimensions: %+v", frame)
				}
				count++
			}
		}
	}
	if count != 10 {
		t.Fatalf("decoded %d frames, want 10; stats=%+v", count, decoder.Stats())
	}
}
