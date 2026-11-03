package rawrtp

//test UDP socket cho StartVideoSessionListener
import (
	"bytes"
	"github.com/pion/rtp"
	"net"
	"testing"
	"time"

	"media-ai-gateway/internal/session"
)

func makeTestVideoSession(t *testing.T) (*session.Session, func()) {
	t.Helper()
	mgr := session.NewManager(session.ManagerConfig{
		MaxSessions:          10,
		PacketQueueSize:      32,
		AudioQueueSize:       8,
		ResultQueueSize:      8,
		VideoPacketQueueSize: 32,
		VideoFrameQueueSize:  8,
		IdleTimeout:          time.Minute,
		GCInterval:           time.Minute,
	})
	sess, err := mgr.Create(session.SessionConfig{
		ID:           "test-video-sess",
		SourceType:   "raw_rtp",
		VideoEnabled: true,
		VideoCodec:   "H264",
	})
	if err != nil {
		t.Fatalf("create video session: %v", err)
	}
	return sess, func() { mgr.Close("test-video-sess") }
}

// TestVideoPortListener_ReceivesPacket gửi 1 RTP datagram H264 qua UDP
// socket (net.Dial), verify StartVideoSessionListener parse đúng bằng
// pion/rtp và route đúng vào sess.VideoPacketQueue.
func TestVideoPortListener_ReceivesPacket(t *testing.T) {
	releaser := &fakeReleaser{}
	sess, cancel := makeTestVideoSession(t)
	defer cancel()

	ln, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pre-listen: %v", err)
	}
	port := ln.LocalAddr().(*net.UDPAddr).Port
	ln.Close()

	if err := StartVideoSessionListener(sess, "127.0.0.1", port, releaser); err != nil {
		t.Fatalf("StartVideoSessionListener: %v", err)
	}

	conn, err := net.Dial("udp", "127.0.0.1:"+itoa(port))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// NAL loại 5 (IDR) tối giản làm payload — không cần hợp lệ H264 thật,
	// StartVideoSessionListener chỉ parse RTP header, không đụng NAL.
	h264Payload := []byte{0x65, 0x88, 0x84, 0x00}
	pkt := buildRTP(42, 90000, 0xCAFEBABE, 96, true, h264Payload)
	if _, err := conn.Write(pkt); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case got := <-sess.VideoPacketQueue:
		if got.SSRC != 0xCAFEBABE {
			t.Errorf("SSRC: want 0xCAFEBABE, got 0x%08X", got.SSRC)
		}
		if got.Sequence != 42 {
			t.Errorf("Sequence: want 42, got %d", got.Sequence)
		}
		if got.Timestamp != 90000 {
			t.Errorf("Timestamp: want 90000, got %d", got.Timestamp)
		}
		if got.PayloadType != 96 {
			t.Errorf("PayloadType: want 96, got %d", got.PayloadType)
		}
		if !got.Marker {
			t.Error("Marker: want true (packet cuối AU)")
		}
		if got.SessionID != "test-video-sess" {
			t.Errorf("SessionID: want test-video-sess, got %s", got.SessionID)
		}
		if got.Codec != "H264" {
			t.Errorf("Codec: want H264, got %s", got.Codec)
		}
		if len(got.Payload) != len(h264Payload) || got.Payload[0] != h264Payload[0] {
			t.Errorf("Payload mismatch: got %v, want %v", got.Payload, h264Payload)
		}
		if got.ReceivedAtMs == 0 {
			t.Error("ReceivedAtMs = 0 — jitter buffer sẽ drop packet này ngay (age tính từ epoch 1970), xem bug đã tìm thấy khi viết video_e2e_test.go")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: không nhận được packet trong sess.VideoPacketQueue")
	}

	// Đảm bảo KHÔNG lọt nhầm vào sess.PacketQueue (audio) — SSRC/port hoàn
	// toàn độc lập giữa 2 track, route sai sẽ khiến audio pipeline nhận
	// nhầm packet H264.
	select {
	case leaked := <-sess.PacketQueue:
		t.Fatalf("packet video bị lọt nhầm vào sess.PacketQueue (audio): %+v", leaked)
	case <-time.After(100 * time.Millisecond):
		// OK — không có gì trong PacketQueue.
	}
}

