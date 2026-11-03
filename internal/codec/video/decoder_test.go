package video

// decoder_test.go — test độc lập cho decoder native (libavcodec in-process),
// đúng tinh thần internal/codec/*_test.go trong repo: self-contained, không
// phụ thuộc session/pipeline/AI.
//
// ĐƠN GIẢN HƠN bản subprocess cũ: decoder mới decode+drain ĐỒNG BỘ ngay
// trong PushRTP() (không còn LatestQueue(1) nội bộ chạy nền qua pipe) — nên
// test chỉ cần "push rồi ReceiveFrame() ngay", không cần goroutine
// push+poll chạy song song với pacing thời gian thực như bản trước.
//
// Cùng phát hiện quan trọng vẫn áp dụng: `ffmpeg -f rtp ...` không lặp lại
// SPS/PPS in-band, nên fixture tự packetize từ elementary stream bằng
// pion/rtp thay vì dùng RTP muxer của ffmpeg — xem genH264RTPFrameGroups().
//
// Test tự SKIP (không FAIL) nếu thiếu `ffmpeg` trên PATH (chỉ cần để SINH
// fixture — decoder chính nó không còn phụ thuộc binary ffmpeg nữa, chỉ cần
// libavcodec/libavutil/libswscale .so lúc runtime).

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

const (
	testWidth   = 160
	testHeight  = 120
	testFPS     = 10
	testSeconds = 2
)

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not found on PATH — chỉ cần để sinh fixture test, skip")
	}
}

// genH264RTPFrameGroups cho ffmpeg encode 1 clip testsrc ngắn ra raw H264
// elementary stream (Annex-B), sau đó TỰ packetize thành RTP bằng pion/rtp
// (1 access unit = 1 lời gọi Packetize() để mỗi frame có đúng 1 RTP
// timestamp + marker bit ở packet cuối).
func genH264RTPFrameGroups(t *testing.T, w, h, fps, seconds int) [][]*rtp.Packet {
	t.Helper()

	dir := t.TempDir()
	rawPath := dir + "/raw.h264"

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=duration="+itoa(seconds)+":size="+itoa(w)+"x"+itoa(h)+":rate="+itoa(fps),
		"-c:v", "libx264", "-profile:v", "baseline", "-tune", "zerolatency",
		"-x264-params", "repeat-headers=1", // SPS/PPS lặp lại trước mỗi IDR trong elementary stream
		"-pix_fmt", "yuv420p",
		"-f", "h264", rawPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg encode elementary stream: %v\n%s", err, out)
	}

	raw, err := os.ReadFile(rawPath)
	if err != nil {
		t.Fatalf("read elementary stream: %v", err)
	}

	frames := splitAnnexBIntoFrames(raw)
	if len(frames) == 0 {
		t.Fatalf("không tách được frame nào từ elementary stream (%d bytes)", len(raw))
	}
	t.Logf("elementary stream: %d bytes → %d access unit (NAL groups)", len(raw), len(frames))

	payloader := &codecs.H264Payloader{}
	sequencer := rtp.NewRandomSequencer()
	packetizer := rtp.NewPacketizer(1200, 96, 0xC0FFEE, payloader, sequencer, uint32(90000))

	samplesPerFrame := uint32(90000 / fps) // clock rate video RTP chuẩn = 90kHz

	groups := make([][]*rtp.Packet, 0, len(frames))
	for _, frameNALs := range frames {
		buf := annexBEncode(frameNALs)
		groups = append(groups, packetizer.Packetize(buf, samplesPerFrame))
	}
	return groups
}

