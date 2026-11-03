// Command mock-rtp-sender gửi RTP UDP từ encoded file để test transcode pipeline.
//
// Hỗ trợ file format:
//   raw   (default): đọc --frame-size bytes/frame, gửi thẳng làm RTP payload
//   amrwb          : đọc AMR-WB file (#!AMR-WB\n + frames), wrap thành RFC 4867 octet-aligned
//   amrnb          : đọc AMR-NB file (#!AMR\n   + frames), wrap thành RFC 4867 octet-aligned
//
// Usage:
//
//	mock-rtp-sender --codec PCMU  --pt 0  --ssrc 12345 --ptime 20 --sample-rate 8000 \
//	  --target 127.0.0.1:40000 --file speech.pcmu
//
//	mock-rtp-sender --codec AMR-WB --pt 98 --ssrc 60003 --ptime 20 --sample-rate 16000 \
//	  --file-format amrwb --target 127.0.0.1:40002 --file speech.amr
package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	pionrtp "github.com/pion/rtp"
)

// ── AMR frame size tables (RFC 4867 Tables 1 & 2, octet-aligned) ─────────────

var amrNBFrameBytes = [16]int{12, 13, 15, 17, 19, 20, 26, 31, 5, 0, 0, 0, 0, 0, 0, 0}
var amrWBFrameBytes = [16]int{17, 23, 32, 36, 40, 46, 50, 58, 60, 5, 0, 0, 0, 0, 0, 0}

// ── fileReader abstraction ────────────────────────────────────────────────────

// frameReader đọc frame từ file và trả về RTP payload bytes.
type frameReader interface {
	// readFrame trả về RTP payload của frame tiếp theo.
	// (nil, io.EOF) = hết file; (nil, nil) = frame rỗng, bỏ qua; (data, nil) = gửi.
	readFrame() ([]byte, error)
	// rewind về đầu vùng data (sau magic header).
	rewind() error
}

// ── rawReader: đọc fixed-size chunks ─────────────────────────────────────────

type rawReader struct {
	f   *os.File
	buf []byte
}

func newRawReader(f *os.File, frameSize int) *rawReader {
	return &rawReader{f: f, buf: make([]byte, frameSize)}
}

func (r *rawReader) readFrame() ([]byte, error) {
	nr, err := io.ReadFull(r.f, r.buf)
	if err != nil {
		return nil, err // io.EOF hoặc io.ErrUnexpectedEOF → caller xử lý
	}
	out := make([]byte, nr)
	copy(out, r.buf[:nr])
	return out, nil
}

func (r *rawReader) rewind() error {
	_, err := r.f.Seek(0, io.SeekStart)
	return err
}

// ── amrReader: đọc AMR-NB / AMR-WB file ──────────────────────────────────────

// AMR file format (3GPP TS 26.243 / TS 26.244):
//   magic header: "#!AMR-WB\n" (9 bytes) hoặc "#!AMR\n" (6 bytes)
//   mỗi frame:    [1-byte header: 0 | FT[6:3] | Q[2] | 0 | 0] [N bytes data]
//
// RFC 4867 octet-aligned RTP payload (1 frame/packet):
//   [CMR=0xF0] [TOC = header & 0x7F (F-bit=0)] [N bytes data]

type amrReader struct {
	f          *os.File
	frameTable [16]int
	dataStart  int64 // offset sau magic header
}

func newAMRReader(f *os.File, variant string) (*amrReader, error) {
	var magic []byte
	var ft [16]int

	switch strings.ToLower(variant) {
	case "amrwb":
		magic = []byte("#!AMR-WB\n")
		ft = amrWBFrameBytes
	case "amrnb":
		magic = []byte("#!AMR\n")
		ft = amrNBFrameBytes
	default:
		return nil, fmt.Errorf("amrReader: variant không hợp lệ %q (amrwb|amrnb)", variant)
	}

	// Kiểm tra và skip magic header.
	hdr := make([]byte, len(magic))
	if _, err := io.ReadFull(f, hdr); err != nil {
		return nil, fmt.Errorf("đọc AMR magic header thất bại: %w", err)
	}
	for i, b := range magic {
		if hdr[i] != b {
			return nil, fmt.Errorf("AMR magic header không khớp: %v (expect %v)", hdr, magic)
		}
	}

	pos, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	return &amrReader{f: f, frameTable: ft, dataStart: pos}, nil
}

