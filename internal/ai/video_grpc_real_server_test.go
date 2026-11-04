package ai

// test AI TRANSPORT: dựng 1 grpc.Server
// thật (network TCP loopback, HTTP/2, codec protowire —
// videoProtoCodec), KHÔNG dùng VideoStreamClient giả trong bộ nhớ.
//
//   - Generated messages interoperate with the frozen legacy server codec
//     using the same protobuf wire format (not only Go structs).
//   - VideoRoutingDialer chọn đúng worker qua WorkerRegistry
//   - SharedConnPool.VideoDialFunc() mở stream gRPC (HTTP/2, network
//     loopback) và gửi VideoConfig đầu tiên đúng như AI worker cần
//   - Toàn bộ round-trip qua network thật (không phải in-process channel)
//

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"

	"media-ai-gateway/internal/pipeline"
)

// realVideoServer là 1 gRPC server THẬT implement "video.VideoStream/Segment"
// bằng chính videoProtoCodec — mirror hành vi tối thiểu 1 AI worker cần có:
// nhận VideoConfig đầu tiên, sau đó với mỗi VideoFrame trả về 1 VideoMask
// (giả — không chạy model thật, chỉ echo lại frame_id để verify round-trip).
type realVideoServer struct {
	receivedConfig chan videoRequestWire
	receivedFrame  chan videoRequestWire
}

func (s *realVideoServer) segmentHandler(_ any, stream grpc.ServerStream) error {
	first := true
	for {
		var req videoRequestWire
		if err := stream.RecvMsg(&req); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if first {
			first = false
			if !req.HasConfig {
				// AI worker thật sẽ reject nếu message đầu không phải config —
				// mirror strict-ness đó ở đây để bắt lỗi thứ tự message sớm.
				return context.DeadlineExceeded
			}
			select {
			case s.receivedConfig <- req:
			default:
			}
			continue
		}
		if !req.HasFrame {
			continue
		}
		select {
		case s.receivedFrame <- req:
		default:
		}
		resp := &videoResponseWire{
			SessionID: req.SessionID,
			StreamID:  req.StreamID,
			IsFinal:   true,
			HasMask:   true,
			FrameID:   req.FrameID,
			MaskWidth: req.OriginalWidth, MaskHeight: req.OriginalHeight,
			RLECounts: []uint32{1, 2, 3, 4},
			Status:    "ok",
		}
		if err := stream.SendMsg(resp); err != nil {
			return err
		}
	}
}

// startRealVideoServer dựng grpc.Server thật trên TCP loopback (port ngẫu
// nhiên), implement đúng path videoSegmentRPC bằng videoProtoCodec
func startRealVideoServer(t *testing.T) (addr string, srv *realVideoServer) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv = &realVideoServer{
		receivedConfig: make(chan videoRequestWire, 4),
		receivedFrame:  make(chan videoRequestWire, 4),
	}
	gs := grpc.NewServer(grpc.ForceServerCodec(videoProtoCodec{}))
	gs.RegisterService(&grpc.ServiceDesc{
		ServiceName: "video.VideoStream",
		HandlerType: (*any)(nil),
		Streams: []grpc.StreamDesc{{
			StreamName:    "Segment",
			Handler:       srv.segmentHandler,
			ServerStreams: true,
			ClientStreams: true,
		}},
	}, nil)

	go gs.Serve(lis)
	t.Cleanup(gs.Stop)

	return lis.Addr().String(), srv
}

