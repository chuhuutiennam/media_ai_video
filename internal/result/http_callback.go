package result

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"media-ai-gateway/internal/pipeline"

	"go.uber.org/zap"
	"golang.org/x/net/http2"
)

// dualH2Transport route request sang h2c transport (http://) hoặc TLS transport (https://).
// Cho phép 1 shared http.Client xử lý cả 2 scheme mà không cần tạo riêng per-sink.
type dualH2Transport struct {
	h2c   *http2.Transport // AllowHTTP + plain TCP dial
	h2tls *http2.Transport // standard TLS via ALPN
}

func (d *dualH2Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == "https" {
		return d.h2tls.RoundTrip(req)
	}
	return d.h2c.RoundTrip(req)
}

// CallbackHTTPClient là shared HTTP/2 client dùng chung toàn bộ gateway.
// http2.Transport pool connection per host — tất cả session cùng callback server
// tự động reuse 1 kết nối H/2 duy nhất.
// ReadIdleTimeout và PingTimeout đảm bảo connection được giữ sống khi idle.
type CallbackHTTPClient struct {
	client         *http.Client
	attemptTimeout time.Duration // per-request timeout forwarded to new sinks

	mu         sync.Mutex
	preconnURL string    // URL đã preconnect (rỗng nếu chưa gọi Preconnect)
	preconnOK  bool      // true nếu lần preconnect gần nhất thành công
	preconnErr string    // thông báo lỗi nếu preconnect thất bại
	preconnAt  time.Time // thời điểm preconnect được thực hiện

	taskMu                       sync.Mutex
	taskClients                  map[string]*http.Client
	readIdleTimeout, pingTimeout time.Duration
}

// PreconnectStatus là snapshot trạng thái preconnect, dùng cho health/connections API.
type PreconnectStatus struct {
	URL          string    `json:"url"`
	Connected    bool      `json:"connected"`
	Error        string    `json:"error,omitempty"`
	PreconnectAt time.Time `json:"preconnect_at,omitempty"`
}

// PreconnectState trả về snapshot trạng thái preconnect tại thời điểm gọi.
func (c *CallbackHTTPClient) PreconnectState() PreconnectStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return PreconnectStatus{
		URL:          c.preconnURL,
		Connected:    c.preconnOK,
		Error:        c.preconnErr,
		PreconnectAt: c.preconnAt,
	}
}

// NewCallbackHTTPClient tạo shared client với H/2 keep-alive settings.
//   - timeout: max duration mỗi HTTP request (gồm cả retry)
//   - readIdleTimeout: gửi PING sau khoảng idle này; 0 = không gửi PING
//   - pingTimeout: đóng connection nếu không nhận PONG; 0 = dùng default golang
func NewCallbackHTTPClient(timeout, readIdleTimeout, pingTimeout time.Duration) *CallbackHTTPClient {
	h2c := &http2.Transport{
		AllowHTTP:       true,
		ReadIdleTimeout: readIdleTimeout,
		PingTimeout:     pingTimeout,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	h2tls := &http2.Transport{
		ReadIdleTimeout: readIdleTimeout,
		PingTimeout:     pingTimeout,
	}
	return &CallbackHTTPClient{
		readIdleTimeout: readIdleTimeout, pingTimeout: pingTimeout,
		client: &http.Client{
			Transport: &dualH2Transport{h2c: h2c, h2tls: h2tls},
			Timeout:   timeout,
		},
		attemptTimeout: timeout,
	}
}

// Preconnect thiết lập kết nối H/2 tới host của rawURL ngay lập tức.
// Gọi khi app khởi động để warm up connection pool trước khi có session đầu tiên.
// Không fatal nếu server chưa sẵn sàng — chỉ log warning.
func (c *CallbackHTTPClient) Preconnect(ctx context.Context, rawURL string) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		zap.L().Warn("callback: preconnect: invalid url", zap.String("url", rawURL), zap.Error(err))
		c.recordPreconnect(rawURL, false, err.Error())
		return
	}
	target := parsed.Scheme + "://" + parsed.Host + "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, target, nil)
	if err != nil {
		zap.L().Warn("callback: preconnect: build request failed", zap.String("url", target), zap.Error(err))
		c.recordPreconnect(target, false, err.Error())
		return
	}
	resp, err := c.client.Do(req)
	if err != nil {
		zap.L().Warn("callback: preconnect failed — will retry on first callback", zap.String("url", target), zap.Error(err))
		c.recordPreconnect(target, false, err.Error())
		return
	}
	resp.Body.Close()
	zap.L().Info("callback: preconnected via HTTP/2", zap.String("url", target), zap.Int("status", resp.StatusCode))
	c.recordPreconnect(target, true, "")
}

