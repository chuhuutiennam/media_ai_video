package controlplane

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"go.uber.org/zap"
	"golang.org/x/net/http2"
)

// dcsfCallControlRequest là body của POST /v1/dcsf/call-sessions/{callId}/call-control/{dcasId}/{callbackKey}.
type dcsfCallControlRequest struct {
	CallID      string `json:"callId"`
	Actions     string `json:"actions"`
	CallbackURL string `json:"callbackUrl,omitempty"` // URL DCAS nhận ctrl-result từ DCSF
}

// maxDynamicDCSFClients giới hạn số client tạo lazy cho host không có trong config (theo media_ai);
// host vượt giới hạn dùng chung client fallback để URL từ request không làm pool phình vô hạn.
const maxDynamicDCSFClients = 64

// dcsfFallbackClient dùng khi DCSFPool là nil hoặc đã đạt giới hạn client động (h2c cho http://).
var dcsfFallbackClient = newDCSFHTTPClient(false)

// dcsfFallbackTLSClient tương tự dcsfFallbackClient cho https:// (TLS thật).
var dcsfFallbackTLSClient = newDCSFHTTPClient(true)

// newDCSFHTTPClient tạo HTTP/2 client: h2c (cleartext) cho http://, TLS + ALPN h2 cho https://.
// Trước đây mọi URL đều mở TCP thường rồi nói h2c, nên DCSF dùng https không nhận được CALL_CTRL.
func newDCSFHTTPClient(useTLS bool) *http.Client {
	if useTLS {
		return &http.Client{Transport: &http2.Transport{}}
	}
	return &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
	}
}

// DCSFPool quản lý HTTP/2 client pool theo từng DCSF host:port.
// Host đã cấu hình được pre-warm khi khởi động; host mới gặp lần đầu
// sẽ tự động tạo http.Client và lưu vào pool để tái sử dụng.
type DCSFPool struct {
	mu      sync.RWMutex
	clients map[string]*http.Client // key: "host:port" (http) hoặc "https://host:port"
	dynamic int                     // số client tạo lazy (không tính host từ config)
}

// NewDCSFPool tạo DCSFPool và pre-populate với danh sách addr từ config.
// addrs là []string "host:port" từ config.ToDCSFAddrs().
// Pool rỗng hợp lệ — host mới sẽ tự động tạo pool khi gặp lần đầu.
func NewDCSFPool(addrs []string) *DCSFPool {
	clients := make(map[string]*http.Client, len(addrs))
	for _, addr := range addrs {
		clients[addr] = newDCSFHTTPClient(false)
	}
	return &DCSFPool{clients: clients}
}

// Preconnect warm-up H/2 connection đến tất cả host đã pre-populate.
// Gọi khi khởi động để tránh latency dial lần đầu. Lỗi chỉ log warning.
func (p *DCSFPool) Preconnect(ctx context.Context) {
	p.mu.RLock()
	addrs := make([]string, 0, len(p.clients))
	clients := make(map[string]*http.Client, len(p.clients))
	for addr, c := range p.clients {
		addrs = append(addrs, addr)
		clients[addr] = c
	}
	p.mu.RUnlock()

	for _, addr := range addrs {
		target := "http://" + addr + "/"
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, target, nil)
		if err != nil {
			zap.L().Warn("dcsf: preconnect: build request failed", zap.String("addr", addr), zap.Error(err))
			continue
		}
		resp, err := clients[addr].Do(req)
		if err != nil {
			zap.L().Warn("dcsf: preconnect failed — will retry on first call-control", zap.String("addr", addr), zap.Error(err))
			continue
		}
		resp.Body.Close()
		zap.L().Info("dcsf: preconnected via HTTP/2", zap.String("addr", addr), zap.Int("status", resp.StatusCode))
	}
}

// clientFor trả về http.Client cho host:port của rawURL.
// Nếu chưa có trong pool → tạo mới và lưu lại (lazy init, thread-safe), tối đa maxDynamicDCSFClients;
// vượt giới hạn dùng client fallback dùng chung. https:// dùng TLS thật, http:// dùng h2c.
// Nil receiver dùng client fallback.
func (p *DCSFPool) clientFor(rawURL string) *http.Client {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return dcsfFallbackClient
	}
	useTLS := strings.EqualFold(parsed.Scheme, "https")
	fallback := dcsfFallbackClient
	key := parsed.Host
	if useTLS {
		fallback = dcsfFallbackTLSClient
		key = "https://" + parsed.Host
	}
	if p == nil {
		return fallback
	}

	p.mu.RLock()
	c, ok := p.clients[key]
	p.mu.RUnlock()
	if ok {
		return c
	}

	// Host chưa có trong pool → tạo mới (double-checked locking).
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok = p.clients[key]; ok {
		return c
	}
	if p.dynamic >= maxDynamicDCSFClients {
		return fallback
	}
	c = newDCSFHTTPClient(useTLS)
	p.clients[key] = c
	p.dynamic++
	zap.L().Info("dcsf: new pool created for host", zap.String("host", key))
	return c
}

// SendCallControl POST CALL_CTRL đến DCSF tại dcsfURL với actions="duplicate".
// dcsfURL lấy từ trường callbackUrl trong ANSWER notify-event body (format: .../call-control/{dcasId}/{callbackKey}).
// ctrlResultURL là URL của DCAS để DCSF gửi ctrl-result trở lại; rỗng thì bỏ qua.
// Host chưa cấu hình sẽ tự động tạo pool mới. Nil receiver an toàn.
func (p *DCSFPool) SendCallControl(ctx context.Context, dcsfURL, callID, ctrlResultURL string) error {
	body, err := json.Marshal(dcsfCallControlRequest{
		CallID:      callID,
		Actions:     "duplicate",
		CallbackURL: ctrlResultURL,
	})
	if err != nil {
		return fmt.Errorf("dcsf: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dcsfURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("dcsf: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.clientFor(dcsfURL).Do(req)
	if err != nil {
		return fmt.Errorf("dcsf: request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errBody struct {
			Cause string `json:"cause"`
		}
		if b, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096)); readErr == nil {
			json.Unmarshal(b, &errBody) //nolint:errcheck
		}
		if errBody.Cause != "" {
			return fmt.Errorf("dcsf: call-control status %d: %s", resp.StatusCode, errBody.Cause)
		}
		return fmt.Errorf("dcsf: call-control status %d", resp.StatusCode)
	}
	zap.L().Debug("dcas→dcsf: call-control OK", zap.String("call_id", callID), zap.String("url", dcsfURL), zap.Int("status", resp.StatusCode))
	return nil
}
