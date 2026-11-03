//go:build linux

package pcap

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/afpacket"
	"go.uber.org/zap"
	"golang.org/x/net/bpf"
)

// Recorder captures RTP packets via two AF_PACKET sockets — one per direction —
// and writes them to separate PCAP files via dedicated writer goroutines.
//
// Ingress socket: BPF filter "udp dst portrange PortMin-PortMax"
//   → packets arriving from MF into the gateway's port pool.
//
// Egress socket:  BPF filter "udp src portrange PortMin-PortMax"
//   → packets sent by the gateway back to MF (paced TTS audio).
//
// Both capture loops and both writers run independently; if either queue fills,
// PCAP packets are dropped (drop counter incremented) without affecting the
// RTP data plane.
type Recorder struct {
	cfg Config

	ingressTP *afpacket.TPacket
	egressTP  *afpacket.TPacket

	ingressQ chan capturedPacket
	egressQ  chan capturedPacket

	stopCh chan struct{}
	wg     sync.WaitGroup

	packets   atomic.Uint64
	bytes     atomic.Uint64
	dropped   atomic.Uint64
	writeErrs atomic.Uint64
}

// New creates a Recorder. Call Start() to begin capture.
// Returns an error if AF_PACKET sockets cannot be created (e.g. missing CAP_NET_RAW).
func New(cfg Config) (*Recorder, error) {
	if cfg.PortMin == 0 || cfg.PortMax == 0 || cfg.PortMin > cfg.PortMax {
		return nil, fmt.Errorf("pcap: invalid port range %d-%d", cfg.PortMin, cfg.PortMax)
	}
	if cfg.Interface == "" {
		return nil, fmt.Errorf("pcap: interface must be specified")
	}

	ingressTP, err := newTPacket(cfg.Interface)
	if err != nil {
		return nil, fmt.Errorf("pcap: ingress socket: %w", err)
	}
	if err := ingressTP.SetBPF(udpDstPortRangeBPF(cfg.PortMin, cfg.PortMax)); err != nil {
		ingressTP.Close()
		return nil, fmt.Errorf("pcap: ingress BPF: %w", err)
	}

	egressTP, err := newTPacket(cfg.Interface)
	if err != nil {
		ingressTP.Close()
		return nil, fmt.Errorf("pcap: egress socket: %w", err)
	}
	if err := egressTP.SetBPF(udpSrcPortRangeBPF(cfg.PortMin, cfg.PortMax)); err != nil {
		ingressTP.Close()
		egressTP.Close()
		return nil, fmt.Errorf("pcap: egress BPF: %w", err)
	}

	qs := cfg.queueSize()
	return &Recorder{
		cfg:       cfg,
		ingressTP: ingressTP,
		egressTP:  egressTP,
		ingressQ:  make(chan capturedPacket, qs),
		egressQ:   make(chan capturedPacket, qs),
		stopCh:    make(chan struct{}),
	}, nil
}

// Start launches 4 goroutines: ingress capture, egress capture, ingress writer,
// egress writer. Non-blocking — returns immediately.
func (r *Recorder) Start() {
	iw := newPcapWriter(r.cfg, "ingress", r.ingressQ, &r.packets, &r.bytes, &r.writeErrs)
	ew := newPcapWriter(r.cfg, "egress", r.egressQ, &r.packets, &r.bytes, &r.writeErrs)

	r.wg.Add(4)
	go func() { defer r.wg.Done(); r.captureLoop(r.ingressTP, r.ingressQ) }()
	go func() { defer r.wg.Done(); r.captureLoop(r.egressTP, r.egressQ) }()
	go func() { defer r.wg.Done(); iw.Run() }()
	go func() { defer r.wg.Done(); ew.Run() }()

	zap.L().Info("pcap: capture started",
		zap.String("interface", r.cfg.Interface),
		zap.Uint16("port_min", r.cfg.PortMin),
		zap.Uint16("port_max", r.cfg.PortMax),
		zap.String("output_dir", r.cfg.OutputDir),
	)
}

// Stop signals all goroutines to exit and waits for them to finish.
func (r *Recorder) Stop() {
	close(r.stopCh)
	// Close sockets — unblocks ZeroCopyReadPacketData.
	r.ingressTP.Close()
	r.egressTP.Close()
	// Close queues after capture loops exit so writers drain cleanly.
	r.wg.Wait() // wait for capture loops (they close queues on exit)
	st := r.Stats()
	zap.L().Info("pcap: capture stopped",
		zap.Uint64("packets", st.Packets),
		zap.Uint64("bytes", st.Bytes),
		zap.Uint64("dropped", st.Dropped),
		zap.Uint64("write_errors", st.WriteErrs),
	)
}

