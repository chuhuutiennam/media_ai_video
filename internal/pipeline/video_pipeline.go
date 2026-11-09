package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"media-ai-gateway/internal/codec/video"
)

// VideoSessionConfig mô tả video pipeline cần tạo cho một session.
type VideoSessionConfig struct {
	// Giới hạn an toàn cho decoder: decoder mới tự suy ra độ phân giải thật từ SPS trong stream.
	MaxWidth, MaxHeight int

	InferWidth, InferHeight int
	InferFPS                float64 // FPS sample cho leg AI; <=0 → dùng default 5

	JPEGQuality int

	SessionID           string
	StreamID            string
	EffectType          string // "bg_blur" | "bg_replace" | "bg_remove"
	Gesture             *GestureOutputConfig
	DisableSegmentation bool // Gesture-only service; skip mask preprocessing/output.
	SourceMaxDimension  int  // >0: one aspect-preserving source JPEG; engine owns task fan-out.
}

// Each task has independent sampling and a bounded queue, sharing one decoder.
type GestureOutputConfig struct {
	StreamID     string
	FPS          int
	MaxDimension int
	Output       chan VideoFrame
}

// sessionVideoPipeline giữ state decode video theo từng session.
//
// Khác bản subprocess cũ: decoder mới (native libavcodec, xem
// internal/codec/video) decode + drain ĐỒNG BỘ ngay trong push() — không cần
// poll loop/goroutine riêng nữa. push() được gọi tuần tự từ đúng 1 worker
// goroutine của VideoWorkerPool cho mỗi session (đúng contract "1 handle
// không thread-safe, không gọi concurrent" của thư viện C++).
type sessionVideoPipeline struct {
	cfg      VideoSessionConfig
	decoder  *video.Decoder
	frameOut chan VideoFrame
	ctx      context.Context

	aiFrameSeq        atomic.Int64
	aiInterval        time.Duration
	decodedSeq        int64
	lastMediaSample   int64
	hasMediaSample    bool
	lastGestureSample int64
	hasGestureSample  bool

	packetsIn     atomic.Uint64
	framesOut     atomic.Uint64
	framesDropped atomic.Uint64
	decodeErrs    atomic.Uint64
	nativeErrors  uint64 // native decoder counters already accumulated; protected by mu

	// Mất gói / chờ IDR: counter cộng dồn (worker pool lấy delta) và đợt chờ hiện tại.
	packetGaps       atomic.Uint64
	waitDrops        atomic.Uint64
	waitEpisodes     atomic.Uint64
	waitMs           atomic.Uint64
	waitingIDR       atomic.Bool // đang trong một đợt chờ IDR do mất gói
	nativeLoss       video.Stats // counter native đã cộng; protected by mu
	waitStartMs      int64       // giờ nhận gói lúc bắt đầu đợt chờ; protected by mu
	waitDropsAtStart uint64      // waitDrops lúc bắt đầu đợt chờ; protected by mu

	mu     sync.Mutex
	closed bool
}

// newSessionVideoPipeline tạo pipeline decode cho một session.
func newSessionVideoPipeline(ctx context.Context, cfg VideoSessionConfig, frameOut chan VideoFrame) (*sessionVideoPipeline, error) {
	if cfg.DisableSegmentation && cfg.Gesture == nil && cfg.SourceMaxDimension == 0 {
		return nil, fmt.Errorf("video pipeline requires at least one task")
	}
	if g := cfg.Gesture; g != nil && (g.FPS < 1 || g.FPS > 60 || g.Output == nil || g.MaxDimension < 1) {
		return nil, fmt.Errorf("invalid gesture subscriber profile")
	}
	if cfg.JPEGQuality <= 0 {
		cfg.JPEGQuality = 80
	}

	dec, err := video.NewDecoder(video.Config{
		MaxWidth:  cfg.MaxWidth,
		MaxHeight: cfg.MaxHeight,
	})
	if err != nil {
		return nil, fmt.Errorf("pipeline: video: %w", err)
	}

	fps := cfg.InferFPS
	if fps <= 0 {
		fps = 5
	}

	sp := &sessionVideoPipeline{
		cfg:        cfg,
		decoder:    dec,
		frameOut:   frameOut,
		ctx:        ctx,
		aiInterval: time.Duration(float64(time.Second) / fps),
	}
	return sp, nil
}