func (c *CallbackHTTPClient) recordPreconnect(url string, ok bool, errMsg string) {
	c.mu.Lock()
	c.preconnURL = url
	c.preconnOK = ok
	c.preconnErr = errMsg
	c.preconnAt = time.Now()
	c.mu.Unlock()
}

// NewHTTPCallbackSink tạo sink dùng shared HTTP/2 client.
// Dùng trong production — nhiều session cùng callback host tái sử dụng 1 kết nối H/2.
func (c *CallbackHTTPClient) NewHTTPCallbackSink(rawURL string, maxRetry int) *HTTPCallbackSink {
	return &HTTPCallbackSink{
		URL:            rawURL,
		MaxRetry:       maxRetry,
		RetryBackoff:   200 * time.Millisecond,
		AttemptTimeout: c.attemptTimeout,
		client:         c.client,
		pool:         	c,
	}
}

// callbackPayload bọc RecognitionResult với field event_type cho callback body.
// Embedding đảm bảo tất cả field của RecognitionResult được flatten vào JSON.
// ContextID và TerminationID được flatten từ MediaResources.TAccess để consumer
// không phải dig vào nested object.
type callbackPayload struct {
	EventType     string `json:"event_type"`
	ContextID     string `json:"contextId,omitempty"`
	TerminationID string `json:"terminationId,omitempty"`
	pipeline.RecognitionResult
}

func eventTypeOf(r pipeline.RecognitionResult) string {
	if r.IsVideoGesture() {
		return "video.gesture.result"
	}
	if r.IsVideoMask() {
		return "video.mask.result"
	}
	if r.IsFinal {
		return "asr.transcript.final"
	}
	return "asr.transcript.partial"
}

func newCallbackPayload(r pipeline.RecognitionResult) callbackPayload {
	p := callbackPayload{
		EventType:         eventTypeOf(r),
		RecognitionResult: r,
	}
	if r.MediaResources != nil {
		mr := r.MediaResources.TAccess
		if mr == nil {
			mr = r.MediaResources.TCore
		}
		if mr != nil {
			p.ContextID = mr.ContextID
			p.TerminationID = mr.TerminationID
		}
	}
	return p
}

// HTTPCallbackSink POST RecognitionResult tới một HTTP/2 endpoint.
//
// Transport:
//   - http:// URL → H2C (cleartext HTTP/2, prior-knowledge).
//   - https:// URL → HTTP/2 over TLS (ALPN negotiation).
//
// Retry policy:
//   - HTTP 5xx → retry với constant backoff, tối đa MaxRetry lần.
//   - HTTP 4xx → không retry (lỗi client, không tự phục hồi được).
//   - Network error → retry.
//
// Dead-letter: khi tất cả retry thất bại, result được forward sang DeadLetter sink.
// DeadLetter = nil → drop và trả về lỗi cuối (hành vi cũ).
type HTTPCallbackSink struct {
	URL            string
	MaxRetry       int
	RetryBackoff   time.Duration // wait cố định giữa các lần retry; mặc định 200ms
	AttemptTimeout time.Duration // per-attempt timeout; 0 = dùng client.Timeout
	DeadLetter     Sink          // optional; nhận result khi hết retry
	client         *http.Client
	retries        atomic.Uint64 // tổng số lần retry đã thực hiện
	pool 		   *CallbackHTTPClient // shared per-task transports, pooled across sessions
}