// TestVideoTransport_RealGRPC_Roundtrip là test AI TRANSPORT: dùng
// đúng SharedConnPool.VideoDialFunc() + VideoRoutingDialer + VideoManager nói chuyện qua network
// loopback với 1 server implement đúng protocol.
func TestVideoTransport_RealGRPC_Roundtrip(t *testing.T) {
	addr, srv := startRealVideoServer(t)

	registry := NewWorkerRegistry(time.Minute)
	registry.Register(WorkerInfo{
		ID: "video-worker-1", Addr: addr,
		Tasks: []string{"video_segment"}, Languages: []string{"*"},
		MediaTypes: []string{"video"}, InputFormats: []string{"jpeg"}, Protocols: []string{"video_segment"},
	})

	pool := NewSharedConnPool(0, 0)
	dialer := NewVideoRoutingDialer(registry, pool.VideoDialFunc())

	mgr := NewVideoManager(Config{
		MaxActiveStreams: 10,
		StreamTimeout:    10 * time.Second,
	}, dialer)

	frameIn := make(chan pipeline.VideoFrame, 4)
	resultOut := make(chan pipeline.RecognitionResult, 4)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	streamCfg := VideoStreamConfig{
		EffectType: "bg_blur", InferWidth: 64, InferHeight: 64, InferFPS: 5,
	}
	if _, err := mgr.Open(ctx, "sess-real-grpc-1", "stream-1", streamCfg, frameIn, resultOut); err != nil {
		t.Fatalf("Open: %v", err)
	}

	// 1. Verify server nhận đúng VideoConfig đầu tiên (qua network thật).
	select {
	case cfg := <-srv.receivedConfig:
		if cfg.EffectType != "bg_blur" {
			t.Errorf("server nhận effect_type = %q, want %q", cfg.EffectType, "bg_blur")
		}
		if cfg.InferWidth != 64 || cfg.InferHeight != 64 {
			t.Errorf("server nhận infer size = %dx%d, want 64x64", cfg.InferWidth, cfg.InferHeight)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: server không nhận được VideoConfig — kiểm tra VideoDialFunc() có gửi config đầu tiên không")
	}

	// 2. Gửi VideoFrame thật qua network, verify nhận đúng VideoMask ngược lại.
	frameIn <- pipeline.VideoFrame{
		SessionID: "sess-real-grpc-1", StreamID: "stream-1",
		FrameID: 42, RTPTimestamp: 12345,
		TimestampMs:   987654321, // xem S17 rtpgw_design_v3.md — phải round-trip đúng qua network thật
		OriginalWidth: 64, OriginalHeight: 64,
		JPEG: []byte{0xFF, 0xD8, 0xFF, 0xD9}, // JPEG SOI+EOI tối thiểu, đủ cho test transport (không cần ảnh thật)
	}

	// 1.5. Verify server nhận đúng timestamp_ms qua network thật (field mới
	// thêm khi đối chiếu rtpgw_design_v3.md §17 — trước bản sửa này CHƯA có
	// assertion nào cho field này dù wire format đã hỗ trợ).
	select {
	case fr := <-srv.receivedFrame:
		if fr.TimestampMs != 987654321 {
			t.Errorf("server nhận timestamp_ms = %d, want 987654321 (kiểm tra field 7 trong marshalVideoRequest/unmarshalVideoFrame)", fr.TimestampMs)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: server không nhận được VideoFrame")
	}

	select {
	case r := <-resultOut:
		if !r.IsVideoMask() {
			t.Fatalf("result không phải video_mask: %+v", r)
		}
		if r.VideoMask.FrameID != 42 {
			t.Errorf("mask.frame_id = %d, want 42 (round-trip qua network thật phải giữ nguyên)", r.VideoMask.FrameID)
		}
		if r.VideoMask.MaskWidth != 64 || r.VideoMask.MaskHeight != 64 {
			t.Errorf("mask size = %dx%d, want 64x64", r.VideoMask.MaskWidth, r.VideoMask.MaskHeight)
		}
		if len(r.VideoMask.RLECounts) != 4 {
			t.Errorf("len(rle_counts) = %d, want 4 (packed varint decode qua network thật)", len(r.VideoMask.RLECounts))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: không nhận được VideoMask qua network — kiểm tra marshalVideoRequest/unmarshalVideoResponse")
	}

	mgr.Close("sess-real-grpc-1")
	// Counters are cumulative and must remain visible after Close removes the stream.
	deadline := time.Now().Add(time.Second)
	for mgr.Stats().FramesSent != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	st := mgr.Stats()
	if st.FramesSent != 1 || st.ResultsReceived != 1 || st.SendErrors != 0 || st.RecvErrors != 0 || st.ResultErrors != 0 {
		t.Fatalf("video counters lost or incorrect after Close: %+v", st)
	}
}

// TestVideoTransport_RealGRPC_NoWorkerAvailable verify VideoRoutingDialer trả
// lỗi rõ ràng (không panic/treo) khi không có worker nào đăng ký task
// "video_segment" — case quan trọng khi AI Engine chưa/mất kết nối registry.
func TestVideoTransport_RealGRPC_NoWorkerAvailable(t *testing.T) {
	registry := NewWorkerRegistry(time.Minute) // rỗng, không đăng ký worker nào
	pool := NewSharedConnPool(0, 0)
	dialer := NewVideoRoutingDialer(registry, pool.VideoDialFunc())
	mgr := NewVideoManager(Config{MaxActiveStreams: 10, StreamTimeout: 2 * time.Second}, dialer)

	frameIn := make(chan pipeline.VideoFrame, 1)
	resultOut := make(chan pipeline.RecognitionResult, 1)

	_, err := mgr.Open(context.Background(), "sess-no-worker", "stream-1",
		VideoStreamConfig{}, frameIn, resultOut)
	if err == nil {
		t.Fatal("kỳ vọng lỗi khi không có worker nào — nhưng Open() trả nil")
	}
	t.Logf("OK: lỗi rõ ràng khi thiếu worker: %v", err)
}
