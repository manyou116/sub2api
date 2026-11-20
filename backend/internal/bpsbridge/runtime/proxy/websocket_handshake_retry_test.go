package proxy

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type handshakeRoundTripper func(*http.Request) (*http.Response, error)

func (f handshakeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWebSocketHandshakeRetriesBeforeCreate(t *testing.T) {
	for _, failure := range []error{io.EOF, io.ErrUnexpectedEOF, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}} {
		t.Run(fmt.Sprintf("%T-%v", failure, failure), func(t *testing.T) {
			var attempts, creates atomic.Int32
			up := wsMock(t, func(ctx context.Context, c *websocket.Conn, _ map[string]any, _ *http.Request) {
				creates.Add(1)
				completeWS(t, ctx, c, []any{})
			})
			s := wsBridge(t, up, nil)
			base := s.client.Transport
			t.Cleanup(s.client.CloseIdleConnections)
			s.client.Transport = handshakeRoundTripper(func(r *http.Request) (*http.Response, error) {
				if attempts.Add(1) == 1 {
					return nil, failure
				}
				return base.RoundTrip(r)
			})
			rec := postResponses(t, s.Handler(), basicBody())
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), "response.completed") {
				t.Fatalf("transient handshake was not recovered: %d %s", rec.Code, rec.Body.String())
			}
			if attempts.Load() != 2 || creates.Load() != 1 {
				t.Fatalf("attempts=%d creates=%d; response.create must be sent once", attempts.Load(), creates.Load())
			}
		})
	}
}

func TestWebSocketHandshakeRetriesRealEarlyClose(t *testing.T) {
	var attempts, creates atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			conn, _, err := fixtureValue[http.Hijacker](w).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"responses"}, OriginPatterns: []string{"bps.openai.com"}})
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = c.CloseNow() }()
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		_, _, err = c.Read(ctx)
		if err != nil {
			t.Error(err)
			return
		}
		creates.Add(1)
		completeWS(t, ctx, c, []any{})
	}))
	defer up.Close()
	s := wsBridge(t, up, nil)
	rec := postResponses(t, s.Handler(), basicBody())
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "response.completed") || attempts.Load() != 2 || creates.Load() != 1 {
		t.Fatalf("real early close not recovered: status=%d attempts=%d creates=%d body=%s", rec.Code, attempts.Load(), creates.Load(), rec.Body.String())
	}
}

func TestWebSocketHandshakeRetryLimitAndRedaction(t *testing.T) {
	s := wsBridge(t, &httptest.Server{URL: "http://127.0.0.1:1"}, nil)
	var attempts atomic.Int32
	s.client.Transport = handshakeRoundTripper(func(r *http.Request) (*http.Response, error) {
		attempts.Add(1)
		return nil, &url.Error{Op: "Get", URL: "https://secret.invalid/?bearer=DO_NOT_LEAK", Err: fmt.Errorf("DO_NOT_LEAK: %w", io.ErrUnexpectedEOF)}
	})
	rec := postResponses(t, s.Handler(), basicBody())
	body := rec.Body.String()
	if rec.Code != 502 || attempts.Load() != 3 {
		t.Fatalf("expected bounded three handshake attempts: status=%d attempts=%d body=%s", rec.Code, attempts.Load(), body)
	}
	for _, want := range []string{"basispoints_transport_error", "reason=network_eof", "attempts=3", "request_not_sent=true"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing safe diagnosis %q: %s", want, body)
		}
	}
	if strings.Contains(body, "DO_NOT_LEAK") || strings.Contains(body, "secret.invalid") {
		t.Fatal("handshake diagnostic leaked raw error")
	}
}

func TestWebSocketHandshakeDoesNotRetryPolicyFailures(t *testing.T) {
	for _, failure := range []error{x509.UnknownAuthorityError{}, errors.New("unclassified DO_NOT_LEAK")} {
		t.Run(fmt.Sprintf("%T", failure), func(t *testing.T) {
			s := wsBridge(t, &httptest.Server{URL: "http://127.0.0.1:1"}, nil)
			var attempts atomic.Int32
			s.client.Transport = handshakeRoundTripper(func(*http.Request) (*http.Response, error) { attempts.Add(1); return nil, failure })
			rec := postResponses(t, s.Handler(), basicBody())
			if rec.Code != 502 || attempts.Load() != 1 || strings.Contains(rec.Body.String(), "DO_NOT_LEAK") {
				t.Fatalf("policy/unknown failure retried or leaked: %d %s attempts=%d", rec.Code, rec.Body.String(), attempts.Load())
			}
		})
	}
	for _, status := range []int{401, 403, 407, 429, 302, 200, 502, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var attempts atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.Header().Set("Retry-After", "17")
				w.WriteHeader(status)
			}))
			defer up.Close()
			rec := postResponses(t, wsBridge(t, up, nil).Handler(), basicBody())
			if attempts.Load() != 1 || rec.Header().Get("Retry-After") != "17" {
				t.Fatalf("HTTP rejection retried or hint lost: %d attempts=%d", rec.Code, attempts.Load())
			}
		})
	}
}

func TestWebSocketHandshakeCancellationStopsBackoff(t *testing.T) {
	s := wsBridge(t, &httptest.Server{URL: "http://127.0.0.1:1"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var attempts atomic.Int32
	s.client.Transport = handshakeRoundTripper(func(*http.Request) (*http.Response, error) {
		attempts.Add(1)
		cancel()
		return nil, io.ErrUnexpectedEOF
	})
	started := time.Now()
	body, err := s.openWebSocket(ctx, []byte(`{"model":"test"}`), "test-token", "test-account")
	if body != nil {
		_ = body.Close()
		t.Fatal("unexpected success")
	}
	if err == nil || attempts.Load() != 1 || time.Since(started) > 500*time.Millisecond {
		t.Fatalf("cancellation was retried or delayed: %v attempts=%d", err, attempts.Load())
	}
}
