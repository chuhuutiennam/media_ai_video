package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"media-ai-gateway/internal/pipeline"
)

// fakeRecvClient: Recv trả các sự kiện test đẩy vào; Close (khi cặp send/recv kết thúc) mở khoá Recv.
type fakeRecvClient struct {
	events chan fakeRecvEvent
	closed chan struct{}
	once   sync.Once
}

type fakeRecvEvent struct {
	r   pipeline.RecognitionResult
	err error
}

func newFakeRecvClient() *fakeRecvClient {
	return &fakeRecvClient{events: make(chan fakeRecvEvent, 4), closed: make(chan struct{})}
}

func (c *fakeRecvClient) Send(pipeline.VideoFrame) error { return nil }
func (c *fakeRecvClient) CloseSend() error               { return nil }
func (c *fakeRecvClient) Close() error                   { c.once.Do(func() { close(c.closed) }); return nil }
func (c *fakeRecvClient) Recv() (pipeline.RecognitionResult, error) {
	select {
	case e := <-c.events:
		return e.r, e.err
	case <-c.closed:
		return pipeline.RecognitionResult{}, context.Canceled
	}
}

func (c *fakeRecvClient) result() {
	c.events <- fakeRecvEvent{r: pipeline.RecognitionResult{Kind: "video_mask", VideoMask: &pipeline.VideoMask{Status: "ok"}}}
}
func (c *fakeRecvClient) fail(code codes.Code) {
	c.events <- fakeRecvEvent{err: status.Error(code, "worker gone")}
}

// fakeReconnectDialer: lần Dial thứ i trả plan[i] (nil = mở được client mới), quá plan thì mở được.
type fakeReconnectDialer struct {
	mu      sync.Mutex
	plan    []error
	dials   int
	clients chan *fakeRecvClient
}

func newFakeReconnectDialer(plan ...error) *fakeReconnectDialer {
	return &fakeReconnectDialer{plan: plan, clients: make(chan *fakeRecvClient, 8)}
}

func (d *fakeReconnectDialer) Dial(context.Context, string, string, VideoStreamConfig) (VideoStreamClient, error) {
	d.mu.Lock()
	i := d.dials
	d.dials++
	var err error
	if i < len(d.plan) {
		err = d.plan[i]
	}
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	c := newFakeRecvClient()
	d.clients <- c
	return c, nil
}

func (d *fakeReconnectDialer) dialCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials
}

func (d *fakeReconnectDialer) next(t *testing.T) *fakeRecvClient {
	t.Helper()
	select {
	case c := <-d.clients:
		return c
	case <-time.After(2 * time.Second):
		t.Fatalf("stream was not (re)opened; dials=%d", d.dialCount())
		return nil
	}
}

func receive(t *testing.T, out <-chan pipeline.RecognitionResult) {
	t.Helper()
	select {
	case <-out:
	case <-time.After(2 * time.Second):
		t.Fatal("result not forwarded")
	}
}

// stopReasons trả lý do của các log "ai: video stream stopped".
func stopReasons(s *logSink) []string {
	s.mu.Lock()
	data := append([]byte(nil), s.buf.Bytes()...)
	s.mu.Unlock()
	var out []string
	for _, line := range bytes.Split(data, []byte("\n")) {
		var e map[string]any
		if json.Unmarshal(line, &e) == nil && e["msg"] == "ai: video stream stopped" {
			out = append(out, e["reason"].(string))
		}
	}
	return out
}

func openTestVideoStream(t *testing.T, cfg Config, d *fakeReconnectDialer) (*VideoManager, chan pipeline.RecognitionResult, context.CancelFunc) {
	t.Helper()
	cfg.MaxActiveStreams = 4
	if cfg.RetryBackoff == 0 {
		cfg.RetryBackoff = time.Millisecond
	}
	m := NewVideoManager(cfg, d)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	out := make(chan pipeline.RecognitionResult, 8)
	if _, err := m.Open(ctx, "s1", "s1", VideoStreamConfig{}, make(chan pipeline.VideoFrame), out); err != nil {
		t.Fatal(err)
	}
	return m, out, cancel
}

// Worker restart: lần mở lại đầu tiên lỗi (worker chưa lên) thì vẫn thử tiếp, không bỏ cuộc.
func TestVideoReconnect_ReopenFailureKeepsRetrying(t *testing.T) {
	logs := observeLogs(t)
	d := newFakeReconnectDialer(nil, errors.New("no reachable worker"))
	m, out, _ := openTestVideoStream(t, Config{MaxRetries: 3}, d)
	d.next(t).fail(codes.Unavailable)
	d.next(t).result() // lần mở lại thứ 2 thành công
	receive(t, out)
	if m.Count() != 1 || m.Stats().StreamsStopped != 0 || len(stopReasons(logs)) != 0 {
		t.Fatalf("stream must survive a failed reopen: active=%d stats=%+v reasons=%v", m.Count(), m.Stats(), stopReasons(logs))
	}
}

