package coordinator

// video_e2e_test.go — test tích hợp video BỎ QUA HOÀN TOÀN HTTP/DCSF control
// plane, gọi thẳng Coordinator.StartVideo() với AI worker giả lập (fakeVideoClient).
//
// Mục đích: chứng minh luồng dữ liệu nội bộ RTPGW ↔ AI Engine đúng thiết kế
//
//	RTP H264 (thật, sinh bởi ffmpeg)
//	  → sess.VideoPacketQueue → Video Jitter Buffer → VideoWorkerPool
//	  → decoder native (libavcodec) → JPEG (đúng InferWidth/Height)
//	  → ai.VideoStreamClient.Send() (fake, thay AI thật)
//	  → ai.VideoStreamClient.Recv() (fake trả mask giả)
//	  → sess.ResultQueue → resultPump() → Dispatcher → HTTPCallbackSink → POST callback
//
// KHÔNG cần: HTTP control-plane thật, DCSF/DCAS mock, AI Engine Python thật,
// UDP ingress thật (packet được đẩy thẳng vào sess.VideoPacketQueue).
//
// Đây là cách nhanh nhất để trả lời "dữ liệu có được truyền đúng thiết kế
// giữa rtpgw và AI engine không" — vì phần DCSF chỉ là signaling/provisioning,
// không phải data plane; đúng/sai của DCSF wiring không ảnh hưởng tới việc
// core pipeline (decode → AI → callback) có đúng hay không.
//
// Nếu test này PASS nhưng gọi qua HTTP/DCSF thật lại không thấy callback nào
// (như tình huống hiện tại của bạn), kết luận chắc chắn: lỗi nằm ở tầng
// control-plane wiring (handler.go chưa gọi StartVideo, hoặc rawrtp ingress
// chưa route packet video vào VideoPacketQueue) — KHÔNG nằm ở decoder/AI
// stream/dispatcher.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"

	"media-ai-gateway/internal/ai"
	"media-ai-gateway/internal/pipeline"
	"media-ai-gateway/internal/session"
)

// ---------- fake video AI worker ----------

type fakeVideoClient struct {
	ctx     context.Context
	frames  chan pipeline.VideoFrame
	results chan pipeline.RecognitionResult
}

func (c *fakeVideoClient) Send(f pipeline.VideoFrame) error {
	select {
	case c.frames <- f:
		return nil
	case <-c.ctx.Done():
		return c.ctx.Err()
	}
}

func (c *fakeVideoClient) Recv() (pipeline.RecognitionResult, error) {
	select {
	case r := <-c.results:
		return r, nil
	case <-c.ctx.Done():
		return pipeline.RecognitionResult{}, io.EOF
	}
}

func (c *fakeVideoClient) CloseSend() error { return nil }

type fakeVideoDialer struct {
	mu      sync.Mutex
	clients map[string]*fakeVideoClient
}

func newFakeVideoDialer() *fakeVideoDialer {
	return &fakeVideoDialer{clients: make(map[string]*fakeVideoClient)}
}

func (d *fakeVideoDialer) Dial(ctx context.Context, sessionID, _ string, _ ai.VideoStreamConfig) (ai.VideoStreamClient, error) {
	c := &fakeVideoClient{
		ctx:     ctx,
		frames:  make(chan pipeline.VideoFrame, 32),
		results: make(chan pipeline.RecognitionResult, 16),
	}
	d.mu.Lock()
	d.clients[sessionID] = c
	d.mu.Unlock()
	return c, nil
}

func (d *fakeVideoDialer) client(sessionID string) *fakeVideoClient {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.clients[sessionID]
}

