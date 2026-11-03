// Package pcap captures RTP packets via AF_PACKET and writes them to PCAP files
// for Wireshark analysis. Capture runs in a dedicated goroutine pair (ingress/egress)
// independent of the RTP media path — if the capture queue fills, packets are dropped
// from the PCAP file, not from the media path.
//
// Linux only: AF_PACKET requires CAP_NET_RAW and is not available on other platforms.
package pcap

// Config configures the PCAP recorder.
type Config struct {
	Interface string       // network interface to capture on (e.g. "eth0")
	OutputDir string       // directory for PCAP output files
	PortMin   uint16       // start of RTP port range (from rtp.port_start)
	PortMax   uint16       // end of RTP port range (from rtp.port_end)
	QueueSize int          // bounded capture queue size per direction; drops when full
	SnapLen   uint32       // max bytes per captured packet (0 → 65535)
	Rotate    RotateConfig
}

// RotateConfig controls PCAP file rotation.
type RotateConfig struct {
	MaxSizeMB int // rotate when file exceeds this size; 0 → no rotation
	MaxFiles  int // keep at most this many files per direction; 0 → unlimited
}

// Stats is a snapshot of recorder counters.
type Stats struct {
	Packets   uint64 // total packets written
	Bytes     uint64 // total bytes written (raw packet bytes, excluding PCAP overhead)
	Dropped   uint64 // packets dropped because capture queue was full
	WriteErrs uint64 // PCAP write errors
}

func (c Config) snapLen() uint32 {
	if c.SnapLen == 0 {
		return 65535
	}
	return c.SnapLen
}

func (c Config) queueSize() int {
	if c.QueueSize <= 0 {
		return 8192
	}
	return c.QueueSize
}
