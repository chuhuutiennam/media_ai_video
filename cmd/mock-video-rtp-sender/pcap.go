package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcapgo"
	"github.com/pion/rtp"
)

type timedPacket struct {
	Packet *rtp.Packet
	Offset time.Duration
}

type captureStream struct {
	Index       int     `json:"index"`
	Source      string  `json:"source"`
	Destination string  `json:"destination"`
	SSRC        uint32  `json:"ssrc"`
	PayloadType uint8   `json:"payload_type"`
	Duration    float64 `json:"duration_sec"`
	Packets     int     `json:"packets"`
	Frames      int     `json:"frames"`
	packets     []timedPacket
	first       time.Time
	sps, pps    bool
}

type captureReader interface {
	ReadPacketData() ([]byte, gopacket.CaptureInfo, error)
	LinkType() layers.LinkType
}

func isCapture(path string) (bool, error) {
	if path == "" {
		return false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	var magic [4]byte
	if _, err = io.ReadFull(f, magic[:]); err != nil {
		return false, err
	}
	switch binary.LittleEndian.Uint32(magic[:]) {
	case 0xa1b2c3d4, 0xd4c3b2a1, 0xa1b23c4d, 0x4d3cb2a1, 0x0a0d0d0a:
		return true, nil
	}
	return false, nil
}

// readCapture extracts plaintext H264 RTP from Ethernet/VLAN, Linux cooked or
// raw IP captures. UDP/IP headers are never transmitted to the gateway.
func readCapture(path string) ([]*captureStream, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buffer := bufio.NewReader(f)
	magic, err := buffer.Peek(4)
	if err != nil {
		return nil, err
	}
	var reader captureReader
	if bytes.Equal(magic, []byte{10, 13, 13, 10}) {
		reader, err = pcapgo.NewNgReader(buffer, pcapgo.DefaultNgReaderOptions)
	} else {
		reader, err = pcapgo.NewReader(buffer)
	}
	if err != nil {
		return nil, err
	}
	var streams []*captureStream
	byFlow := make(map[string]*captureStream)
	for {
		data, info, err := reader.ReadPacketData()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read capture: %w", err)
		}
		if info.CaptureLength != info.Length {
			return nil, fmt.Errorf("capture contains truncated packets; record with full snaplen")
		}
		packet := gopacket.NewPacket(data, reader.LinkType(), gopacket.DecodeOptions{Lazy: true, NoCopy: true})
		udpLayer := packet.Layer(layers.LayerTypeUDP)
		if udpLayer == nil || packet.NetworkLayer() == nil {
			continue
		}
		udp := udpLayer.(*layers.UDP)
		// RTCP shares RTP version bits but occupies packet types 192..223.
		if len(udp.Payload) < 12 || udp.Payload[1] >= 192 && udp.Payload[1] <= 223 {
			continue
		}
		p := &rtp.Packet{}
		if err := p.Unmarshal(udp.Payload); err != nil || p.Version != 2 || len(p.Payload) == 0 {
			continue
		}
		src := fmt.Sprintf("%s:%d", packet.NetworkLayer().NetworkFlow().Src(), udp.SrcPort)
		dst := fmt.Sprintf("%s:%d", packet.NetworkLayer().NetworkFlow().Dst(), udp.DstPort)
		key := fmt.Sprintf("%s/%s/%d/%d", src, dst, p.SSRC, p.PayloadType)
		s := byFlow[key]
		if s == nil {
			s = &captureStream{Source: src, Destination: dst, SSRC: p.SSRC, PayloadType: p.PayloadType, first: info.Timestamp}
			byFlow[key] = s
			streams = append(streams, s)
		}
		offset := info.Timestamp.Sub(s.first)
		if offset < 0 || len(s.packets) > 0 && offset < s.packets[len(s.packets)-1].Offset {
			return nil, fmt.Errorf("capture timestamps go backwards for SSRC %d", p.SSRC)
		}
		s.noteParameterSets(p.Payload)
		s.packets = append(s.packets, timedPacket{Packet: p.Clone(), Offset: offset})
		s.Packets++
		if p.Marker {
			s.Frames++
		}
		s.Duration = offset.Seconds()
	}
	var h264 []*captureStream
	for _, s := range streams {
		if s.sps && s.pps && s.Frames > 0 {
			s.Index = len(h264)
			h264 = append(h264, s)
		}
	}
	if len(h264) == 0 {
		return nil, fmt.Errorf("no plaintext H264 RTP stream with SPS/PPS found in capture")
	}
	return h264, nil
}

func (s *captureStream) noteParameterSets(payload []byte) {
	if len(payload) == 0 {
		return
	}
	switch payload[0] & 31 {
	case 7:
		s.sps = true
	case 8:
		s.pps = true
	case 24: // STAP-A may contain both parameter sets.
		for rest := payload[1:]; len(rest) >= 2; {
			size := int(binary.BigEndian.Uint16(rest))
			rest = rest[2:]
			if size == 0 || size > len(rest) {
				return
			}
			if rest[0]&31 == 7 {
				s.sps = true
			}
			if rest[0]&31 == 8 {
				s.pps = true
			}
			rest = rest[size:]
		}
	}
}

func capturePackets(path string, index int, duration time.Duration) ([]timedPacket, *captureStream, error) {
	streams, err := readCapture(path)
	if err != nil {
		return nil, nil, err
	}
	if index < 0 || index >= len(streams) {
		return nil, nil, fmt.Errorf("pcap-stream %d outside available streams 0..%d", index, len(streams)-1)
	}
	s := streams[index]
	var packets []timedPacket
	complete := 0
	for _, item := range s.packets {
		if item.Offset >= duration {
			break
		}
		// The lab negotiates PT 96. Keep sequence, SSRC, timestamp, extensions,
		// padding and H264 payload; only remap the dynamic payload type.
		item.Packet.PayloadType = 96
		packets = append(packets, item)
		if item.Packet.Marker {
			complete = len(packets)
		}
	}
	packets = packets[:complete] // Never end replay halfway through a fragmented AU.
	if len(packets) == 0 {
		return nil, nil, fmt.Errorf("no complete H264 frame in selected capture interval")
	}
	return packets, s, nil
}
