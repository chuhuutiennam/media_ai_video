package video

// Regression test cho các giới hạn và timestamp của decoder native:
//   - FU-A: max_nalu_size phải áp dụng ngay khi ghép từng mảnh (không đợi mảnh cuối).
//   - MaxWidth/MaxHeight: frame vượt giới hạn bị bỏ và đếm ở OversizeFrames, không phân biệt
//     ngang/dọc.
//   - B-frame: timestamp của frame là của chính AU sinh ra nó, không phải AU vừa đẩy vào.

import (
	"context"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

// Mảnh FU-A đầu (IDR) rồi các mảnh giữa, không có mảnh cuối: trước đây fu_buffer_ tăng không
// giới hạn và chỉ bị kiểm tra khi mảnh cuối đến. Nay AU bị bỏ ngay ở mảnh làm vượt max_nalu_size.
func TestDecoder_FuALimitAppliedPerFragment(t *testing.T) {
	dec, err := NewDecoder(Config{MaxNALUSize: 64})
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()

	frag := make([]byte, 2+30)
	frag[0] = 0x7C     // FU indicator: NRI=3, type=28 (FU-A)
	frag[1] = 0x80 | 5 // FU header: S=1, NAL type 5 (IDR)
	seq := uint16(1000)
	push := func() {
		t.Helper()
		if _, err := dec.PushRTP(seq, 9000, false, frag); err != nil {
			t.Fatal(err)
		}
		seq++
	}
	push()      // 31 byte
	frag[1] = 5 // mảnh giữa: S=0, E=0
	push()      // 61 byte, vẫn trong giới hạn
	if got := dec.Stats().CorruptedAccessUnits; got != 0 {
		t.Fatalf("trong giới hạn mà đã bỏ AU: corrupted=%d", got)
	}
	push() // 91 byte > 64: phải bỏ ngay, không đợi mảnh cuối
	if got := dec.Stats().CorruptedAccessUnits; got == 0 {
		t.Fatal("max_nalu_size chưa được áp dụng khi ghép mảnh giữa (chỉ kiểm tra ở mảnh cuối)")
	}

	// Mảnh đầu đã vượt giới hạn cũng bị bỏ ngay.
	big := make([]byte, 2+100)
	big[0], big[1] = 0x7C, 0x80|5
	before := dec.Stats().CorruptedAccessUnits
	if _, err := dec.PushRTP(seq, 18000, false, big); err != nil {
		t.Fatal(err)
	}
	if dec.Stats().CorruptedAccessUnits == before {
		t.Fatal("mảnh FU-A đầu vượt max_nalu_size không bị bỏ")
	}
}

func decodeAll(t *testing.T, dec *Decoder, groups [][]*rtp.Packet) []*Frame {
	t.Helper()
	var frames []*Frame
	for _, p := range flattenPackets(groups) {
		if _, err := dec.PushRTP(p.SequenceNumber, p.Timestamp, p.Marker, p.Payload); err != nil {
			t.Fatal(err)
		}
		for {
			f, ok := dec.ReceiveFrame()
			if !ok {
				break
			}
			frames = append(frames, f)
		}
	}
	return frames
}

func TestDecoder_MaxDimensions(t *testing.T) {
	requireFFmpeg(t)
	cases := []struct {
		name               string
		w, h, maxW, maxH   int
		wantFrames         bool
		wantOversizeFrames bool
	}{
		// libavcodec từ chối qua max_pixels (160x120 vượt xa 32x32 kể cả sau khi nới lên bội 64).
		{"160x120 vs 32x32", 160, 120, 32, 32, false, true},
		// Lọt max_pixels (giới hạn diện tích có nới) nhưng cạnh ngắn 64 > 48: bỏ sau decode.
		{"64x64 vs 64x48", 64, 64, 64, 48, false, true},
		{"đúng bằng giới hạn", 64, 48, 64, 48, true, false},
		// Stream dọc vừa khung giới hạn ngang: không bị bỏ.
		{"dọc 48x64 vs 64x48", 48, 64, 64, 48, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			groups := genH264RTPFrameGroups(t, tc.w, tc.h, 10, 1)
			dec, err := NewDecoder(Config{MaxWidth: tc.maxW, MaxHeight: tc.maxH})
			if err != nil {
				t.Fatal(err)
			}
			defer dec.Close()
			frames := decodeAll(t, dec, groups)
			st := dec.Stats()
			if (len(frames) > 0) != tc.wantFrames || (st.OversizeFrames > 0) != tc.wantOversizeFrames {
				t.Fatalf("frames=%d oversize=%d, want frames=%v oversize=%v", len(frames), st.OversizeFrames, tc.wantFrames, tc.wantOversizeFrames)
			}
			for _, f := range frames {
				if f.Width != tc.w || f.Height != tc.h {
					t.Fatalf("frame %dx%d, want %dx%d", f.Width, f.Height, tc.w, tc.h)
				}
			}
			if tc.wantOversizeFrames {
				if st.DecodeErrors != 0 {
					t.Fatalf("frame quá lớn bị tính vào DecodeErrors=%d", st.DecodeErrors)
				}
				if st.OversizeFrames != uint64(len(groups)) {
					t.Fatalf("OversizeFrames=%d, want %d (mỗi AU một frame)", st.OversizeFrames, len(groups))
				}
			}
		})
	}
}