// Stats returns a snapshot of capture counters.
func (r *Recorder) Stats() Stats {
	return Stats{
		Packets:   r.packets.Load(),
		Bytes:     r.bytes.Load(),
		Dropped:   r.dropped.Load(),
		WriteErrs: r.writeErrs.Load(),
	}
}

// captureLoop reads packets from tp and enqueues them. Exits when tp is closed
// or stopCh is closed; then closes q so the corresponding writer goroutine exits.
func (r *Recorder) captureLoop(tp *afpacket.TPacket, q chan<- capturedPacket) {
	defer close(q)
	snapLen := int(r.cfg.snapLen())
	for {
		select {
		case <-r.stopCh:
			return
		default:
		}

		data, ci, err := tp.ZeroCopyReadPacketData()
		if err != nil {
			if err == afpacket.ErrTimeout {
				continue
			}
			return
		}

		// ZeroCopyReadPacketData returns a slice into the ring buffer that is
		// valid only until the next call. Copy before enqueuing.
		n := len(data)
		if n > snapLen {
			n = snapLen
		}
		cp := make([]byte, n)
		copy(cp, data[:n])

		pkt := capturedPacket{
			ci: gopacket.CaptureInfo{
				Timestamp:     ci.Timestamp,
				CaptureLength: n,
				Length:        ci.Length,
			},
			data: cp,
		}
		select {
		case q <- pkt:
		default:
			r.dropped.Add(1)
		}
	}
}

// newTPacket creates an AF_PACKET socket on iface with a 8 MiB ring buffer.
func newTPacket(iface string) (*afpacket.TPacket, error) {
	return afpacket.NewTPacket(
		afpacket.OptInterface(iface),
		afpacket.OptFrameSize(65536),
		afpacket.OptBlockSize(1<<23), // 8 MiB
		afpacket.OptNumBlocks(1),
		afpacket.OptPollTimeout(100*time.Millisecond),
	)
}

// udpDstPortRangeBPF returns a BPF program that passes UDP packets whose
// destination port is in [min, max]. Assumes Ethernet + IPv4 (no IP options).
//
// BPF offsets:
//
//	[12–13] Ethernet ethertype
//	[23]    IP protocol
//	[36–37] UDP destination port  (14 Ethernet + 20 IP + 2)
func udpDstPortRangeBPF(min, max uint16) []bpf.RawInstruction {
	return compileBPF(36, min, max)
}

// udpSrcPortRangeBPF returns a BPF program that passes UDP packets whose
// source port is in [min, max]. Assumes Ethernet + IPv4 (no IP options).
//
//	[34–35] UDP source port  (14 Ethernet + 20 IP + 0)
func udpSrcPortRangeBPF(min, max uint16) []bpf.RawInstruction {
	return compileBPF(34, min, max)
}

// compileBPF generates BPF instructions: pass if IPv4 UDP && port at portOff in [min,max].
//
// Instruction layout (0-indexed):
//
//	0  ldh  [12]              — load ethertype
//	1  jeq  0x0800, 0, 5     — not IPv4 → drop (instr 7)
//	2  ldb  [23]              — load IP protocol
//	3  jeq  17, 0, 3         — not UDP → drop (instr 7)
//	4  ldh  [portOff]         — load port
//	5  jge  min, 0, 1        — port < min → drop (instr 7)
//	6  jgt  max, 0, 1        — port > max → drop (instr 7); else pass (instr 8)
//	7  ret  0                 — drop
//	8  ret  65535             — pass
func compileBPF(portOff uint32, min, max uint16) []bpf.RawInstruction {
	insns := []bpf.Instruction{
		bpf.LoadAbsolute{Off: 12, Size: 2},
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: 0x0800, SkipTrue: 0, SkipFalse: 5},
		bpf.LoadAbsolute{Off: 23, Size: 1},
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: 17, SkipTrue: 0, SkipFalse: 3},
		bpf.LoadAbsolute{Off: portOff, Size: 2},
		bpf.JumpIf{Cond: bpf.JumpGreaterOrEqual, Val: uint32(min), SkipTrue: 0, SkipFalse: 1},
		bpf.JumpIf{Cond: bpf.JumpGreaterThan, Val: uint32(max), SkipTrue: 0, SkipFalse: 1},
		bpf.RetConstant{Val: 0},
		bpf.RetConstant{Val: 65535},
	}
	raw, err := bpf.Assemble(insns)
	if err != nil {
		// bpf.Assemble only fails on invalid instruction types — not reachable here.
		panic(fmt.Sprintf("pcap: BPF assemble: %v", err))
	}
	return raw
}