func (r *amrReader) readFrame() ([]byte, error) {
	var hdr [1]byte
	if _, err := io.ReadFull(r.f, hdr[:]); err != nil {
		return nil, err // io.EOF khi hết file
	}
	ft := (hdr[0] >> 3) & 0x0F
	n := r.frameTable[ft]

	var data []byte
	if n > 0 {
		data = make([]byte, n)
		if _, err := io.ReadFull(r.f, data); err != nil {
			return nil, fmt.Errorf("đọc AMR frame data thất bại (FT=%d n=%d): %w", ft, n, err)
		}
	}

	if n == 0 {
		// FT=NO_DATA hoặc SPEECH_LOST — bỏ qua frame này.
		return nil, nil
	}

	// Xây dựng RFC 4867 octet-aligned RTP payload:
	//   byte 0: CMR = 0xF0 (no codec change request)
	//   byte 1: TOC = header byte với F-bit=0 (chỉ 1 frame/packet)
	//   byte 2+: frame data
	payload := make([]byte, 2+n)
	payload[0] = 0xF0          // CMR
	payload[1] = hdr[0] & 0x7F // TOC (F=0, FT, Q)
	copy(payload[2:], data)
	return payload, nil
}

func (r *amrReader) rewind() error {
	_, err := r.f.Seek(r.dataStart, io.SeekStart)
	return err
}

// ── main ──────────────────────────────────────────────────────────────────────

