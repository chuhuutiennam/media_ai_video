package controlplane

import (
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

func availableUDPRange(t *testing.T, count int) (int, int) {
	t.Helper()
	for attempt := 0; attempt < 100; attempt++ {
		first, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		start := first.LocalAddr().(*net.UDPAddr).Port
		sockets := []net.PacketConn{first}
		for port := start + 1; port < start+count && port <= 65535; port++ {
			sock, err := net.ListenPacket("udp", fmt.Sprintf("127.0.0.1:%d", port))
			if err != nil {
				break
			}
			sockets = append(sockets, sock)
		}
		ok := len(sockets) == count
		for _, sock := range sockets {
			sock.Close()
		}
		if ok {
			return start, start + count - 1
		}
	}
	t.Fatal("cannot find a free UDP range")
	return 0, 0
}

func waitPortsReleased(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.portAlloc.Available() >= s.portAlloc.Total() {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got := s.portAlloc.Available(); got != s.portAlloc.Total() {
		t.Fatalf("ports available %d, want %d", got, s.portAlloc.Total())
	}
	seen := map[int]bool{}
	for i := 0; i < s.portAlloc.Total(); i++ {
		p, err := s.portAlloc.Acquire()
		if err != nil || seen[p] {
			t.Fatalf("duplicate/missing released port %d: %v", p, err)
		}
		seen[p] = true
	}
	for p := range seen {
		s.portAlloc.Release(p)
	}
}

func TestVideoAnswerRollback(t *testing.T) {
	for _, tc := range []struct {
		name               string
		ports              int
		startErr, videoErr error
		occupied           bool
		want               int
		videoFailAt        int
	}{
		{name: "port exhaustion", ports: 1, want: http.StatusServiceUnavailable},
		{name: "second bind fails", ports: 2, occupied: true, want: http.StatusInternalServerError},
		{name: "coordinator fails", ports: 2, startErr: fmt.Errorf("start failed"), want: http.StatusBadGateway},
		{name: "video AI fails", ports: 2, videoErr: fmt.Errorf("AI unavailable"), want: http.StatusBadGateway},
		{name: "second video AI fails", ports: 2, videoErr: fmt.Errorf("second AI unavailable"), videoFailAt: 2, want: http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start, end := availableUDPRange(t, tc.ports)
			if tc.occupied {
				sock, err := net.ListenPacket("udp", fmt.Sprintf("127.0.0.1:%d", start))
				if err != nil {
					t.Fatal(err)
				}
				defer sock.Close()
			}
			s := newTestServerWithVideo(t, &fakeStarter{err: tc.startErr, videoErr: tc.videoErr, videoFailAt: tc.videoFailAt}, start, end)
			// Rollback cần dịch vụ 2 leg: cấu hình change_background với legs: both.
			s.cfg.VideoServices["change_background"] = VideoService{Enabled: true, EffectType: "bg_replace", AITask: VideoTaskSegment}
			w := postJSON(t, s, notifyPath(testCallID), SessionEvent{Event: "ANSWER", SelectedService: "change_background"})
			assertStatus(t, w, tc.want)
			if s.sessionMgr.Count() != 0 {
				t.Fatal("failed ANSWER left sessions alive")
			}
			waitPortsReleased(t, s)
		})
	}
}

func TestVideoAnswerDisabledAndMissingPorts(t *testing.T) {
	for _, tc := range []struct {
		enabled bool
		want    int
	}{{false, http.StatusBadRequest}, {true, http.StatusServiceUnavailable}} {
		s := newTestServer(t, &fakeStarter{})
		s.cfg.VideoEnabled = tc.enabled
		w := postJSON(t, s, notifyPath(testCallID), SessionEvent{Event: "ANSWER", SelectedService: "change_background"})
		assertStatus(t, w, tc.want)
		if s.sessionMgr.Count() != 0 {
			t.Fatal("invalid config created sessions")
		}
	}
}

func TestGestureOnlyAnswerAllocatesVideoWithoutBackground(t *testing.T) {
	starter := &fakeStarter{}
	start, end := availableUDPRange(t, 4)
	s := newTestServerWithVideo(t, starter, start, end)
	w := postJSON(t, s, notifyPath(testCallID), SessionEvent{Event: "ANSWER", SelectedService: "video_effects"})
	assertStatus(t, w, http.StatusCreated)
	var response SessionResponse
	decodeJSON(t, w, &response)
	// video_effects chỉ tạo leg tcore (legs: tcore), giống change_background.
	if response.TCoreVideoRTPPort == 0 || response.TAccessVideoRTPPort != 0 || response.TCoreRTPPort != 0 || response.TAccessRTPPort != 0 {
		t.Fatalf("gesture-only allocated the wrong media ports: %+v", response)
	}
	if len(starter.sessions) != 1 || starter.sessions[0].ID != testCallID+"-tcore" || starter.videoStarted != 1 {
		t.Fatalf("gesture-only must start only the tcore video leg, got %d sessions / %d video", len(starter.sessions), starter.videoStarted)
	}
	if _, ok := s.sessionMgr.Get(testCallID + "-taccess"); ok {
		t.Fatal("gesture-only created a taccess session")
	}
	for _, sess := range starter.sessions {
		defer s.sessionMgr.Close(sess.ID)
		if sess.Task != "video_effects" || sess.Codec != "" || sess.EffectType != "" || !sess.VideoEnabled || sess.VideoAITask != VideoTaskGesture {
			t.Fatalf("gesture-only session requested audio/background: %+v", sess)
		}
	}
}