// Stream main profile có B-frame: RTP ts theo thứ tự decode không tăng đều (I P B B ...).
// Frame ra theo thứ tự hiển thị phải mang đúng ts của chính nó và tăng dần.
func TestDecoder_BFrameTimestampsFollowFrames(t *testing.T) {
	requireFFmpeg(t)
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not found on PATH — cần để lấy pts của fixture, skip")
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	run := func(name string, args ...string) []byte {
		t.Helper()
		out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", name, err, out)
		}
		return out
	}
	mp4, raw := dir+"/b.mp4", dir+"/b.h264"
	run("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=duration=2:size=64x64:rate=10",
		"-threads", "1", "-c:v", "libx264", "-profile:v", "main", "-bf", "2",
		"-x264-params", "b-adapt=0:b-pyramid=none:scenecut=0:keyint=100", "-pix_fmt", "yuv420p", mp4)
	run("ffmpeg", "-hide_banner", "-loglevel", "error", "-i", mp4, "-c", "copy", "-bsf:v", "h264_mp4toannexb", "-f", "h264", raw)
	// pts theo thứ tự packet (= thứ tự decode) → RTP ts của từng AU (RTP ts = thời điểm hiển thị).
	var pts []uint32
	probe := run("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "packet=pts_time", "-of", "csv=p=0", mp4)
	for _, line := range strings.Fields(string(probe)) {
		v, err := strconv.ParseFloat(strings.TrimSuffix(line, ","), 64)
		if err != nil {
			t.Fatal(err)
		}
		pts = append(pts, uint32(90000+v*90000+0.5))
	}
	data, err := os.ReadFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	aus := splitAnnexBIntoFrames(data)
	if len(aus) != len(pts) {
		t.Fatalf("AU=%d nhưng pts=%d", len(aus), len(pts))
	}
	if sort.SliceIsSorted(pts, func(i, j int) bool { return pts[i] < pts[j] }) {
		t.Fatal("fixture không có B-frame (pts theo thứ tự decode đã tăng dần)")
	}

	packetizer := rtp.NewPacketizer(1200, 96, 0xC0FFEE, &codecs.H264Payloader{}, rtp.NewFixedSequencer(1), 90000)
	dec, err := NewDecoder(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	var got []*Frame
	for i, nals := range aus {
		for _, p := range packetizer.Packetize(annexBEncode(nals), 0) {
			if _, err := dec.PushRTP(p.SequenceNumber, pts[i], p.Marker, p.Payload); err != nil {
				t.Fatal(err)
			}
			for {
				f, ok := dec.ReceiveFrame()
				if !ok {
					break
				}
				got = append(got, f)
			}
		}
	}
	want := append([]uint32(nil), pts...)
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	if len(got) < len(want)/2 {
		t.Fatalf("decode được quá ít frame: %d/%d", len(got), len(want))
	}
	for i, f := range got {
		if f.RTPTimestamp != want[i] || f.ExtendedRTPTimestamp != uint64(want[i]) {
			t.Fatalf("frame %d: rtp_ts=%d ext=%d, want %d", i, f.RTPTimestamp, f.ExtendedRTPTimestamp, want[i])
		}
		if wantMs := int64(want[i]-want[0]) * 1000 / 90000; f.TimestampMs != wantMs {
			t.Fatalf("frame %d: timestamp_ms=%d, want %d", i, f.TimestampMs, wantMs)
		}
		if i > 0 && f.TimestampMs <= got[i-1].TimestampMs {
			t.Fatalf("timestamp không tăng dần: frame %d=%d ms sau %d ms", i, f.TimestampMs, got[i-1].TimestampMs)
		}
	}
}