// push đẩy một MediaPacket (RTP H264) vào decode chain, rồi rút NGAY mọi
// frame đã decode xong trong cùng lời gọi này (đồng bộ — không polling).
// Gọi tuần tự từ 1 worker goroutine duy nhất cho mỗi session.
func (sp *sessionVideoPipeline) push(pkt MediaPacket) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.closed {
		return // session đã đóng (race với UnregisterSession) — no-op, không đụng decoder đã Close()
	}
	defer func() {
		// libavcodec may reject an AU while PushRTP itself returns StatusOK.
		// Include those errors rather than only checking the Go error return.
		st := sp.decoder.Stats()
		total := st.DecodeErrors + st.ConversionErrors
		sp.decodeErrs.Add(total - sp.nativeErrors)
		sp.nativeErrors = total
		atMs := pkt.ReceivedAtMs
		if atMs <= 0 {
			atMs = time.Now().UnixMilli()
		}
		sp.trackLoss(st, atMs)
	}()

	sp.packetsIn.Add(1)

	status, err := sp.decoder.PushRTP(pkt.Sequence, pkt.Timestamp, pkt.Marker, pkt.Payload)
	if err != nil {
		sp.decodeErrs.Add(1)
		zap.L().Debug("pipeline: video: push_rtp lỗi", zap.String("session_id", sp.cfg.SessionID), zap.Error(err))
		return
	}
	if status == video.StatusPacketGap {
		zap.L().Debug("pipeline: video: phát hiện packet gap", zap.String("session_id", sp.cfg.SessionID), zap.Uint32("seq", uint32(pkt.Sequence)))
	}

	for {
		frame, ok := sp.decoder.ReceiveFrame()
		if !ok {
			return
		}
		sp.handleDecodedFrame(frame)
	}
}

// trackLoss cộng số liệu mất gói từ counter native và theo dõi đợt chờ IDR theo giờ nhận gói
// tại listener (atMs). Đợt chờ bắt đầu khi decoder báo thêm một episode, kết thúc khi decoder
// hết chờ (đã nhận IDR). Lần chờ IDR đầu stream không tính. Gọi dưới sp.mu sau mỗi PushRTP.
func (sp *sessionVideoPipeline) trackLoss(st video.Stats, atMs int64) {
	prev := sp.nativeLoss
	sp.nativeLoss = st
	dropsBefore := sp.waitDrops.Load()
	sp.packetGaps.Add(st.PacketGaps - prev.PacketGaps)
	sp.waitDrops.Add(st.WaitIDRDrops - prev.WaitIDRDrops)
	started := st.WaitIDREpisodes - prev.WaitIDREpisodes
	sp.waitEpisodes.Add(started)

	if sp.waitingIDR.Load() && (started > 0 || !st.WaitingForIDR) {
		sp.endWait(atMs)
	}
	if started == 0 {
		return
	}
	sp.waitingIDR.Store(true)
	sp.waitStartMs, sp.waitDropsAtStart = atMs, dropsBefore
	zap.L().Debug("pipeline: video: mất gói, chờ IDR", zap.String("session_id", sp.cfg.SessionID))
	if !st.WaitingForIDR { // bắt đầu và hết chờ trong cùng một gói
		sp.endWait(atMs)
	}
}

func (sp *sessionVideoPipeline) endWait(atMs int64) {
	waited := max(int64(0), atMs-sp.waitStartMs)
	sp.waitMs.Add(uint64(waited))
	sp.waitingIDR.Store(false)
	zap.L().Info("pipeline: video: đã nhận IDR sau khi mất gói",
		zap.String("session_id", sp.cfg.SessionID),
		zap.Int64("wait_ms", waited),
		zap.Uint64("dropped_frames", sp.waitDrops.Load()-sp.waitDropsAtStart))
}

