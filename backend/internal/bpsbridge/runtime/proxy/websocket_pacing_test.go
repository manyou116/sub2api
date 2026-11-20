package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func waitDialCount(t *testing.T, s *Server, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.RateSnapshot("acct-test")["dial_waiting"] == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("dial waiters=%v, want %d", s.RateSnapshot("acct-test"), want)
}

func TestWebSocketSlowHandshakesDoNotSerializePermittedDials(t *testing.T) {
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"responses"}, OriginPatterns: []string{"bps.openai.com"}})
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		var body map[string]any
		if wsjson.Read(r.Context(), c, &body) == nil {
			completeWS(t, r.Context(), c, nil)
		}
	}))
	defer up.Close()
	s := wsBridge(t, up, func(c *Config) { c.UpstreamRPS = 2 })
	defer s.CloseStartRate()
	s = s.WithStartAuthorization("same-auth", 0)
	results := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() { results <- postResponses(t, s.Handler(), basicBody()) }()
	}
	for range 2 {
		select {
		case <-arrived:
		case <-time.After(2 * time.Second):
			unblock()
			t.Fatal("the first unfinished handshake serialized another permitted dial")
		}
	}
	unblock()
	for range 2 {
		rec := <-results
		if rec.Code != 200 {
			t.Fatalf("request failed: %d %s", rec.Code, rec.Body.String())
		}
		var previous int64
		for _, key := range []string{"Ws-Dial-Start-Unix-Ms", "Ws-Dial-Done-Unix-Ms", "Ws-Create-Write-Start-Unix-Ms", "Ws-Create-Write-Done-Unix-Ms"} {
			at, err := strconv.ParseInt(rec.Header().Get("X-BPS-"+key), 10, 64)
			if err != nil || at < previous || at <= 0 {
				t.Fatalf("missing or unordered stage %s: %q", key, rec.Header().Get("X-BPS-"+key))
			}
			previous = at
		}
		if rec.Header().Get("X-BPS-Ws-Handshake-Status") != "101" || rec.Header().Get("X-BPS-Handshake-Rate-Wait-Ms") == "" {
			t.Fatal("missing handshake status/wait observation")
		}
	}
}

func TestWebSocketDialQueueCancellationAndCloseNeverReachNetwork(t *testing.T) {
	for _, mode := range []string{"cancel", "close"} {
		t.Run(mode, func(t *testing.T) {
			var reached atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1); w.WriteHeader(500) }))
			defer up.Close()
			s := wsBridge(t, up, func(c *Config) { c.UpstreamRPS = 1 })
			defer s.CloseStartRate()
			if err := s.dialRates.run(context.Background(), "acct-test", startAuthorization{}, time.Time{}, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(basicBody())).WithContext(ctx)
				rec := httptest.NewRecorder()
				s.Handler().ServeHTTP(rec, req)
				result <- rec
			}()
			waitDialCount(t, s, 1)
			if mode == "cancel" {
				cancel()
			} else {
				s.CloseStartRate()
			}
			rec := <-result
			if reached.Load() != 0 || rec.Header().Get("X-BPS-Ws-Dial-Start-Unix-Ms") != "" || rec.Header().Get("X-BPS-Ws-Create-Write-Start-Unix-Ms") != "" {
				t.Fatal("a cancelled/closed dial waiter reached the network or invented a stage")
			}
			if mode == "close" && (rec.Code != 503 || !strings.Contains(rec.Body.String(), "bps_rate_limiter_closed")) {
				t.Fatalf("close result: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestWebSocketRejectionFencesQueuedDialsAndPreservesWindowOnResume(t *testing.T) {
	var handshakes atomic.Int32
	created := make(chan struct{})
	reject := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := handshakes.Add(1)
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"responses"}, OriginPatterns: []string{"bps.openai.com"}})
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		var body map[string]any
		if wsjson.Read(r.Context(), c, &body) != nil {
			return
		}
		if n == 1 {
			close(created)
			<-reject
			_ = wsjson.Write(r.Context(), c, map[string]any{"type": "error", "error": map[string]string{"code": "request_rejected"}})
			return
		}
		completeWS(t, r.Context(), c, nil)
	}))
	defer up.Close()
	s := wsBridge(t, up, func(c *Config) { c.UpstreamRPS = 1 }).WithStartAuthorization("auth", 0)
	defer s.CloseStartRate()
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- postResponses(t, s.Handler(), basicBody()) }()
	<-created
	pending := make(chan *httptest.ResponseRecorder, 1)
	go func() { pending <- postResponses(t, s.Handler(), basicBody()) }()
	waitDialCount(t, s, 1)
	close(reject)
	if !strings.Contains((<-first).Body.String(), "request_rejected") {
		t.Fatal("upstream rejection missing")
	}
	if rec := <-pending; rec.Code != 409 || handshakes.Load() != 1 || rec.Header().Get("X-BPS-Ws-Dial-Start-Unix-Ms") != "" {
		t.Fatalf("queued dial escaped rejection: %d, handshakes %d", rec.Code, handshakes.Load())
	}
	rec := postResponses(t, s.WithStartAuthorization("auth", 1).Handler(), basicBody())
	waitMS, _ := strconv.Atoi(rec.Header().Get("X-BPS-Handshake-Rate-Wait-Ms"))
	if rec.Code != 200 || handshakes.Load() != 2 || waitMS < 500 {
		t.Fatalf("resuming reset dial window: code %d, handshakes %d, wait %d", rec.Code, handshakes.Load(), waitMS)
	}
}

func TestWebSocketFailedHandshakeObservesOnlyOccurredStages(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer up.Close()
	s := wsBridge(t, up, nil)
	defer s.CloseStartRate()
	rec := postResponses(t, s.Handler(), basicBody())
	if rec.Code != 403 || rec.Header().Get("X-BPS-Ws-Handshake-Status") != "403" || rec.Header().Get("X-BPS-Ws-Dial-Start-Unix-Ms") == "" || rec.Header().Get("X-BPS-Ws-Dial-Done-Unix-Ms") == "" {
		t.Fatalf("failed handshake lost real observations: %v", rec.Header())
	}
	if rec.Header().Get("X-BPS-Ws-Create-Write-Start-Unix-Ms") != "" || rec.Header().Get("X-BPS-Ws-Create-Write-Done-Unix-Ms") != "" {
		t.Fatal("a failed handshake invented a create write")
	}
}