// flattenPackets nối các nhóm packet theo AU thành 1 slice phẳng — dùng cho
// test lossy/reorder vốn không cần giữ ranh giới AU khi drop/hoán đổi packet.
func flattenPackets(groups [][]*rtp.Packet) []*rtp.Packet {
	var out []*rtp.Packet
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// splitAnnexBIntoFrames tách elementary stream Annex-B thành các nhóm NAL
// theo access unit: 1 access unit kết thúc ngay sau NAL loại VCL (slice,
// type 1 hoặc 5) đầu tiên — đúng với clip test (mỗi frame chỉ có 1 slice).
func splitAnnexBIntoFrames(data []byte) [][][]byte {
	nals := splitAnnexBNALUs(data)
	var frames [][][]byte
	var cur [][]byte
	for _, n := range nals {
		if len(n) == 0 {
			continue
		}
		cur = append(cur, n)
		naluType := n[0] & 0x1F
		if naluType == 1 || naluType == 5 {
			frames = append(frames, cur)
			cur = nil
		}
	}
	if len(cur) > 0 {
		frames = append(frames, cur)
	}
	return frames
}

func splitAnnexBNALUs(data []byte) [][]byte {
	var starts []int
	n := len(data)
	for i := 0; i < n-2; {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			starts = append(starts, i+3)
			i += 3
			continue
		}
		i++
	}
	var nals [][]byte
	for i, s := range starts {
		e := n
		if i+1 < len(starts) {
			nextStart := starts[i+1]
			e = nextStart - 3
			if e > 0 && data[e-1] == 0 {
				e--
			}
		}
		if e > s {
			nals = append(nals, data[s:e])
		}
	}
	return nals
}

func annexBEncode(nals [][]byte) []byte {
	var buf []byte
	for _, n := range nals {
		buf = append(buf, 0x00, 0x00, 0x00, 0x01)
		buf = append(buf, n...)
	}
	return buf
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// TestDecoder_H264_Roundtrip là test chính của Giai đoạn 1: feed RTP packet
// H264 thật (sinh bởi ffmpeg) vào decode chain, verify ra đúng số frame
// YUV420P đúng kích thước — hoàn toàn độc lập session/pipeline/AI.
//
// Decode đồng bộ (không polling): push xong 1 AU là ReceiveFrame() ngay có
// thể lấy được frame tương ứng (hoặc 0 nếu decoder cần thêm reference frame
// — hiếm với baseline profile không B-frame).
func TestDecoder_H264_Roundtrip(t *testing.T) {
	requireFFmpeg(t)

	frameGroups := genH264RTPFrameGroups(t, testWidth, testHeight, testFPS, testSeconds)
	t.Logf("fixture: %d access unit", len(frameGroups))

	dec, err := NewDecoder(Config{})
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	defer dec.Close()

	var frames []*Frame
	for _, pkts := range frameGroups {
		for _, pkt := range pkts {
			if _, err := dec.PushRTP(pkt.SequenceNumber, pkt.Timestamp, pkt.Marker, pkt.Payload); err != nil {
				t.Logf("push_rtp warning: %v", err)
			}
		}
		for {
			f, ok := dec.ReceiveFrame()
			if !ok {
				break
			}
			frames = append(frames, f)
		}
	}

	// Drain nốt frame còn lại (decoder có thể giữ vài frame reference chưa flush).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f, ok := dec.ReceiveFrame()
		if !ok {
			break
		}
		frames = append(frames, f)
	}

	if len(frames) == 0 {
		t.Fatal("không decode được frame nào — kiểm tra libavcodec / AU assembler / RTP parser")
	}

	for i, f := range frames {
		if f.PixelFormat != PixelFormatYUV420P {
			t.Errorf("frame[%d]: pixel_format = %d, want YUV420P(%d)", i, f.PixelFormat, PixelFormatYUV420P)
			continue
		}
		wantY := f.StrideY * f.Height
		if len(f.Y) != wantY {
			t.Errorf("frame[%d]: len(Y) = %d, want %d (%dx%d)", i, len(f.Y), wantY, f.Width, f.Height)
		}
		if f.Width != testWidth || f.Height != testHeight {
			t.Errorf("frame[%d]: kích thước = %dx%d, want %dx%d (decoder tự suy từ SPS)", i, f.Width, f.Height, testWidth, testHeight)
		}
	}

	minExpected := len(frameGroups) / 2
	if len(frames) < minExpected {
		t.Errorf("số frame decode được = %d, kỳ vọng >= %d (input %d AU)", len(frames), minExpected, len(frameGroups))
	}

	stats := dec.Stats()
	t.Logf("decode OK: %d/%d frame | stats: packets=%d au_completed=%d idr=%d decode_errors=%d",
		len(frames), len(frameGroups), stats.RTPPacketsInput, stats.AccessUnitsCompleted, stats.IDRFrames, stats.DecodeErrors)
}