// handleDecodedFrame samples each task after decoding and prepares JPEG buffers,
// rồi emit vào frameOut (non-blocking — drop frame cũ nhất và giữ frame mới nhất nếu AI leg queue đầy).
func (sp *sessionVideoPipeline) handleDecodedFrame(frame *video.Frame) {
	sp.decodedSeq++
	if frame.PixelFormat != video.PixelFormatYUV420P {
		sp.decodeErrs.Add(1)
		zap.L().Warn("pipeline: video: pixel format không hỗ trợ, bỏ frame",
			zap.String("session_id", sp.cfg.SessionID), zap.Int("pixel_format", int(frame.PixelFormat)))
		return
	}

	if sp.cfg.SourceMaxDimension > 0 {
		// Media timestamps have millisecond precision: a 15 FPS period can be
		// 66 ms, so comparing against 66.666 ms would accidentally halve FPS.
		if sp.hasMediaSample && frame.TimestampMs-sp.lastMediaSample < max(int64(1), sp.aiInterval.Milliseconds()) {
			return
		}
		w, h := frame.Width, frame.Height
		if longest := max(w, h); longest > sp.cfg.SourceMaxDimension {
			w, h = max(1, w*sp.cfg.SourceMaxDimension/longest), max(1, h*sp.cfg.SourceMaxDimension/longest)
		}
		data, err := encodeJPEGFromYUV420P(frame, w, h, sp.cfg.JPEGQuality)
		if err != nil {
			sp.decodeErrs.Add(1)
			return
		}
		sp.hasMediaSample, sp.lastMediaSample = true, frame.TimestampMs
		sp.enqueueFrame(VideoFrame{SessionID: sp.cfg.SessionID, StreamID: sp.cfg.StreamID,
			FrameID: sp.decodedSeq, RTPTimestamp: frame.RTPTimestamp, ExtendedRTPTimestamp: frame.ExtendedRTPTimestamp,
			TimestampMs: frame.TimestampMs, OriginalWidth: int32(frame.Width), OriginalHeight: int32(frame.Height),
			JPEG: data, Width: w, Height: h})
		return
	}
	// Sample gesture before the segmentation early return: consumers never split a channel.
	if g := sp.cfg.Gesture; g != nil && (!sp.hasGestureSample || frame.TimestampMs-sp.lastGestureSample >= int64(1000/g.FPS)) {
		w, h := frame.Width, frame.Height
		if longest := max(w, h); longest > g.MaxDimension {
			w, h = max(1, w*g.MaxDimension/longest), max(1, h*g.MaxDimension/longest)
		}
		pixels, err := encodeJPEGFromYUV420P(frame, w, h, sp.cfg.JPEGQuality)
		if err != nil {
			sp.decodeErrs.Add(1)
		} else {
			sp.lastGestureSample, sp.hasGestureSample = frame.TimestampMs, true
			out := VideoFrame{SessionID: sp.cfg.SessionID, StreamID: g.StreamID, FrameID: sp.decodedSeq,
				RTPTimestamp: frame.RTPTimestamp, ExtendedRTPTimestamp: frame.ExtendedRTPTimestamp, TimestampMs: frame.TimestampMs,
				OriginalWidth: int32(frame.Width), OriginalHeight: int32(frame.Height), JPEG: pixels, Width: w, Height: h}
			sp.enqueueTo(g.Output, out)
		}
	}
	if sp.cfg.DisableSegmentation {
		return
	}
	if sp.hasMediaSample && time.Duration(frame.TimestampMs-sp.lastMediaSample)*time.Millisecond < sp.aiInterval {
		return
	}
	w, h := sp.cfg.InferWidth, sp.cfg.InferHeight
	if w <= 0 || h <= 0 {
		w, h = frame.Width, frame.Height
	}
	jpegBuf, err := encodeJPEGFromYUV420P(frame, w, h, sp.cfg.JPEGQuality)
	if err != nil {
		sp.decodeErrs.Add(1)
		zap.L().Warn("pipeline: video: jpeg encode failed", zap.String("session_id", sp.cfg.SessionID), zap.Error(err))
		return
	}
	sp.lastMediaSample, sp.hasMediaSample = frame.TimestampMs, true

	out := VideoFrame{
		SessionID:            sp.cfg.SessionID,
		StreamID:             sp.cfg.StreamID,
		FrameID:              sp.aiFrameSeq.Add(1),
		RTPTimestamp:         frame.RTPTimestamp,
		ExtendedRTPTimestamp: frame.ExtendedRTPTimestamp,
		TimestampMs:          frame.TimestampMs,
		OriginalWidth:        int32(frame.Width),
		OriginalHeight:       int32(frame.Height),
		JPEG:                 jpegBuf,
		EffectType:           sp.cfg.EffectType,
		Width:                w,
		Height:               h,
	}
	if sp.cfg.Gesture != nil {
		out.FrameID = sp.decodedSeq
	}

	sp.enqueueFrame(out)
}