func waitForVideoClient(t *testing.T, d *fakeVideoDialer, sessionID string) *fakeVideoClient {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c := d.client(sessionID); c != nil {
			return c
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout: video AI client for session %s chưa được dial — kiểm tra StartVideo() có gọi videoAI.Open() không", sessionID)
	return nil
}

// ---------- stack helper (mirror newStack/newMgr trong coordinator_test.go) ----------

func newVideoStack(t *testing.T) (*Coordinator, *pipeline.VideoWorkerPool, *fakeVideoDialer) {
	t.Helper()
	coord, pool, _, disp, _ := newStack(t)

	videoPool := pipeline.NewVideoWorkerPool(pipeline.VideoWorkerPoolConfig{Workers: 4, QueueSize: 256})
	videoDialer := newFakeVideoDialer()
	videoAI := ai.NewVideoManager(ai.Config{MaxActiveStreams: 50, StreamTimeout: 30 * time.Second}, videoDialer)
	coord.WithVideo(videoPool, videoAI)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runStack(ctx, pool, disp)
	go videoPool.Run(ctx)

	return coord, videoPool, videoDialer
}

func newVideoMgr() *session.Manager {
	return session.NewManager(session.ManagerConfig{
		MaxSessions:          100,
		PacketQueueSize:      64,
		AudioQueueSize:       64,
		ResultQueueSize:      32,
		VideoPacketQueueSize: 256,
		VideoFrameQueueSize:  16,
		IdleTimeout:          30 * time.Second,
		GCInterval:           time.Minute,
	})
}

func createVideoSession(t *testing.T, mgr *session.Manager, id, callbackURL string) *session.Session {
	t.Helper()
	sess, err := mgr.Create(session.SessionConfig{
		ID: id, SourceType: "raw_rtp",
		Codec: "PCMU", SampleRate: 8000, Channels: 1, // video call vẫn có audio leg — Start() cần codec hợp lệ
		CallbackURL:     callbackURL,
		VideoEnabled:    true,
		VideoInferWidth: 64, VideoInferHeight: 64, // nhỏ cho test nhanh
		VideoInferFPS: 30, // cao để không bị FPS-sample bỏ frame trong test ngắn
		EffectType:    "bg_blur",
	})
	if err != nil {
		t.Fatalf("create video session %s: %v", id, err)
	}
	return sess
}

// TestStartVideoOnly_NoAudioCodec verify Coordinator.Start() không còn bắt
// buộc phải có audio codec hợp lệ — session VIDEO-ONLY (Codec == "", đúng
// như ServiceProfile.NeedsAudio=false sẽ tạo ra khi DCSF có service
// video-only trong tương lai — xem internal/controlplane/handler.go) vẫn
// Start() thành công VÀ callback vẫn nhận được kết quả (chứng minh
// resultPump/Dispatcher hoạt động độc lập khỏi audio pipeline).
func TestStartVideoOnly_NoAudioCodec(t *testing.T) {
	requireFFmpegForVideoE2E(t)

	received := make(chan videoCallbackBody, 4)
	srv := newH2CTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b videoCallbackBody
		json.NewDecoder(r.Body).Decode(&b)
		w.WriteHeader(http.StatusOK)
		select {
		case received <- b:
		default:
		}
	}))
	defer srv.Close()

	coord, _, videoDialer := newVideoStack(t)
	mgr := newVideoMgr()

	// Khác createVideoSession(): KHÔNG set Codec/SampleRate — mô phỏng đúng
	// session video-only mà handleAnswer() sẽ tạo khi ServiceProfile.NeedsAudio == false.
	sess, err := mgr.Create(session.SessionConfig{
		ID: "video-only-1", SourceType: "raw_rtp",
		CallbackURL:     srv.URL,
		VideoEnabled:    true,
		VideoInferWidth: 64, VideoInferHeight: 64,
		VideoInferFPS: 30,
		EffectType:    "bg_blur",
	})
	if err != nil {
		t.Fatalf("create video-only session: %v", err)
	}

	if _, err := coord.Start(sess); err != nil {
		t.Fatalf("Start() với session video-only (Codec rỗng) phải thành công, lỗi: %v", err)
	}
	if err := coord.StartVideo(sess); err != nil {
		t.Fatalf("StartVideo: %v", err)
	}

	pkts := genShortH264Packets(t, sess.ID)
	for _, p := range pkts {
		sess.VideoPacketQueue <- p
	}

	client := waitForVideoClient(t, videoDialer, sess.ID)
	var frame pipeline.VideoFrame
	select {
	case frame = <-client.frames:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: video-only session không decode/gửi được frame nào")
	}

	client.results <- pipeline.RecognitionResult{
		SessionID: sess.ID, Kind: "video_mask", IsFinal: true,
		VideoMask: &pipeline.VideoMask{FrameID: frame.FrameID, MaskWidth: 64, MaskHeight: 64, Status: "ok"},
	}

	select {
	case got := <-received:
		if got.VideoMask == nil || got.VideoMask.FrameID != frame.FrameID {
			t.Fatalf("callback không khớp frame_id — resultPump có thể chưa chạy đúng cho session video-only")
		}
		t.Logf("OK: session video-only (không audio codec) hoạt động đúng end-to-end")
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: không nhận được callback cho session video-only — resultPump có thể không chạy khi Codec rỗng")
	}
}