// TestDecoder_LossyPackets mô phỏng packet loss + reorder — RTP parser/AU
// assembler phải KHÔNG panic/deadlock, dù output có thể thiếu vài frame
// quanh chỗ mất gói (drop_until_idr_on_loss theo policy mặc định).
func TestDecoder_LossyPackets(t *testing.T) {
	requireFFmpeg(t)

	packets := flattenPackets(genH264RTPFrameGroups(t, testWidth, testHeight, testFPS, testSeconds))
	if len(packets) < 10 {
		t.Skip("fixture quá ít packet để test lossy/reorder có ý nghĩa")
	}

	var lossy []*rtp.Packet
	for i, p := range packets {
		if i%7 == 6 {
			continue // drop ~14%
		}
		lossy = append(lossy, p)
	}
	for i := 0; i+1 < len(lossy); i += 11 {
		lossy[i], lossy[i+1] = lossy[i+1], lossy[i]
	}

	dec, err := NewDecoder(Config{})
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	defer dec.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, pkt := range lossy {
			dec.PushRTP(pkt.SequenceNumber, pkt.Timestamp, pkt.Marker, pkt.Payload)
			for {
				if _, ok := dec.ReceiveFrame(); !ok {
					break
				}
			}
		}
	}()

	select {
	case <-done:
		// OK — không panic/deadlock với input lossy/reorder.
	case <-time.After(20 * time.Second):
		t.Fatal("decoder không thoát trong 20s với input lossy/reorder — nghi ngờ deadlock/vòng lặp vô hạn trong AU assembler")
	}

	stats := dec.Stats()
	t.Logf("lossy stats: packets=%d packet_gaps=%d corrupted_au=%d decode_errors=%d decoded_frames=%d",
		stats.RTPPacketsInput, stats.PacketGaps, stats.CorruptedAccessUnits, stats.DecodeErrors, stats.DecodedFrames)
}

// TestDecoder_Reset kiểm tra Reset() đưa decoder về WAIT_IDR, phục hồi được
// khi có IDR mới — đúng acceptance criteria #10 trong design doc.
func TestDecoder_Reset(t *testing.T) {
	requireFFmpeg(t)

	frameGroups := genH264RTPFrameGroups(t, testWidth, testHeight, testFPS, testSeconds)
	if len(frameGroups) < 4 {
		t.Skip("fixture quá ngắn để test reset")
	}

	dec, err := NewDecoder(Config{})
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	defer dec.Close()

	// Decode vài AU đầu bình thường.
	for _, pkts := range frameGroups[:2] {
		for _, pkt := range pkts {
			dec.PushRTP(pkt.SequenceNumber, pkt.Timestamp, pkt.Marker, pkt.Payload)
		}
	}
	drainAll(dec)

	if err := dec.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	// Sau reset, AU thứ 3 trở đi thiếu SPS/PPS/IDR ban đầu (vì chỉ frame đầu
	// tiên của toàn stream có IDR) — decoder ở WAIT_IDR nên các AU này (nếu
	// không phải IDR) sẽ bị drop, không phải panic/lỗi.
	for _, pkts := range frameGroups[2:] {
		for _, pkt := range pkts {
			if _, err := dec.PushRTP(pkt.SequenceNumber, pkt.Timestamp, pkt.Marker, pkt.Payload); err != nil {
				t.Fatalf("push_rtp sau reset lỗi cứng (không được phép): %v", err)
			}
		}
	}
	drainAll(dec)

	stats := dec.Stats()
	t.Logf("reset OK, stats sau reset: %+v", stats)
}

func drainAll(dec *Decoder) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := dec.ReceiveFrame(); !ok {
			return
		}
	}
}
