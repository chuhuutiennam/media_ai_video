package webrtc

import (
	"bytes"
	"context"
	"fmt"
	"net/http"

	pion "github.com/pion/webrtc/v4"
)

// dcProxy implements IMS Data Channel Mode 2.1 (HTTP Proxy):
//
//	UE → SCTP/DTLS DataChannel → Gateway → HTTP POST → DC Application Server
//
// When a message arrives on the DataChannel, dcProxy forwards it to a
// configured URL via HTTP POST, preserving the Content-Type and adding an
// X-Session-ID header so the application server can correlate messages.
type dcProxy struct {
	sessID string
	url    string
	client *http.Client
}

// newDCProxy creates a dcProxy that POSTs incoming DataChannel messages to proxyURL.
func newDCProxy(sessID, proxyURL string) *dcProxy {
	return &dcProxy{
		sessID: sessID,
		url:    proxyURL,
		client: &http.Client{},
	}
}

// wire registers dc.OnMessage so that every inbound message is forwarded via
// HTTP in its own goroutine. The goroutine uses the provided context so that
// in-flight requests are cancelled when the session is torn down.
func (p *dcProxy) wire(ctx context.Context, dc *pion.DataChannel) {
	dc.OnMessage(func(msg pion.DataChannelMessage) {
		go func() {
			_ = p.forward(ctx, msg.Data, msg.IsString)
		}()
	})
}

// forward POSTs data to p.url.
// isText=true → Content-Type: application/json
// isText=false → Content-Type: application/octet-stream
// Returns an error if the server responds with HTTP >= 300.
func (p *dcProxy) forward(ctx context.Context, data []byte, isText bool) error {
	contentType := "application/octet-stream"
	if isText {
		contentType = "application/json"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Session-ID", p.sessID)

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("dcProxy: upstream returned %d", resp.StatusCode)
	}
	return nil
}
