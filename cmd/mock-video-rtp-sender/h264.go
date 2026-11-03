package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

// h264Frames generates a test pattern or transcodes input to H264, then packetizes
// each access unit separately. AUD delimiters preserve multi-slice frames.
func h264Frames(ctx context.Context, width, height, fps int, duration time.Duration, input string, ssrc uint32) ([][]*rtp.Packet, error) {
	if width <= 0 || height <= 0 || width%2 != 0 || height%2 != 0 || fps <= 0 || fps > 120 || duration <= 0 {
		return nil, fmt.Errorf("positive even dimensions, fps in 1..120 and positive duration required")
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	if input == "" {
		args = append(args, "-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=%dx%d:rate=%d", width, height, fps))
	} else {
		args = append(args, "-stream_loop", "-1", "-i", input)
	}
	args = append(args, "-t", fmt.Sprintf("%.3f", duration.Seconds()), "-an", "-vf", fmt.Sprintf("scale=%d:%d,fps=%d", width, height, fps),
		"-c:v", "libx264", "-threads", "1", "-profile:v", "baseline", "-tune", "zerolatency", "-pix_fmt", "yuv420p",
		"-x264-params", "aud=1:repeat-headers=1:keyint=10", "-f", "h264", "pipe:1")
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg: %w: %s", err, stderr.String())
	}
	// Find NAL boundaries, accepting both 3- and 4-byte Annex-B prefixes.
	var units [][]byte
	start := -1
	for i := 0; i+3 <= len(raw); {
		prefix := 0
		if bytes.HasPrefix(raw[i:], []byte{0, 0, 0, 1}) {
			prefix = 4
		} else if bytes.HasPrefix(raw[i:], []byte{0, 0, 1}) {
			prefix = 3
		}
		if prefix == 0 {
			i++
			continue
		}
		if start >= 0 && i > start {
			units = append(units, raw[start:i])
		}
		start, i = i+prefix, i+prefix
	}
	if start >= 0 && start < len(raw) {
		units = append(units, raw[start:])
	}
	packetizer := rtp.NewPacketizer(1200, 96, ssrc, &codecs.H264Payloader{}, rtp.NewRandomSequencer(), 90000)
	var frames [][]*rtp.Packet
	var au []byte
	flush := func() {
		if len(au) > 0 {
			packets := packetizer.Packetize(au, uint32(90000/fps))
			if len(packets) > 0 {
				frames = append(frames, packets)
			}
			au = nil
		}
	}
	for _, nal := range units {
		if len(nal) == 0 {
			continue
		}
		if nal[0]&0x1f == 9 {
			flush()
		}
		au = append(au, 0, 0, 0, 1)
		au = append(au, nal...)
	}
	flush()
	if len(frames) == 0 {
		return nil, fmt.Errorf("ffmpeg produced no H264 access units")
	}
	return frames, nil
}