// ---------- H264 RTP fixture (giống decoder_test.go, rút gọn cho test này) ----------

func requireFFmpegForVideoE2E(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not found on PATH — chỉ cần để sinh fixture test, skip")
	}
}

// genShortH264Packets sinh vài access unit H264 nhỏ (đủ 1 IDR + vài P-frame)
// dưới dạng pipeline.MediaPacket sẵn sàng đẩy vào sess.VideoPacketQueue.
// Mỗi access unit (frame) = 1 lời gọi Packetize() riêng — GIỐNG HỆT cách
// genH264RTPFrameGroups() trong decoder_test.go, để mỗi AU có đúng 1 RTP
// timestamp + marker bit ở packet cuối (gộp tất cả NAL của cả clip vào 1 AU
// giả — như bản nháp đầu tiên của test này — là sai ngữ nghĩa H264 và khiến
// decode không ra frame nào).
func genShortH264Packets(t *testing.T, sessionID string) []pipeline.MediaPacket {
	t.Helper()
	dir := t.TempDir()
	rawPath := dir + "/raw.h264"

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=duration=1:size=64x64:rate=10",
		"-c:v", "libx264", "-profile:v", "baseline", "-tune", "zerolatency",
		"-x264-params", "repeat-headers=1",
		"-pix_fmt", "yuv420p",
		"-f", "h264", rawPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg encode: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(rawPath)
	if err != nil {
		t.Fatalf("read elementary stream: %v", err)
	}

	frameGroups := splitAnnexBIntoFramesE2E(raw)
	if len(frameGroups) == 0 {
		t.Fatalf("không tách được frame nào từ elementary stream (%d bytes)", len(raw))
	}

	payloader := &codecs.H264Payloader{}
	sequencer := rtp.NewRandomSequencer()
	packetizer := rtp.NewPacketizer(1200, 96, 0xC0FFEE, payloader, sequencer, 90000)
	const samplesPerFrame = 9000 // 90000Hz / 10fps

	var out []pipeline.MediaPacket
	for _, nals := range frameGroups {
		var buf []byte
		for _, n := range nals {
			buf = append(buf, 0x00, 0x00, 0x00, 0x01)
			buf = append(buf, n...)
		}
		for _, p := range packetizer.Packetize(buf, samplesPerFrame) {
			out = append(out, pipeline.MediaPacket{
				SessionID:    sessionID,
				SourceType:   "raw_rtp",
				SSRC:         p.SSRC,
				PayloadType:  p.PayloadType,
				Sequence:     p.SequenceNumber,
				Timestamp:    p.Timestamp,
				Marker:       p.Marker,
				Payload:      p.Payload,
				ReceivedAtMs: time.Now().UnixMilli(), // BẮT BUỘC: jitter.Buffer.Push() drop packet nếu thiếu field này (age = now - UnixMilli(0) ≈ 56 năm > MaxLateMs)
			})
		}
	}
	return out
}

// splitAnnexBIntoFramesE2E tách elementary stream thành từng access unit:
// kết thúc ngay sau NAL loại VCL (slice, type 1 hoặc 5) đầu tiên.
func splitAnnexBIntoFramesE2E(data []byte) [][][]byte {
	nals := splitAnnexBNALUsE2E(data)
	var frames [][][]byte
	var cur [][]byte
	for _, n := range nals {
		if len(n) == 0 {
			continue
		}
		cur = append(cur, n)
		if t := n[0] & 0x1F; t == 1 || t == 5 {
			frames = append(frames, cur)
			cur = nil
		}
	}
	if len(cur) > 0 {
		frames = append(frames, cur)
	}
	return frames
}