// Retries trả về tổng số lần retry đã thực hiện (không tính lần gửi đầu tiên).
func (s *HTTPCallbackSink) Retries() uint64 { return s.retries.Load() }

// NewHTTPCallbackSink tạo HTTPCallbackSink.
// timeout áp dụng cho từng HTTP/2 request; maxRetry là số lần retry (không kể lần đầu).
func NewHTTPCallbackSink(rawURL string, timeout time.Duration, maxRetry int) *HTTPCallbackSink {
	pool := &CallbackHTTPClient{client: &http.Client{Transport: newH2Transport(rawURL), Timeout: timeout}}
	return pool.NewHTTPCallbackSink(rawURL, maxRetry)
}

// clientFor uses independent connection pools for the video tasks, including
// when they target the same host. A slow task cannot consume the other task's
// HTTP/2 concurrent-stream slots or connection-level flow-control window.
func (c *CallbackHTTPClient) clientFor(r pipeline.RecognitionResult) *http.Client {
	task := ""
	if r.IsVideoGesture() {
		task = "gesture_recognition"
	} else if r.IsVideoMask() {
		task = "video_segment"
 	}
	if task == "" {
		return c.client
	}
	c.taskMu.Lock()
	defer c.taskMu.Unlock()
	if c.taskClients == nil {
		c.taskClients = make(map[string]*http.Client)
	}
	if c.taskClients[task] == nil {
		c.taskClients[task] = NewCallbackHTTPClient(c.client.Timeout, c.readIdleTimeout, c.pingTimeout).client
	}
	return c.taskClients[task]
}

// newH2Transport trả về HTTP/2 transport phù hợp với scheme của URL.
//
//   - http:// → H2C (AllowHTTP + DialTLSContext bypass TLS).
//     http2.Transport.tlsConfig() luôn trả về non-nil *tls.Config nên phải
//     override DialTLSContext để kết nối plain TCP thay vì TLS.
//   - https:// → http2.Transport mặc định, tự negotiate H2 qua ALPN.
func newH2Transport(rawURL string) http.RoundTripper {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" {
		return &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		}
	}
	return &http2.Transport{}
}

func (s *HTTPCallbackSink) Type() string { return "http_callback" }

// Send POST result tới s.URL với retry. Context-aware: dừng ngay khi ctx done.
// Khi hết retry mà vẫn lỗi, forward sang DeadLetter sink nếu có.
func (s *HTTPCallbackSink) Send(ctx context.Context, r pipeline.RecognitionResult) error {
	body, err := json.Marshal(newCallbackPayload(r))
	if err != nil {
		return fmt.Errorf("http_callback: marshal: %w", err)
	}

	zap.L().Debug("callback: sending",
		zap.String("url", s.URL),
		zap.String("session_id", r.SessionID),
		zap.Bool("is_final", r.IsFinal),
		zap.Uint64("seq", r.Seq),
		zap.Int("body_bytes", len(body)),
	)

	var lastErr error
	for attempt := 0; attempt <= s.MaxRetry; attempt++ {
		if attempt > 0 {
			s.retries.Add(1)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(s.RetryBackoff):
			}
		}

		reqCtx := ctx
		reqCancel := func() {}
		if s.AttemptTimeout > 0 {
			reqCtx, reqCancel = context.WithTimeout(ctx, s.AttemptTimeout)
		}
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, s.URL, bytes.NewReader(body))
		if err != nil {
			reqCancel()
			return fmt.Errorf("http_callback: build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		client := s.client
		if s.pool != nil {
			client = s.pool.clientFor(r)
		}
		resp, err := client.Do(req)
		reqCancel()
		if err != nil {
			lastErr = fmt.Errorf("http_callback: request: %w", err)
			continue // network error → retry
		}
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		lastErr = fmt.Errorf("http_callback: status %d", resp.StatusCode)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return lastErr // 4xx: client error, không retry
		}
		// 5xx: server error → retry
	}

	// Hết retry: forward sang dead-letter sink nếu có.
	if s.DeadLetter != nil {
		return s.DeadLetter.Send(ctx, r)
	}
	return lastErr
}
