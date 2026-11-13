// Package video decode H.264 RTP -> decoded video frame (YUV420P/NV12) qua
// một thư viện C++ nhúng bằng cgo, link trực tiếp libavcodec/libavutil/
// libswscale (KHÔNG spawn subprocess ffmpeg như bản trước) — xem
// rtpgw_h264_cpp_design.md đi kèm để biết đầy đủ thiết kế C++.
//
// So với bản port từ mcf (subprocess ffmpeg qua pipe), bản này:
//   - Decode in-process, không fork/exec, không pipe — nhanh hơn, ít
//     syscall hơn, không cần binary `ffmpeg` trên PATH lúc runtime.
//   - PushRTP() decode và drain đồng bộ trong cùng lời gọi (không cần poll
//     loop/goroutine riêng để rút frame — gọi ReceiveFrame() ngay sau
//     PushRTP() cho tới khi hết là đủ).
//   - Độ phân giải decode được TỰ suy ra từ SPS trong stream (không cấu
//     hình cứng width/height khi tạo decoder như bản cũ).
//   - Output là YUV420P/NV12 tại độ phân giải gốc, KHÔNG tự resize/convert
//     sang RGB — việc resize + JPEG-encode cho AI vẫn làm ở Go
//     (video_pipeline.go), y hệt kiến trúc trước, chỉ khác input giờ là
//     YCbCr thay vì BGR24.
//
// BUILD REQUIREMENT: cần CGO_ENABLED=1, compiler C++17, và
// libavcodec/libavutil/libswscale (dev headers + .so) có sẵn qua pkg-config
// lúc build. Runtime chỉ cần .so tương ứng (không cần binary ffmpeg nữa).
package video