func splitAnnexBNALUsE2E(data []byte) [][]byte {
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
			e = starts[i+1] - 3
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

// ---------- test chính ----------

type videoCallbackBody struct {
	EventType string `json:"event_type"`
	pipeline.RecognitionResult
}

// TestStartVideo_EndToEnd_RTPToCallback là test quan trọng nhất: chứng minh
// TOÀN BỘ chuỗi RTP → decode → JPEG → AI(fake) → mask → callback hoạt động
// đúng thiết kế, hoàn toàn KHÔNG qua HTTP/DCSF.
func TestStartVideo_EndToEnd_RTPToCallback(t *testing.T) {
	requireFFmpegForVideoE2E(t)

	received := make(chan videoCallbackBody, 4)
	srv := newH2CTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b videoCallbackBody
		json.NewDecoder(r.Body).Decode(&b)
		w.WriteHeader(http.StatusOK)
		select {
		case received <- b:
		default:
		}
	}))
	defer srv.Close()

	coord, _, videoDialer := newVideoStack(t)
	mgr := newVideoMgr()
	sess := createVideoSession(t, mgr, "video-e2e-1", srv.URL)

	if _, err := coord.Start(sess); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := coord.StartVideo(sess); err != nil {
		t.Fatalf("StartVideo: %v", err)
	}

	// 1. Đẩy RTP H264 thật vào đúng entry point sản xuất dùng (VideoPacketQueue).
	pkts := genShortH264Packets(t, sess.ID)
	for _, p := range pkts {
		sess.VideoPacketQueue <- p
	}

	// 2. Verify Gateway thực sự decode + JPEG-encode + gửi frame sang "AI".
	client := waitForVideoClient(t, videoDialer, sess.ID)
	var frame pipeline.VideoFrame
	select {
	case frame = <-client.frames:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: không có VideoFrame nào tới AI client — kiểm tra decode chain (RTP→libavcodec→JPEG)")
	}
	if len(frame.JPEG) == 0 {
		t.Error("frame.JPEG rỗng — decode/encode lỗi")
	}
	if frame.SessionID != sess.ID {
		t.Errorf("frame.SessionID = %q, want %q", frame.SessionID, sess.ID)
	}
	if frame.EffectType != "bg_blur" {
		t.Errorf("frame.EffectType = %q, want %q (phải truyền đúng từ SessionConfig)", frame.EffectType, "bg_blur")
	}
	t.Logf("OK: gateway decode+encode thật, JPEG %d bytes, %dx%d gốc", len(frame.JPEG), frame.OriginalWidth, frame.OriginalHeight)

	// 3. Giả lập AI Engine trả mask — verify Gateway forward đúng qua callback.
	wantMask := pipeline.RecognitionResult{
		SessionID: sess.ID,
		Kind:      "video_mask",
		IsFinal:   true,
		VideoMask: &pipeline.VideoMask{
			FrameID:   frame.FrameID,
			MaskWidth: 64, MaskHeight: 64,
			RLECounts: []uint32{100, 50, 4046},
			Status:    "ok",
		},
	}
	client.results <- wantMask

	select {
	case got := <-received:
		if got.EventType != "video.mask.result" {
			t.Errorf("event_type = %q, want %q", got.EventType, "video.mask.result")
		}
		if got.SessionID != sess.ID {
			t.Errorf("callback session_id = %q, want %q", got.SessionID, sess.ID)
		}
		if got.VideoMask == nil {
			t.Fatal("callback không có video_mask — kiểm tra RecognitionResult JSON tag / eventTypeOf()")
		}
		if got.VideoMask.FrameID != frame.FrameID {
			t.Errorf("video_mask.frame_id = %d, want %d (phải khớp frame đã gửi ở bước 2)", got.VideoMask.FrameID, frame.FrameID)
		}
		t.Logf("OK: callback nhận đúng video_mask cho frame_id=%d", got.VideoMask.FrameID)
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: callback server không nhận được video.mask.result — kiểm tra resultPump()/Dispatcher/eventTypeOf()")
	}
}