// TestVideoPortListener_StopsOnContextCancel verify port được release đúng
// khi session đóng — đối xứng TestPortManager_StopsOnContextCancel (audio).
func TestVideoPortListener_StopsOnContextCancel(t *testing.T) {
	releaser := &fakeReleaser{}
	sess, cancel := makeTestVideoSession(t)

	ln, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pre-listen: %v", err)
	}
	port := ln.LocalAddr().(*net.UDPAddr).Port
	ln.Close()

	if err := StartVideoSessionListener(sess, "127.0.0.1", port, releaser); err != nil {
		t.Fatalf("StartVideoSessionListener: %v", err)
	}

	cancel()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if releaser.count() > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("video port không được release sau khi session context bị cancel")
}

// TestVideoPortListener_BindError_ReturnsError verify lỗi bind port (đã bị
// chiếm) trả về error rõ ràng thay vì panic/silent fail.
func TestVideoPortListener_BindError_ReturnsError(t *testing.T) {
	ln, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pre-bind: %v", err)
	}
	defer ln.Close()
	port := ln.LocalAddr().(*net.UDPAddr).Port

	releaser := &fakeReleaser{}
	sess, cancel := makeTestVideoSession(t)
	defer cancel()

	err = StartVideoSessionListener(sess, "127.0.0.1", port, releaser)
	if err == nil {
		t.Fatal("expected bind error (port đã bị chiếm bởi ln), got nil")
	}
}

// TestVideoPortListener_QueueFull_IncrementsDropCounter verify khi
// VideoPacketQueue đầy (consumer không đọc), packet bị drop có đếm qua
// VideoPacketQueueDropped thay vì block/panic.
func TestVideoPortListener_QueueFull_IncrementsDropCounter(t *testing.T) {
	releaser := &fakeReleaser{}
	sess, cancel := makeTestVideoSession(t) // VideoPacketQueueSize=32 theo makeTestVideoSession
	defer cancel()

	ln, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pre-listen: %v", err)
	}
	port := ln.LocalAddr().(*net.UDPAddr).Port
	ln.Close()

	if err := StartVideoSessionListener(sess, "127.0.0.1", port, releaser); err != nil {
		t.Fatalf("StartVideoSessionListener: %v", err)
	}

	conn, err := net.Dial("udp", "127.0.0.1:"+itoa(port))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Gửi nhiều hơn capacity queue (32) mà KHÔNG đọc — buộc phải drop.
	for i := 0; i < 50; i++ {
		pkt := buildRTP(uint16(i), uint32(i)*90, 0x1234, 96, false, []byte{0x61, 0x00})
		conn.Write(pkt)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sess.VideoPacketQueueDropped.Load() > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("gửi 50 packet vào queue cap 32 không đọc — VideoPacketQueueDropped vẫn = 0, kỳ vọng > 0")
}

// RTP padding and header extensions must never reach the H264 parser. Invalid
// version/truncated headers/padding are skipped, and datagrams >1500 bytes are
// received intact (a truncated FU-A otherwise corrupts the complete frame).
func TestVideoPortListener_ExtensionsPaddingAndMalformed(t *testing.T) {
	sess, cancel := makeTestVideoSession(t)
	defer cancel()
	ln, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.LocalAddr().(*net.UDPAddr).Port
	ln.Close()
	if err := StartVideoSessionListener(sess, "127.0.0.1", port, &fakeReleaser{}); err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("udp", "127.0.0.1:"+itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	payload := bytes.Repeat([]byte{0x65}, 2048)
	packet := rtp.Packet{Header: rtp.Header{Version: 2, Padding: true, SequenceNumber: 77, Timestamp: 90000, SSRC: 123, PayloadType: 96, Marker: true}, Payload: payload, PaddingSize: 4}
	if err := packet.Header.SetExtension(1, []byte{2, 3, 4}); err != nil {
		t.Fatal(err)
	}
	valid, err := packet.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	invalidVersion := append([]byte(nil), valid...)
	invalidVersion[0] = (invalidVersion[0] & 0x3f) | 0x40
	invalidPadding := append([]byte(nil), valid...)
	invalidPadding[len(invalidPadding)-1] = 0
	for _, data := range [][]byte{{0x80}, {0x90, 0}, invalidVersion, invalidPadding, valid} {
		if _, err := conn.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case got := <-sess.VideoPacketQueue:
		if !bytes.Equal(got.Payload, payload) || got.Sequence != 77 || got.SampleRate != 90000 {
			t.Fatalf("RTP payload/header corrupted: len=%d seq=%d rate=%d", len(got.Payload), got.Sequence, got.SampleRate)
		}
	case <-time.After(time.Second):
		t.Fatal("valid RTP after malformed packets was not received")
	}
	select {
	case got := <-sess.VideoPacketQueue:
		t.Fatalf("malformed RTP was accepted: %+v", got)
	case <-time.After(50 * time.Millisecond):
	}
}