/*
#cgo pkg-config: libavcodec libavutil libswscale
#cgo CXXFLAGS: -std=c++17
#include <stdlib.h>
#include "video_decoder.h"
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"
)

// PixelFormat khớp VideoPixelFormat trong video_decoder.h.
type PixelFormat int

const (
	PixelFormatUnknown PixelFormat = iota
	PixelFormatYUV420P
	PixelFormatNV12
	PixelFormatRGB24
)

// Status khớp VideoStatus trong video_decoder.h — dùng để phân biệt các kết
// quả "không phải lỗi" (VideoNoFrame/VideoPacketGap/...) khỏi lỗi thật.
type Status int

const (
	StatusOK        Status = 0
	StatusNoFrame   Status = 1
	StatusPacketGap Status = 2
	StatusDropAU    Status = 3
	StatusWaitIDR   Status = 4
)

// Frame là 1 frame đã decode, plane Y/Cb/Cr được COPY sang bộ nhớ Go (an
// toàn dùng sau khi ReceiveFrame() trả về — không giữ pointer C nào).
// Chỉ hợp lệ khi PixelFormat == PixelFormatYUV420P (decoder hiện tại của
// mcf/video_parser_short chỉ thực sự sinh ra YUV420P/YUVJ420P từ H264
// baseline/main profile phổ biến; NV12/RGB24 hiếm gặp trong thực tế nhưng
// vẫn được map field pixel_format trung thực để pipeline tự quyết định).
type Frame struct {
	FrameID              uint64
	RTPTimestamp         uint32
	ExtendedRTPTimestamp uint64
	TimestampMs          int64

	Width, Height int
	PixelFormat   PixelFormat

	// YUV420P planes, đã copy, KHÔNG padding (stride == width cho Y,
	// width/2 cho Cb/Cr — xem CopyFrameToOwned trong h264_decoder.cpp).
	Y, Cb, Cr                   []byte
	StrideY, StrideCb, StrideCr int
}

// Stats khớp VideoDecoderStats trong video_decoder.h.
type Stats struct {
	RTPPacketsInput      uint64
	SingleNALUPackets    uint64
	StapAPackets         uint64
	FuAPackets           uint64
	PacketGaps           uint64 // số lần sequence RTP không liên tục
	CorruptedAccessUnits uint64
	AccessUnitsCompleted uint64
	IDRFrames            uint64
	WaitIDRDrops         uint64 // AU đã ghép xong nhưng bị bỏ vì đang chờ IDR
	DecodeErrors         uint64
	DecodedFrames        uint64
	ConversionErrors     uint64
	WaitIDREpisodes      uint64 // số lần bắt đầu chờ IDR do mất gói/AU hỏng (không tính đầu stream)
	WaitingForIDR        bool   // đang chờ IDR (kể cả đầu stream)
	OversizeFrames       uint64 // frame bị bỏ vì vượt MaxWidth/MaxHeight (không tính vào DecodeErrors)
}

// Config khớp VideoDecoderConfig. Numeric fields set to zero retain C++
// defaults, including when other fields (such as dimensions) are overridden.
// Mất gói luôn bỏ mọi slice tới IDR kế tiếp (không có cờ tắt).
// MaxWidth/MaxHeight là khung giới hạn không phân biệt ngang/dọc (cạnh dài ≤ cạnh dài
// giới hạn, cạnh ngắn ≤ cạnh ngắn giới hạn); frame vượt bị bỏ, đếm ở Stats.OversizeFrames.
type Config struct {
	MaxNALUSize         int
	MaxAccessUnitSize   int
	MaxWidth, MaxHeight int
	MaxReadyFrames      int
}

// Decoder là handle Go-facing cho 1 phiên decode video (1 session/stream).
// KHÔNG thread-safe trên cùng 1 handle — đúng contract của thư viện C++
// (§18 design doc): 1 handle chỉ được PushRTP tuần tự từ 1 goroutine.
type Decoder struct {
	handle    C.VideoDecoderHandle
	closeOnce sync.Once
}

// NewDecoder tạo decoder mới. cfg là zero-value (Config{}) để dùng default
// an toàn của C++ side.
func NewDecoder(cfg Config) (*Decoder, error) {
	if cfg.MaxNALUSize < 0 || cfg.MaxAccessUnitSize < 0 || cfg.MaxWidth < 0 || cfg.MaxHeight < 0 || cfg.MaxReadyFrames < 0 {
		return nil, fmt.Errorf("codec/video: config limits must be non-negative")
	}
	var handle C.VideoDecoderHandle
	if (cfg == Config{}) {
		handle = C.video_decoder_create()
	} else {
		cCfg := C.VideoDecoderConfig{
			max_nalu_size:        C.size_t(cfg.MaxNALUSize),
			max_access_unit_size: C.size_t(cfg.MaxAccessUnitSize),
			max_width:            C.int(cfg.MaxWidth),
			max_height:           C.int(cfg.MaxHeight),
			max_ready_frames:     C.int(cfg.MaxReadyFrames),
		}
		handle = C.video_decoder_create_with_config(&cCfg)
	}
	if handle == nil {
		return nil, fmt.Errorf("codec/video: video_decoder_create failed (decoder init/FFmpeg open lỗi)")
	}
	return &Decoder{handle: handle}, nil
}

// PushRTP đẩy 1 RTP packet (H264 payload, đã tách khỏi RTP header bởi
// pion/rtp) vào decode chain. Trả về Status (không phải error) cho các kết
// quả "bình thường" như StatusPacketGap; chỉ trả error cho lỗi thật (input
// không hợp lệ, decoder chưa mở, hết bộ nhớ...).
//
// Sau khi PushRTP trả về, gọi ReceiveFrame() theo vòng lặp cho tới khi
// StatusNoFrame — có thể có 0, 1, hoặc nhiều frame sẵn sàng ngay sau 1 lần
// PushRTP (decode + drain xảy ra đồng bộ bên trong).
func (d *Decoder) PushRTP(sequence uint16, timestamp uint32, marker bool, payload []byte) (Status, error) {
	if len(payload) == 0 {
		return StatusOK, fmt.Errorf("codec/video: empty RTP payload")
	}
	ret := C.video_decoder_push_rtp(
		d.handle,
		C.uint16_t(sequence), C.uint32_t(timestamp), boolToCInt(marker),
		(*C.uint8_t)(unsafe.Pointer(&payload[0])), C.int(len(payload)),
	)
	if ret < 0 {
		return Status(ret), fmt.Errorf("codec/video: push_rtp lỗi (%s)", statusString(Status(ret)))
	}
	return Status(ret), nil
}

// ReceiveFrame rút 1 frame đã decode ra khỏi decoder (non-blocking — decoder
// không tự chờ, luôn trả ngay). ok=false khi hết frame (StatusNoFrame) —
// gọi PushRTP tiếp để có thêm frame mới, không phải poll chờ.
//
// Frame trả về đã được COPY sang bộ nhớ Go (release ngay bên trong hàm này)
// — caller không cần/không thể gọi release thủ công, an toàn giữ Frame bao
// lâu tuỳ ý.
func (d *Decoder) ReceiveFrame() (*Frame, bool) {
	var cFrame C.DecodedVideoFrame
	ret := C.video_decoder_receive_frame(d.handle, &cFrame)
	if ret != C.VIDEO_OK {
		return nil, false
	}
	defer C.video_decoder_release_frame(d.handle, &cFrame)

	f := &Frame{
		FrameID:              uint64(cFrame.frame_id),
		RTPTimestamp:         uint32(cFrame.rtp_timestamp),
		ExtendedRTPTimestamp: uint64(cFrame.extended_rtp_timestamp),
		TimestampMs:          int64(cFrame.timestamp_ms),
		Width:                int(cFrame.width),
		Height:               int(cFrame.height),
		PixelFormat:          PixelFormat(cFrame.pixel_format),
		StrideY:              int(cFrame.stride0),
		StrideCb:             int(cFrame.stride1),
		StrideCr:             int(cFrame.stride2),
	}

	if cFrame.data == nil || cFrame.data_size <= 0 {
		return f, true
	}
	raw := C.GoBytes(unsafe.Pointer(cFrame.data), cFrame.data_size)

	switch f.PixelFormat {
	case PixelFormatYUV420P:
		ySize := f.StrideY * f.Height
		cSize := f.StrideCb * ((f.Height + 1) / 2)
		if ySize+cSize+cSize <= len(raw) {
			f.Y = raw[:ySize]
			f.Cb = raw[ySize : ySize+cSize]
			f.Cr = raw[ySize+cSize : ySize+cSize+cSize]
		}
	default:
		// NV12/RGB24/unknown: giữ nguyên buffer thô trong Y để pipeline
		// tầng trên tự xử lý theo PixelFormat nếu cần (hiện tại
		// video_pipeline.go chỉ xử lý YUV420P — xem README tích hợp).
		f.Y = raw
	}

	return f, true
}

// Stats trả về snapshot thống kê hiện tại (packet/AU/frame counters).
func (d *Decoder) Stats() Stats {
	var s C.VideoDecoderStats
	if C.video_decoder_get_stats(d.handle, &s) != C.VIDEO_OK {
		return Stats{}
	}
	return Stats{
		RTPPacketsInput:      uint64(s.rtp_packets_input),
		SingleNALUPackets:    uint64(s.single_nalu_packets),
		StapAPackets:         uint64(s.stapa_packets),
		FuAPackets:           uint64(s.fua_packets),
		PacketGaps:           uint64(s.packet_gaps),
		CorruptedAccessUnits: uint64(s.corrupted_access_units),
		AccessUnitsCompleted: uint64(s.access_units_completed),
		IDRFrames:            uint64(s.idr_frames),
		WaitIDRDrops:         uint64(s.wait_idr_drops),
		DecodeErrors:         uint64(s.decode_errors),
		DecodedFrames:        uint64(s.decoded_frames),
		ConversionErrors:     uint64(s.conversion_errors),
		WaitIDREpisodes:      uint64(s.wait_idr_episodes),
		WaitingForIDR:        s.waiting_for_idr != 0,
		OversizeFrames:       uint64(s.oversize_frames),
	}
}

// NotifyPacketLoss báo cho decoder biết Jitter Buffer (Go) đã phát hiện mất
// packet giữa 2 sequence number — decoder dùng để bảo vệ state FU-A/AU
// (drop fragment dở dang thay vì feed NAL hỏng vào libavcodec).
func (d *Decoder) NotifyPacketLoss(expectedSeq, receivedSeq uint16) {
	C.video_decoder_notify_packet_loss(d.handle, C.uint16_t(expectedSeq), C.uint16_t(receivedSeq))
}

// Reset đưa decoder về trạng thái WAIT_IDR (dùng khi SSRC đổi, lỗi decode
// nghiêm trọng, hoặc Control Plane yêu cầu reset stream).
func (d *Decoder) Reset() error {
	if ret := C.video_decoder_reset(d.handle); ret != C.VIDEO_OK {
		return fmt.Errorf("codec/video: reset lỗi (%s)", statusString(Status(ret)))
	}
	return nil
}

// Close giải phóng decoder (AVCodecContext, AVFrame/AVPacket, buffer nội bộ).
// An toàn gọi nhiều lần.
func (d *Decoder) Close() {
	d.closeOnce.Do(func() {
		C.video_decoder_destroy(d.handle)
	})
}

func boolToCInt(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

func statusString(s Status) string {
	switch s {
	case StatusOK:
		return "OK"
	case StatusNoFrame:
		return "NO_FRAME"
	case StatusPacketGap:
		return "PACKET_GAP"
	case StatusDropAU:
		return "DROP_AU"
	case StatusWaitIDR:
		return "WAIT_IDR"
	default:
		return fmt.Sprintf("ERR(%d)", int(s))
	}
}
