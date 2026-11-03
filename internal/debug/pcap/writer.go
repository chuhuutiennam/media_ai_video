package pcap

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcapgo"
	"go.uber.org/zap"
)

// capturedPacket is the unit queued between capture loop and writer goroutine.
type capturedPacket struct {
	ci   gopacket.CaptureInfo
	data []byte
}

// pcapWriter writes captured packets to rotating PCAP files.
// Run must be called in a dedicated goroutine; it exits when queue is closed.
type pcapWriter struct {
	cfg     Config
	prefix  string // "ingress" or "egress"
	queue   <-chan capturedPacket
	packets *atomic.Uint64
	bytes   *atomic.Uint64
	writeErrs *atomic.Uint64

	f       *os.File
	pw      *pcapgo.Writer
	size    int64
	fileNum int
}

func newPcapWriter(
	cfg Config, prefix string, queue <-chan capturedPacket,
	packets, bytes, writeErrs *atomic.Uint64,
) *pcapWriter {
	return &pcapWriter{
		cfg:       cfg,
		prefix:    prefix,
		queue:     queue,
		packets:   packets,
		bytes:     bytes,
		writeErrs: writeErrs,
	}
}

func (w *pcapWriter) Run() {
	if err := w.openFile(); err != nil {
		zap.L().Error("pcap: failed to open initial file", zap.String("prefix", w.prefix), zap.Error(err))
		// Drain queue so sender is never blocked on a closed channel.
		for range w.queue {
		}
		return
	}
	defer w.closeFile()

	for pkt := range w.queue {
		if err := w.writePacket(pkt); err != nil {
			w.writeErrs.Add(1)
			zap.L().Debug("pcap: write error", zap.String("prefix", w.prefix), zap.Error(err))
			continue
		}
		w.packets.Add(1)
		w.bytes.Add(uint64(pkt.ci.CaptureLength))

		maxBytes := int64(w.cfg.Rotate.MaxSizeMB) * 1024 * 1024
		if w.cfg.Rotate.MaxSizeMB > 0 && w.size >= maxBytes {
			w.rotate()
		}
	}
}

func (w *pcapWriter) writePacket(pkt capturedPacket) error {
	if w.pw == nil {
		return fmt.Errorf("no open file")
	}
	err := w.pw.WritePacket(pkt.ci, pkt.data)
	if err == nil {
		// pcapgo doesn't track size; approximate from capture length + header (16 bytes).
		w.size += int64(pkt.ci.CaptureLength) + 16
	}
	return err
}

func (w *pcapWriter) rotate() {
	w.closeFile()
	if err := w.openFile(); err != nil {
		zap.L().Error("pcap: rotate failed", zap.String("prefix", w.prefix), zap.Error(err))
		return
	}
	zap.L().Info("pcap: rotated file", zap.String("prefix", w.prefix), zap.Int("file_num", w.fileNum))
	w.pruneOldFiles()
}

func (w *pcapWriter) openFile() error {
	if err := os.MkdirAll(w.cfg.OutputDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", w.cfg.OutputDir, err)
	}
	w.fileNum++
	name := fmt.Sprintf("%s-%s-%04d.pcap", w.prefix, time.Now().Format("20060102-150405"), w.fileNum)
	path := filepath.Join(w.cfg.OutputDir, name)
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	pw := pcapgo.NewWriter(f)
	if err := pw.WriteFileHeader(w.cfg.snapLen(), layers.LinkTypeEthernet); err != nil {
		f.Close()
		return fmt.Errorf("write pcap header: %w", err)
	}
	w.f = f
	w.pw = pw
	w.size = 24 // global PCAP header size
	zap.L().Info("pcap: opened file", zap.String("path", path))
	return nil
}

func (w *pcapWriter) closeFile() {
	if w.f == nil {
		return
	}
	_ = w.f.Sync()
	_ = w.f.Close()
	w.f = nil
	w.pw = nil
}

// pruneOldFiles deletes the oldest files when MaxFiles is exceeded.
func (w *pcapWriter) pruneOldFiles() {
	if w.cfg.Rotate.MaxFiles <= 0 {
		return
	}
	pattern := filepath.Join(w.cfg.OutputDir, w.prefix+"-*.pcap")
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) <= w.cfg.Rotate.MaxFiles {
		return
	}
	sort.Strings(matches) // lexicographic = chronological (timestamp in name)
	for _, path := range matches[:len(matches)-w.cfg.Rotate.MaxFiles] {
		if err := os.Remove(path); err != nil {
			zap.L().Warn("pcap: prune failed", zap.String("path", path), zap.Error(err))
		} else {
			zap.L().Info("pcap: pruned old file", zap.String("path", path))
		}
	}
}
