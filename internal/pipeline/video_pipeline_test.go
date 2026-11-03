package pipeline

// unit test cho 2 hành vi
//  1. enqueueFrame(): chính sách "drop oldest, giữ newest" khi VideoFrameQueue
//     đầy (§21) — video_e2e_test.go dùng queue đủ lớn (không bao giờ đầy)
//     nên KHÔNG bao giờ chạm nhánh evict này.
//  2. resizeYUV420P(): video_e2e_test.go/decoder_test.go luôn dùng
//     InferWidth/Height == kích thước decode gốc (64x64) nên nhánh resize
//     (dw != sw) chưa từng chạy qua bất kỳ test nào trước bản sửa này.

import (
	"bytes"
	"image/jpeg"
	"testing"

	"media-ai-gateway/internal/codec/video"
)

func TestEnqueueFrame_DropsOldestNotNewest(t *testing.T) {
	frameOut := make(chan VideoFrame, 2)
	sp := &sessionVideoPipeline{
		cfg:      VideoSessionConfig{SessionID: "s1"},
		frameOut: frameOut,
	}

	sp.enqueueFrame(VideoFrame{FrameID: 1})
	sp.enqueueFrame(VideoFrame{FrameID: 2})
	// Queue (cap=2) giờ đầy với [1,2]. Frame 3 phải làm frame 1 (cũ nhất) bị
	// evict, KHÔNG phải bị chính frame 3 (mới nhất) bị drop.
	sp.enqueueFrame(VideoFrame{FrameID: 3})

	close(frameOut)
	var got []int64
	for f := range frameOut {
		got = append(got, f.FrameID)
	}

	if len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Fatalf("sau khi queue đầy + 1 frame mới: got frame_id %v, want [2 3] (drop frame CŨ NHẤT=1, giữ 2 frame mới nhất theo đúng §21 — không phải drop frame mới nhất)", got)
	}

	if sp.framesDropped.Load() != 1 {
		t.Errorf("framesDropped = %d, want 1", sp.framesDropped.Load())
	}
	if sp.framesOut.Load() != 3 {
		t.Errorf("framesOut = %d, want 3 (đếm cả frame cuối cùng enqueue thành công sau evict)", sp.framesOut.Load())
	}
}

func TestEnqueueFrame_NoDropWhenQueueHasRoom(t *testing.T) {
	frameOut := make(chan VideoFrame, 4)
	sp := &sessionVideoPipeline{cfg: VideoSessionConfig{SessionID: "s1"}, frameOut: frameOut}

	sp.enqueueFrame(VideoFrame{FrameID: 1})
	sp.enqueueFrame(VideoFrame{FrameID: 2})

	if sp.framesDropped.Load() != 0 {
		t.Errorf("queue chưa đầy — framesDropped phải = 0, got %d", sp.framesDropped.Load())
	}
	if len(frameOut) != 2 {
		t.Errorf("len(frameOut) = %d, want 2", len(frameOut))
	}
}

// TestResizeYUV420P_DownscalesCorrectly verify resizeYUV420P() (nhánh CHƯA
// từng được test nào chạm tới trước bản sửa này) tạo đúng kích thước output
// và giữ đúng subsampling 4:2:0, kèm spot-check giá trị pixel qua
// nearest-neighbor mapping.
func TestResizeYUV420P_DownscalesCorrectly(t *testing.T) {
	// Frame nguồn 4x4, plane Y là gradient đơn giản để spot-check nearest-neighbor.
	src := &video.Frame{
		Width: 4, Height: 4,
		PixelFormat: video.PixelFormatYUV420P,
		StrideY:     4,
		Y: []byte{
			0, 1, 2, 3,
			4, 5, 6, 7,
			8, 9, 10, 11,
			12, 13, 14, 15,
		},
		StrideCb: 2,
		Cb:       []byte{100, 101, 102, 103}, // 2x2 (4:2:0 của 4x4)
		Cr:       []byte{200, 201, 202, 203},
	}

	dst := resizeYUV420P(src, 2, 2)

	if dst.Rect.Dx() != 2 || dst.Rect.Dy() != 2 {
		t.Fatalf("kích thước output = %dx%d, want 2x2", dst.Rect.Dx(), dst.Rect.Dy())
	}
	if len(dst.Y) != 4 {
		t.Fatalf("len(Y) = %d, want 4 (2x2)", len(dst.Y))
	}
	// 4:2:0 của ảnh đích 2x2 → plane màu 1x1 (làm tròn lên từ (2+1)/2=1).
	if len(dst.Cb) != 1 || len(dst.Cr) != 1 {
		t.Fatalf("len(Cb)=%d len(Cr)=%d, want 1,1 (subsampling 4:2:0 của đích 2x2)", len(dst.Cb), len(dst.Cr))
	}

	// Nearest-neighbor 4x4→2x2: dst(x,y) lấy src(x*4/2, y*4/2) = src(x*2, y*2).
	// dst(0,0)=src(0,0)=0; dst(1,0)=src(2,0)=2; dst(0,1)=src(0,2)=8; dst(1,1)=src(2,2)=10.
	want := []byte{0, 2, 8, 10}
	for i, w := range want {
		if dst.Y[i] != w {
			t.Errorf("dst.Y[%d] = %d, want %d (nearest-neighbor mapping sai)", i, dst.Y[i], w)
		}
	}
}

// TestEncodeJPEGFromYUV420P_ResizePath verify đường resize+JPEG-encode đầy
// đủ (encodeJPEGFromYUV420P với dw!=sw) chạy được và ra JPEG hợp lệ, đúng
// kích thước đích — decoder_test.go/video_e2e_test.go trước giờ luôn dùng
// InferWidth/Height == kích thước decode gốc nên KHÔNG đi qua nhánh này.
func TestEncodeJPEGFromYUV420P_ResizePath(t *testing.T) {
	src := &video.Frame{
		Width: 8, Height: 8,
		PixelFormat: video.PixelFormatYUV420P,
		StrideY:     8,
		Y:           make([]byte, 64),
		StrideCb:    4,
		Cb:          make([]byte, 16),
		Cr:          make([]byte, 16),
	}
	for i := range src.Y {
		src.Y[i] = byte(i * 4)
	}

	jpegBytes, err := encodeJPEGFromYUV420P(src, 4, 4, 80) // resize 8x8 → 4x4
	if err != nil {
		t.Fatalf("encodeJPEGFromYUV420P: %v", err)
	}
	if len(jpegBytes) == 0 {
		t.Fatal("JPEG output rỗng")
	}

	img, err := jpeg.Decode(bytes.NewReader(jpegBytes))
	if err != nil {
		t.Fatalf("JPEG output không decode được: %v", err)
	}
	if img.Bounds().Dx() != 4 || img.Bounds().Dy() != 4 {
		t.Errorf("kích thước JPEG output = %dx%d, want 4x4 (resize từ 8x8)", img.Bounds().Dx(), img.Bounds().Dy())
	}
}
