package webrtc

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDCProxy_Forward_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newDCProxy("sess-1", srv.URL)
	if err := p.forward(context.Background(), []byte(`{"key":"value"}`), true); err != nil {
		t.Errorf("forward() unexpected error: %v", err)
	}
}

func TestDCProxy_Forward_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := newDCProxy("sess-1", srv.URL)
	err := p.forward(context.Background(), []byte("data"), false)
	if err == nil {
		t.Error("forward() expected error for 500 response, got nil")
	}
}

func TestDCProxy_Forward_ContentType_Text(t *testing.T) {
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newDCProxy("sess-1", srv.URL)
	if err := p.forward(context.Background(), []byte(`{}`), true); err != nil {
		t.Fatalf("forward() unexpected error: %v", err)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want %q", gotContentType, "application/json")
	}
}

func TestDCProxy_Forward_ContentType_Binary(t *testing.T) {
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newDCProxy("sess-1", srv.URL)
	if err := p.forward(context.Background(), []byte{0x01, 0x02}, false); err != nil {
		t.Fatalf("forward() unexpected error: %v", err)
	}
	if gotContentType != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want %q", gotContentType, "application/octet-stream")
	}
}

func TestDCProxy_Forward_SessionIDHeader(t *testing.T) {
	const wantSessID = "my-session-42"
	var gotSessID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSessID = r.Header.Get("X-Session-ID")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newDCProxy(wantSessID, srv.URL)
	if err := p.forward(context.Background(), []byte("hello"), true); err != nil {
		t.Fatalf("forward() unexpected error: %v", err)
	}
	if gotSessID != wantSessID {
		t.Errorf("X-Session-ID = %q, want %q", gotSessID, wantSessID)
	}
}

func TestDCProxy_Forward_BodyForwarded(t *testing.T) {
	const payload = `{"msg":"test"}`
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newDCProxy("sess-1", srv.URL)
	if err := p.forward(context.Background(), []byte(payload), true); err != nil {
		t.Fatalf("forward() unexpected error: %v", err)
	}
	if string(gotBody) != payload {
		t.Errorf("body = %q, want %q", gotBody, payload)
	}
}