func main() {
	codec      := flag.String("codec",       "",      "codec name (PCMU/PCMA/opus/AMR-WB/AMR/EVS/...)")
	pt         := flag.Uint("pt",            0,       "RTP payload type (0–127)")
	ssrc       := flag.Uint("ssrc",          12345,   "SSRC")
	seqStart   := flag.Uint("seq",           0,       "sequence number start")
	tsStart    := flag.Uint("ts",            0,       "timestamp start")
	ptimeMs    := flag.Int("ptime",          20,      "packet time in ms")
	sampleRate := flag.Int("sample-rate",    8000,    "sample rate Hz (cho timestamp increment)")
	target     := flag.String("target",      "",      "destination ip:port (bắt buộc)")
	filePath   := flag.String("file",        "",      "input encoded audio file (bắt buộc)")
	fileFormat := flag.String("file-format", "",      "raw|amrwb|amrnb (auto-detect từ --codec nếu bỏ trống)")
	frameSzFlag:= flag.Int("frame-size",     0,       "bytes per RTP payload cho raw format (0=auto theo codec+ptime)")
	count      := flag.Int("count",          0,       "số packet tối đa (0=đến hết file)")
	loop       := flag.Bool("loop",          false,   "lặp lại file khi hết EOF")
	logLevel   := flag.String("log-level",   "info",  "debug|info|warn|error")
	flag.Parse()

	zapLevel := zapcore.InfoLevel
	switch *logLevel {
	case "debug":
		zapLevel = zapcore.DebugLevel
	case "warn":
		zapLevel = zapcore.WarnLevel
	case "error":
		zapLevel = zapcore.ErrorLevel
	}
	zapCfg := zap.NewProductionConfig()
	zapCfg.Level = zap.NewAtomicLevelAt(zapLevel)
	zapCfg.EncoderConfig.EncodeTime = zapcore.TimeEncoderOfLayout("2006-01-02T15:04:05.000000Z07:00")
	logger, err := zapCfg.Build()
	if err != nil {
		panic(err)
	}
	defer logger.Sync() //nolint:errcheck
	zap.ReplaceGlobals(logger)

	if *target == "" {
		zap.L().Error("--target là bắt buộc")
		os.Exit(1)
	}
	if *filePath == "" {
		zap.L().Error("--file là bắt buộc")
		os.Exit(1)
	}
	if *pt > 127 {
		zap.L().Error("--pt phải trong khoảng 0–127")
		os.Exit(1)
	}

	// Tự phát hiện file format từ codec nếu không chỉ định.
	fileFmt := resolveFileFormat(*fileFormat, *codec)

	f, err := os.Open(*filePath)
	if err != nil {
		zap.L().Error("mở file thất bại", zap.String("file", *filePath), zap.Error(err))
		os.Exit(1)
	}
	defer f.Close()

	// Khởi tạo frameReader theo format.
	var reader frameReader
	switch fileFmt {
	case "amrwb", "amrnb":
		r, rErr := newAMRReader(f, fileFmt)
		if rErr != nil {
			zap.L().Error("khởi tạo AMR reader thất bại", zap.Error(rErr))
			os.Exit(1)
		}
		reader = r
	default: // "raw"
		fs := *frameSzFlag
		if fs == 0 {
			fs, err = autoFrameSize(*codec, *sampleRate, *ptimeMs)
			if err != nil {
				zap.L().Error("không xác định được frame size", zap.Error(err))
				os.Exit(1)
			}
		}
		if fs <= 0 {
			zap.L().Error("frame size phải > 0", zap.Int("frame_size", fs))
			os.Exit(1)
		}
		reader = newRawReader(f, fs)
	}

	tsIncr := uint32(*sampleRate * *ptimeMs / 1000) //nolint:gosec

	conn, err := net.Dial("udp", *target)
	if err != nil {
		zap.L().Error("dial UDP thất bại", zap.String("target", *target), zap.Error(err))
		os.Exit(1)
	}
	defer conn.Close()

	zap.L().Info("mock-rtp-sender khởi động",
		zap.String("codec", *codec),
		zap.String("file_format", fileFmt),
		zap.Uint("pt", *pt),
		zap.Uint("ssrc", *ssrc),
		zap.Int("ptime_ms", *ptimeMs),
		zap.Uint32("ts_incr", tsIncr),
		zap.String("target", *target),
		zap.String("file", *filePath),
		zap.Bool("loop", *loop),
	)

	seq      := uint16(*seqStart) //nolint:gosec
	ts       := uint32(*tsStart)  //nolint:gosec
	sent     := 0
	skipped  := 0
	interval := time.Duration(*ptimeMs) * time.Millisecond
	next     := time.Now()

	for {
		if *count > 0 && sent >= *count {
			break
		}

		payload, readErr := reader.readFrame()
		if readErr != nil {
			if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
				if *loop {
					if rewErr := reader.rewind(); rewErr != nil {
						zap.L().Error("rewind thất bại", zap.Error(rewErr))
						break
					}
					continue
				}
				break // hết file
			}
			zap.L().Error("đọc frame thất bại", zap.Error(readErr))
			break
		}
		if payload == nil {
			// Frame rỗng (NO_DATA/SPEECH_LOST) — tăng timestamp không gửi.
			skipped++
			ts += tsIncr
			next = next.Add(interval)
			if sleep := time.Until(next); sleep > 0 {
				time.Sleep(sleep)
			}
			continue
		}

		pkt := &pionrtp.Packet{
			Header: pionrtp.Header{
				Version:        2,
				PayloadType:    uint8(*pt), //nolint:gosec
				SequenceNumber: seq,
				Timestamp:      ts,
				SSRC:           uint32(*ssrc), //nolint:gosec
			},
			Payload: payload,
		}

		raw, marshalErr := pkt.Marshal()
		if marshalErr != nil {
			zap.L().Error("marshal RTP thất bại", zap.Error(marshalErr))
			break
		}

		if _, writeErr := conn.Write(raw); writeErr != nil {
			zap.L().Error("gửi UDP thất bại", zap.Error(writeErr))
			break
		}

		seq++
		ts += tsIncr
		sent++

		if sent%50 == 0 {
			zap.L().Info("tiến độ", zap.Int("sent", sent), zap.Uint16("seq", seq-1), zap.Uint32("ts", ts-tsIncr))
		}

		next = next.Add(interval)
		if sleep := time.Until(next); sleep > 0 {
			time.Sleep(sleep)
		}
	}

	zap.L().Info("hoàn thành", zap.Int("packets_sent", sent), zap.Int("frames_skipped", skipped))
}

// resolveFileFormat xác định file format từ --file-format và --codec.
func resolveFileFormat(explicit, codec string) string {
	if explicit != "" {
		return strings.ToLower(explicit)
	}
	switch strings.ToUpper(strings.NewReplacer("-", "", "_", "").Replace(codec)) {
	case "AMRWB":
		return "amrwb"
	case "AMR", "AMRNB":
		return "amrnb"
	default:
		return "raw"
	}
}

// autoFrameSize tính frame size cho raw format từ codec và ptime.
func autoFrameSize(codec string, sampleRate, ptimeMs int) (int, error) {
	switch strings.ToUpper(strings.NewReplacer("-", "", "_", "").Replace(codec)) {
	case "PCMU", "PCMA":
		return sampleRate * ptimeMs / 1000, nil
	default:
		return 0, fmt.Errorf("codec %q chưa có auto frame-size cho raw format, dùng --frame-size", codec)
	}
}
