package main

import (
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"

	"media-ai-gateway/internal/codec/video"
)

type sourceFrame struct {
	File         string  `json:"file"`
	RTPTimestamp uint32  `json:"rtp_timestamp"`
	Time         float64 `json:"time_sec"`
	Width        int     `json:"width"`
	Height       int     `json:"height"`
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0644)
}

func writePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	err = png.Encode(f, img)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

// Save source frames and their RTP timestamp index before playback, using the
// same RTP decoder as the gateway. MF consumes these files separately.
func dumpFrames(dir string, packets []timedPacket) error {
	if err := os.MkdirAll(filepath.Join(dir, "source"), 0755); err != nil {
		return err
	}
	decoder, err := video.NewDecoder(video.Config{})
	if err != nil {
		return err
	}
	defer decoder.Close()
	var frames []sourceFrame
	seen := make(map[uint32]bool)
	firstTS := packets[0].Packet.Timestamp
	for _, item := range packets {
		p := item.Packet
		if _, err := decoder.PushRTP(p.SequenceNumber, p.Timestamp, p.Marker, p.Payload); err != nil {
			return err
		}
		for {
			f, ok := decoder.ReceiveFrame()
			if !ok {
				break
			}
			if f.PixelFormat != video.PixelFormatYUV420P || seen[f.RTPTimestamp] {
				return fmt.Errorf("source frame dump requires YUV420P and a unique RTP timestamp for each decoded frame")
			}
			seen[f.RTPTimestamp] = true
			img := &image.YCbCr{Y: f.Y, Cb: f.Cb, Cr: f.Cr, YStride: f.StrideY, CStride: f.StrideCb,
				SubsampleRatio: image.YCbCrSubsampleRatio420, Rect: image.Rect(0, 0, f.Width, f.Height)}
			frame := sourceFrame{File: fmt.Sprintf("source/%06d.png", len(frames)+1), RTPTimestamp: f.RTPTimestamp,
				Time: float64(f.RTPTimestamp-firstTS) / 90000, Width: f.Width, Height: f.Height}
			if err := writePNG(filepath.Join(dir, frame.File), img); err != nil {
				return err
			}
			frames = append(frames, frame)
		}
	}
	if len(frames) < 2 {
		return fmt.Errorf("need at least two decoded source frames to export video")
	}
	if stats := decoder.Stats(); stats.DecodeErrors != 0 || stats.ConversionErrors != 0 {
		return fmt.Errorf("source decoder errors: %+v", stats)
	}
	return writeJSON(filepath.Join(dir, "frames.json"), frames)
}
