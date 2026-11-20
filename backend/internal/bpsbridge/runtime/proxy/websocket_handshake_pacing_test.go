package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestWebSocketHandshakeRetriesRespectPacingDeadlineAndNormalClose(t *testing.T) {
	var creates atomic.Int32
	closed := make(chan websocket.StatusCode, 1)
	up := wsMock(t, func(ctx context.Context, c *websocket.Conn, _ map[string]any, _ *http.Request) {
		creates.Add(1)
		completeWS(t, ctx, c, nil)
		_, _, err := c.Read(ctx)
		closed <- websocket.CloseStatus(err)
	})
	s := wsBridge(t, up, func(c *Config) { c.UpstreamRPS = 1 })
	defer s.CloseStartRate()
	base := s.client.Transport
	t.Cleanup(s.client.CloseIdleConnections)
	var starts, deadlines []time.Time
	s.client.Transport = handshakeRoundTripper(func(r *http.Request) (*http.Response, error) {
		starts = append(starts, time.Now())
		deadline, ok := r.Context().Deadline()
		if !ok {
			t.Error("handshake has no deadline")
		}
		deadlines = append(deadlines, deadline)
		if len(starts) == 1 {
			return nil, io.ErrUnexpectedEOF
		}
		return base.RoundTrip(r)
	})
	rec := postResponses(t, s.Handler(), basicBody())
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "response.completed") || len(starts) != 2 || creates.Load() != 1 {
		t.Fatalf("retry result: status=%d attempts=%d creates=%d body=%s", rec.Code, len(starts), creates.Load(), rec.Body.String())
	}
	if starts[1].Sub(starts[0]) < 900*time.Millisecond {
		t.Fatal("retry bypassed the one-dial-per-second window")
	}
	if !deadlines[0].Equal(deadlines[1]) {
		t.Fatal("retry reset the handshake deadline")
	}
	waitMS, err := strconv.Atoi(rec.Header().Get("X-BPS-Handshake-Rate-Wait-Ms"))
	if err != nil || waitMS < 500 {
		t.Fatalf("retry dial wait was not observed: %q", rec.Header().Get("X-BPS-Handshake-Rate-Wait-Ms"))
	}
	if rec.Header().Get("X-BPS-Ws-Handshake-Status") != "101" || rec.Header().Get("X-BPS-Ws-Create-Write-Done-Unix-Ms") == "" {
		t.Fatal("successful retry lost handshake/create observations")
	}
	select {
	case code := <-closed:
		if code != websocket.StatusNormalClosure {
			t.Fatalf("successful retry lost its close handle: close code=%d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("successful retry did not close the upgraded transport")
	}
}

func TestWebSocketHandshakeRetryQueueRespectsAuthorizationAndCancellation(t *testing.T) {
	for _, mode := range []string{"reject", "close", "cancel", "start-deadline"} {
		t.Run(mode, func(t *testing.T) {
			s := wsBridge(t, &httptest.Server{URL: "http://127.0.0.1:1"}, func(c *Config) { c.UpstreamRPS = 1 }).WithStartAuthorization("auth", 0)
			defer s.CloseStartRate()
			if mode == "start-deadline" {
				s = s.WithStartDeadline(time.Now().Add(600 * time.Millisecond))
			}
			var attempts atomic.Int32
			s.client.Transport = handshakeRoundTripper(func(*http.Request) (*http.Response, error) {
				attempts.Add(1)
				return nil, io.ErrUnexpectedEOF
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			timing := &webSocketTiming{}
			ctx = context.WithValue(ctx, webSocketTimingKey{}, timing)
			result := make(chan error, 1)
			go func() {
				body, err := s.openWebSocket(ctx, []byte(`{"model":"test"}`), "test-token", "acct-test")
				if body != nil {
					_ = body.Close()
				}
				result <- err
			}()
			waitDialCount(t, s, 1)
			wantCode := ""
			switch mode {
			case "reject":
				s.rejectStarts("acct-test")
				wantCode = "bps_start_authorization_blocked"
			case "close":
				s.CloseStartRate()
				wantCode = "bps_rate_limiter_closed"
			case "cancel":
				cancel()
			case "start-deadline":
				wantCode = "bps_rate_wait_timeout"
			}
			select {
			case err := <-result:
				var pacing *StartRateError
				if wantCode != "" && (!errors.As(err, &pacing) || pacing.Code != wantCode) {
					t.Fatalf("lost local pacing error: %v, want %s", err, wantCode)
				}
				if mode == "cancel" && !errors.Is(err, context.Canceled) {
					t.Fatalf("lost cancellation: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("queued handshake retry did not stop")
			}
			if attempts.Load() != 1 || !timing.writeStart.IsZero() || !timing.writeDone.IsZero() {
				t.Fatalf("queued retry escaped fencing or invented response.create: attempts=%d timing=%+v", attempts.Load(), timing)
			}
		})
	}
}
