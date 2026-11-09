// mock-video-rtp-sender sends paced H264 RTP over UDP for video E2E tests.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pion/rtp"
	"golang.org/x/net/http2"
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
	playoutURL := flag.String("mf-start-url", "", "POST source-start to HTTP/2 MF after frame preparation, before RTP")
	deferPNG := flag.Bool("defer-source-png", false, "MF exports source PNG debug after realtime capture; requires frames-dir and mf-start-url")
	dropList := flag.String("drop-packets", "", "comma-separated 0-based RTP packet indexes (send order) to skip, simulating loss; sequence numbers stay consumed")
	flag.Parse()
	if *deferPNG && (*framesDir == "" || *playoutURL == "") {
		fmt.Fprintln(os.Stderr, "--defer-source-png requires --frames-dir and --mf-start-url")
		os.Exit(2)
	}
	drop, err := parseDropPackets(*dropList)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
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
		err = send(*target, *width, *height, *fps, *duration, *input, uint32(*ssrc), *pcapStream, *framesDir, *playoutURL, *deferPNG, drop)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// parseDropPackets đọc danh sách chỉ số gói cần bỏ ("37,120"); rỗng = không bỏ gói nào.
func parseDropPackets(list string) (map[int]bool, error) {
	drop := map[int]bool{}
	for _, field := range strings.Split(list, ",") {
		if field = strings.TrimSpace(field); field == "" {
			continue
		}
		index, err := strconv.Atoi(field)
		if err != nil || index < 0 {
			return nil, fmt.Errorf("--drop-packets: invalid packet index %q", field)
		}
		drop[index] = true
	}
	return drop, nil
}

func send(target string, width, height, fps int, duration time.Duration, input string, ssrc uint32, streamIndex int, framesDir string, playoutURL string, deferPNG bool, drop map[int]bool) error {
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
	for index := range drop {
		if index >= len(packets) {
			return fmt.Errorf("--drop-packets: index %d out of range (%d packets)", index, len(packets))
		}
	}
	if framesDir != "" {
		if err := dumpFramesWithOptions(framesDir, packets, !deferPNG); err != nil {
			return fmt.Errorf("save source frames: %w", err)
		}
	}
	conn, err := net.Dial("udp", target)
	if err != nil {
		return err
	}
	defer conn.Close()
	if playoutURL != "" {
		transport := &http2.Transport{AllowHTTP: true, DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}}
		client := &http.Client{Transport: transport, Timeout: 15 * time.Second}
		response, err := client.Post(playoutURL, "application/json", nil)
		if err != nil {
			return fmt.Errorf("MF source start: %w", err)
		}
		response.Body.Close()
		transport.CloseIdleConnections()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("MF source start: HTTP %d", response.StatusCode)
		}
	}
	start := time.Now()
	frames := 0
	droppedSeqs := []uint16{}
	for index, item := range packets {
		time.Sleep(time.Until(start.Add(item.Offset)))
		if drop[index] {
			droppedSeqs = append(droppedSeqs, item.Packet.SequenceNumber)
			continue
		}
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
		"dropped_packets": len(droppedSeqs), "dropped_seqs": droppedSeqs,
	}
	if !capture {
		result["fps"] = fps
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
