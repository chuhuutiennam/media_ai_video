// mock-video-rtp-sender sends paced H264 RTP over UDP for video E2E tests.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/pion/rtp"
)

func main() {
	target := flag.String("target", "127.0.0.1:41000", "destination UDP address")
	width := flag.Int("width", 320, "encoded width (even)")
	height := flag.Int("height", 240, "encoded height (even)")
	fps := flag.Int("fps", 10, "frames per second")
	duration := flag.Duration("duration", 3*time.Second, "clip duration")
	ssrc := flag.Uint("ssrc", 88001, "RTP SSRC")
	input := flag.String("input", "", "image/video or PCAP/PCAPNG file (detected by magic bytes)")
	inspect := flag.Bool("inspect", false, "describe input capture streams as JSON without sending")
	pcapStream := flag.Int("pcap-stream", 0, "H264 stream index in capture; see --inspect")
	framesDir := flag.String("frames-dir", "", "save original decoded frames and RTP timestamp index here")
	flag.Parse()
	var err error
	if *inspect {
		var capture bool
		capture, err = isCapture(*input)
		if err == nil {
			var streams []*captureStream
			if capture {
				streams, err = readCapture(*input)
			}
			if err == nil {
				err = json.NewEncoder(os.Stdout).Encode(map[string]any{"pcap": capture, "streams": streams})
			}
		}
	} else {
		err = send(*target, *width, *height, *fps, *duration, *input, uint32(*ssrc), *pcapStream, *framesDir)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func send(target string, width, height, fps int, duration time.Duration, input string, ssrc uint32, streamIndex int, framesDir string) error {
	if duration <= 0 {
		return fmt.Errorf("duration must be positive")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	capture, err := isCapture(input)
	if err != nil {
		return err
	}
	var packets []timedPacket
	var source *captureStream
	if capture {
		packets, source, err = capturePackets(input, streamIndex, duration)
	} else {
		var frames [][]*rtp.Packet
		frames, err = h264Frames(ctx, width, height, fps, duration, input, ssrc)
		for i, frame := range frames {
			for _, packet := range frame {
				packets = append(packets, timedPacket{Packet: packet, Offset: time.Duration(i) * time.Second / time.Duration(fps)})
			}
		}
	}
	if err != nil {
		return err
	}
	if framesDir != "" {
		if err := dumpFrames(framesDir, packets); err != nil {
			return fmt.Errorf("save source frames: %w", err)
		}
	}
	conn, err := net.Dial("udp", target)
	if err != nil {
		return err
	}
	defer conn.Close()
	start := time.Now()
	frames := 0
	for _, item := range packets {
		time.Sleep(time.Until(start.Add(item.Offset)))
		data, err := item.Packet.Marshal()
		if err != nil {
			return err
		}
		if _, err := conn.Write(data); err != nil {
			return err
		}
		if item.Packet.Marker {
			frames++
		}
	}
	result := map[string]any{
		"event": "sent", "frames": frames, "packets": len(packets), "source": source,
		"target": target, "ssrc": packets[0].Packet.SSRC, "duration_sec": packets[len(packets)-1].Offset.Seconds(),
	}
	if !capture {
		result["fps"] = fps
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