func (sp *sessionVideoPipeline) enqueueFrame(out VideoFrame) {
	sp.enqueueTo(sp.frameOut, out)
}

func (sp *sessionVideoPipeline) enqueueTo(output chan VideoFrame, out VideoFrame) {
	select {
	case output <- out:
		sp.framesOut.Add(1)
		return
	default:
	}

	// Queue full: evict oldest to keep the newest frame.
	select {
	case <-output:
		sp.framesDropped.Add(1)
	default:
	}

	// Single producer (one shard goroutine per session) so the vacancy from
	// above — or one the consumer already made — is still there.
	output <- out
	sp.framesOut.Add(1)
}

// close đóng decoder (giải phóng AVCodecContext/buffer nội bộ). chờ các push() đang chạy
// hoàn tất rồi mới Close()
func (sp *sessionVideoPipeline) close() {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.closed {
		return
	}
	sp.closed = true
	sp.decoder.Close()
}

// encodeJPEGFromYUV420P dựng image.YCbCr trực tiếp từ plane Y/Cb/Cr đã
// decode (không cần chuyển qua RGBA trước khi JPEG-encode — image/jpeg hỗ
// trợ encode thẳng YCbCr, nhanh hơn và không mất màu do round-trip RGB).
// Nếu cần resize xuống kích thước inference, resize plane trước bằng
// nearest-neighbor (đủ tốt cho input model segmentation).
func encodeJPEGFromYUV420P(f *video.Frame, dw, dh, quality int) ([]byte, error) {
	sw, sh := f.Width, f.Height
	if sw <= 0 || sh <= 0 {
		return nil, fmt.Errorf("kích thước frame không hợp lệ %dx%d", sw, sh)
	}
	if dw <= 0 || dh <= 0 {
		dw, dh = sw, sh
	}

	var img *image.YCbCr
	if dw == sw && dh == sh {
		img = ycbcrFromFrame(f)
	} else {
		img = resizeYUV420P(f, dw, dh)
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ycbcrFromFrame wrap trực tiếp plane đã decode vào image.YCbCr (không copy
// thêm lần nữa — plane trong Frame đã là bản copy owned bởi Go).
func ycbcrFromFrame(f *video.Frame) *image.YCbCr {
	return &image.YCbCr{
		Y:              f.Y,
		Cb:             f.Cb,
		Cr:             f.Cr,
		YStride:        f.StrideY,
		CStride:        f.StrideCb,
		SubsampleRatio: image.YCbCrSubsampleRatio420,
		Rect:           image.Rect(0, 0, f.Width, f.Height),
	}
}

// resizeYUV420P nearest-neighbor resize riêng từng plane Y/Cb/Cr, giữ đúng
// tỉ lệ subsampling 4:2:0 (plane màu luôn bằng nửa kích thước plane sáng,
// làm tròn lên).
func resizeYUV420P(f *video.Frame, dw, dh int) *image.YCbCr {
	sw, sh := f.Width, f.Height
	dcw, dch := (dw+1)/2, (dh+1)/2
	scw, sch := (sw+1)/2, (sh+1)/2

	dst := &image.YCbCr{
		Y:              make([]byte, dw*dh),
		Cb:             make([]byte, dcw*dch),
		Cr:             make([]byte, dcw*dch),
		YStride:        dw,
		CStride:        dcw,
		SubsampleRatio: image.YCbCrSubsampleRatio420,
		Rect:           image.Rect(0, 0, dw, dh),
	}

	for y := 0; y < dh; y++ {
		sy := y * sh / dh
		srow := sy * f.StrideY
		drow := y * dw
		for x := 0; x < dw; x++ {
			sx := x * sw / dw
			dst.Y[drow+x] = f.Y[srow+sx]
		}
	}
	for y := 0; y < dch; y++ {
		sy := y * sch / dch
		srow := sy * f.StrideCb
		drow := y * dcw
		for x := 0; x < dcw; x++ {
			sx := x * scw / dcw
			dst.Cb[drow+x] = f.Cb[srow+sx]
			dst.Cr[drow+x] = f.Cr[srow+sx]
		}
	}
	return dst
}
