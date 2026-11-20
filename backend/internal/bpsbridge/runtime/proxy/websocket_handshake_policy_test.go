package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestWebSocketHandshakeDiagnosticRemainsSingleAttempt(t *testing.T) {
	s := wsBridge(t, &httptest.Server{URL: "http://127.0.0.1:1"}, nil)
	var attempts atomic.Int32
	s.client.Transport = handshakeRoundTripper(func(*http.Request) (*http.Response, error) { attempts.Add(1); return nil, io.ErrUnexpectedEOF })
	ctx := withProtocolObserver(context.Background(), protocolObserver{})
	body, err := s.openWebSocket(ctx, []byte(`{"model":"test"}`), "test-token", "test-account")
	if body != nil {
		_ = body.Close()
		t.Fatal("unexpected success")
	}
	if err == nil || attempts.Load() != 1 {
		t.Fatalf("diagnostic retried: %v attempts=%d", err, attempts.Load())
	}
}

func TestWebSocketHandshakeInvalidUpgradeNeverRetries(t *testing.T) {
	var attempts atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Upgrade", "websocket")
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Sec-WebSocket-Accept", "DO_NOT_LEAK")
		w.WriteHeader(http.StatusSwitchingProtocols)
	}))
	defer up.Close()
	rec := postResponses(t, wsBridge(t, up, nil).Handler(), basicBody())
	if rec.Code != 502 || attempts.Load() != 1 || !strings.Contains(rec.Body.String(), "reason=invalid_upgrade") || strings.Contains(rec.Body.String(), "DO_NOT_LEAK") {
		t.Fatalf("unsafe invalid-upgrade handling: %d attempts=%d %s", rec.Code, attempts.Load(), rec.Body.String())
	}
}