// max_retry là số lỗi liên tiếp: sau khi stream mở lại có kết quả, sự cố sau lại có đủ lượt.
func TestVideoReconnect_RetriesResetAfterResult(t *testing.T) {
	d := newFakeReconnectDialer()
	m, out, _ := openTestVideoStream(t, Config{MaxRetries: 1}, d)
	c := d.next(t)
	for incident := 1; incident <= 3; incident++ {
		c.fail(codes.Unavailable)
		c = d.next(t)
		c.result()
		receive(t, out)
	}
	if m.Count() != 1 || m.Stats().StreamsStopped != 0 {
		t.Fatalf("3 separate incidents with max_retry=1 must not stop the stream: active=%d stats=%+v", m.Count(), m.Stats())
	}
}

func TestVideoReconnect_StopsWithReason(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    Config
		plan   []error
		code   codes.Code
		reason string
		dials  int
	}{
		{"hết lượt (mở lại lỗi liên tiếp)", Config{MaxRetries: 2}, []error{nil, errors.New("down"), errors.New("down")}, codes.Unavailable, videoStopRetriesExhausted, 3},
		{"max_retry 0", Config{MaxRetries: 0}, nil, codes.Unavailable, videoStopRetriesExhausted, 1},
		{"lỗi vĩnh viễn", Config{MaxRetries: 3}, nil, codes.InvalidArgument, videoStopPermanentError, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := observeLogs(t)
			d := newFakeReconnectDialer(tc.plan...)
			m, _, _ := openTestVideoStream(t, tc.cfg, d)
			d.next(t).fail(tc.code)
			if !waitUntil(2*time.Second, func() bool { return m.Count() == 0 }) {
				t.Fatal("stream must stop")
			}
			if got := stopReasons(logs); len(got) != 1 || got[0] != tc.reason || m.Stats().StreamsStopped != 1 || d.dialCount() != tc.dials {
				t.Fatalf("reasons=%v stopped=%d dials=%d, want [%s] 1 %d", got, m.Stats().StreamsStopped, d.dialCount(), tc.reason, tc.dials)
			}
		})
	}
}

// stream_timeout_sec > 0 vẫn là giới hạn cứng nhưng có log + metric; session đóng thì không.
func TestVideoReconnect_TimeoutLoggedSessionCloseSilent(t *testing.T) {
	logs := observeLogs(t)
	d := newFakeReconnectDialer()
	m, _, _ := openTestVideoStream(t, Config{MaxRetries: 3, StreamTimeout: 50 * time.Millisecond}, d)
	d.next(t)
	if !waitUntil(2*time.Second, func() bool { return m.Count() == 0 }) {
		t.Fatal("stream must end at stream timeout")
	}
	if got := stopReasons(logs); len(got) != 1 || got[0] != videoStopStreamTimeout || m.Stats().StreamsStopped != 1 {
		t.Fatalf("timeout: reasons=%v stats=%+v", got, m.Stats())
	}

	logs = observeLogs(t)
	d = newFakeReconnectDialer()
	m, _, cancel := openTestVideoStream(t, Config{MaxRetries: 3}, d)
	d.next(t)
	cancel()
	if !waitUntil(2*time.Second, func() bool { return m.Count() == 0 }) {
		t.Fatal("stream must end with the session")
	}
	if got := stopReasons(logs); len(got) != 0 || m.Stats().StreamsStopped != 0 {
		t.Fatalf("session close must not count as stopped: reasons=%v stats=%+v", got, m.Stats())
	}
}

func TestVideoRetryBackoff(t *testing.T) {
	for _, tc := range []struct {
		base    time.Duration
		attempt int
		want    time.Duration
	}{
		{time.Second, 1, time.Second}, {time.Second, 2, 2 * time.Second}, {time.Second, 3, 4 * time.Second},
		{time.Second, 10, 30 * time.Second}, {0, 1, time.Second}, {20 * time.Second, 2, 30 * time.Second},
	} {
		if got := videoRetryBackoff(tc.base, tc.attempt); got != tc.want {
			t.Errorf("videoRetryBackoff(%v, %d) = %v, want %v", tc.base, tc.attempt, got, tc.want)
		}
	}
}
