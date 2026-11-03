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

// dcsfFallbackClient dùng khi DCSFPool là nil (không cấu hình).
var dcsfFallbackClient = newDCSFHTTPClient()

func newDCSFHTTPClient() *http.Client {
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
	clients map[string]*http.Client // key: "host:port"
}

// NewDCSFPool tạo DCSFPool và pre-populate với danh sách addr từ config.
// addrs là []string "host:port" từ config.ToDCSFAddrs().
// Pool rỗng hợp lệ — host mới sẽ tự động tạo pool khi gặp lần đầu.
func NewDCSFPool(addrs []string) *DCSFPool {
	clients := make(map[string]*http.Client, len(addrs))
	for _, addr := range addrs {
		clients[addr] = newDCSFHTTPClient()
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
// Nếu chưa có trong pool → tự động tạo mới và lưu lại (lazy init, thread-safe).
// Nil receiver trả về dcsfFallbackClient.
func (p *DCSFPool) clientFor(rawURL string) *http.Client {
	if p == nil {
		return dcsfFallbackClient
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return dcsfFallbackClient
	}
	host := parsed.Host

	p.mu.RLock()
	c, ok := p.clients[host]
	p.mu.RUnlock()
	if ok {
		return c
	}

	// Host chưa có trong pool → tạo mới (double-checked locking).
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok = p.clients[host]; ok {
		return c
	}
	c = newDCSFHTTPClient()
	p.clients[host] = c
	zap.L().Info("dcsf: new pool created for host", zap.String("host", host))
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
