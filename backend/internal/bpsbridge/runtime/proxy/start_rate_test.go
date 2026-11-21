package proxy

import (
	"context"
	"errors"
	"io"
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

func testStartRates(t *testing.T, cfg Config) *Server {
	t.Helper()
	rates, err := newStartRates(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{startRates: rates}
	t.Cleanup(s.CloseStartRate)
	return s
}
func waitRateCount(t *testing.T, s *Server, account string, want int) {
	t.Helper()
	until := time.Now().Add(2 * time.Second)
	for time.Now().Before(until) {
		if s.RateSnapshot(account)["rate_waiting"] == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("waiting count = %v; want %d", s.RateSnapshot(account), want)
}
func rateCode(t *testing.T, err error, code string) {
	t.Helper()
	var detail *StartRateError
	if !errors.As(err, &detail) || detail.Code != code {
		t.Fatalf("error=%v; want %s", err, code)
	}
}

func TestStartRateSlidingWindowFIFOAndAccountIsolation(t *testing.T) {
	s := testStartRates(t, Config{UpstreamRPS: 1})
	var mu sync.Mutex
	var order []int
	var times []time.Time
	send := func(i int) func() error {
		return func() error {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, i)
			times = append(times, time.Now())
			return nil
		}
	}
	if err := s.startRates.run(context.Background(), "a", startAuthorization{id: "one"}, time.Time{}, send(0)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	for i := 1; i <= 2; i++ {
		i := i
		go func() {
			done <- s.startRates.run(context.Background(), "a", startAuthorization{id: strconv.Itoa(i)}, time.Time{}, send(i))
		}()
		waitRateCount(t, s, "a", i)
	}
	started := time.Now()
	if err := s.startRates.run(context.Background(), "b", startAuthorization{}, time.Time{}, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("other account was blocked")
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for i := range order {
		if order[i] != i {
			t.Fatalf("non-FIFO order %v", order)
		}
	}
	for i := 1; i < len(times); i++ {
		if times[i].Sub(times[i-1]) < time.Second-5*time.Millisecond {
			t.Fatalf("sliding window violated: %v", times)
		}
	}
	if snapshot := s.RateSnapshot("a"); snapshot["starts_last_second"] != 1 || fixtureValue[int64](snapshot["next_start_ms"]) <= 0 {
		t.Fatalf("bad snapshot: %v", snapshot)
	}
}

func TestStartRateCancellationQueueDeadlineAndClose(t *testing.T) {
	s := testStartRates(t, Config{UpstreamRPS: 1, RateQueueLimit: 1})
	if err := s.startRates.run(context.Background(), "a", startAuthorization{}, time.Time{}, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- s.startRates.run(ctx, "a", startAuthorization{}, time.Time{}, func() error { t.Error("cancelled request sent"); return nil })
	}()
	waitRateCount(t, s, "a", 1)
	rateCode(t, s.startRates.run(context.Background(), "a", startAuthorization{}, time.Time{}, func() error { return nil }), "bps_rate_queue_full")
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error %v", err)
	}
	waitRateCount(t, s, "a", 0)
	rateCode(t, s.startRates.run(context.Background(), "a", startAuthorization{}, time.Now().Add(15*time.Millisecond), func() error { t.Error("timed-out request sent"); return nil }), "bps_rate_wait_timeout")
	go func() {
		done <- s.startRates.run(context.Background(), "a", startAuthorization{}, time.Time{}, func() error { t.Error("closed limiter sent"); return nil })
	}()
	waitRateCount(t, s, "a", 1)
	s.CloseStartRate()
	rateCode(t, <-done, "bps_rate_limiter_closed")
}

func TestStartRateSlowDispatchDoesNotPrepayWindow(t *testing.T) {
	s := testStartRates(t, Config{UpstreamRPS: 1})
	first := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 3)
	go func() {
		done <- s.startRates.run(context.Background(), "a", startAuthorization{id: "same"}, time.Time{}, func() error { close(first); <-release; return nil })
	}()
	<-first
	started := make(chan time.Time, 2)
	for i := 1; i <= 2; i++ {
		go func() {
			done <- s.startRates.run(context.Background(), "a", startAuthorization{id: "same"}, time.Time{}, func() error { started <- time.Now(); return nil })
		}()
		waitRateCount(t, s, "a", i)
	}
	// The first window expires while the dispatch gate remains occupied.
	time.Sleep(1050 * time.Millisecond)
	close(release)
	a, b := <-started, <-started
	if b.Sub(a) < time.Second-5*time.Millisecond {
		t.Fatalf("permits prepaid behind dispatch lock: %s", b.Sub(a))
	}
	for range 3 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestStartRateDisabledPreservesConcurrencyAndConfiguration(t *testing.T) {
	s := testStartRates(t, Config{RateQueueLimit: 1})
	first := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.startRates.run(context.Background(), "a", startAuthorization{id: "same"}, time.Now().Add(-time.Second), func() error { close(first); <-release; return nil })
	}()
	<-first
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := s.startRates.run(ctx, "a", startAuthorization{id: "same"}, time.Now().Add(-time.Second), func() error { return nil }); err != nil {
		t.Fatalf("disabled rate changed concurrency: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigureAccountRate("a", 2, 5); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigureAccountRate("a", 0, 4); err != nil {
		t.Fatal(err)
	}
	if s.RateSnapshot("a")["upstream_rps"] != 2 {
		t.Fatal("stale setting replaced newer rate")
	}
	if err := s.ConfigureAccountRate("a", 0, 6); err != nil {
		t.Fatal(err)
	}
	if s.RateSnapshot("a")["upstream_rps"] != 0 {
		t.Fatal("explicit zero failed")
	}
	for _, cfg := range []Config{{UpstreamRPS: -1}, {UpstreamRPS: 10001}, {RateQueueLimit: -1}, {RateWaitTimeout: -1}} {
		if _, err := newStartRates(cfg); err == nil {
			t.Fatalf("accepted invalid config %+v", cfg)
		}
	}
}

func TestStartRateAuthorizationRevisionFencesWithoutResettingWindow(t *testing.T) {
	s := testStartRates(t, Config{UpstreamRPS: 1})
	old := startAuthorization{id: "auth", revision: 2}
	if err := s.startRates.run(context.Background(), "a", old, time.Time{}, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- s.startRates.run(context.Background(), "a", old, time.Time{}, func() error { t.Error("rejected queued request sent"); return nil })
	}()
	waitRateCount(t, s, "a", 1)
	s.startRates.reject("a", old)
	rateCode(t, <-done, "bps_start_authorization_blocked")
	rateCode(t, s.startRates.check("a", old), "bps_start_authorization_blocked")
	newer := startAuthorization{id: "auth", revision: 3}
	if err := s.startRates.check("a", newer); err != nil {
		t.Fatal(err)
	}
	s.startRates.reject("a", old)
	if err := s.startRates.check("a", newer); err != nil {
		t.Fatalf("old stream reblocked newer revision: %v", err)
	}
	rateCode(t, s.startRates.check("a", old), "bps_start_authorization_stale")
	rateCode(t, s.startRates.run(context.Background(), "a", newer, time.Now().Add(15*time.Millisecond), func() error { t.Error("new revision cleared start window"); return nil }), "bps_rate_wait_timeout")
	if err := s.startRates.check("a", startAuthorization{id: "other"}); err != nil {
		t.Fatal("blocked other authorization")
	}
}

func TestStartRateWebSocketRejectionStopsPendingCreates(t *testing.T) {
	var created atomic.Int32
	firstCreated := make(chan struct{})
	reject := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"responses"}, OriginPatterns: []string{"bps.openai.com"}})
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var request map[string]any
		if wsjson.Read(ctx, c, &request) != nil {
			return
		}
		if created.Add(1) == 1 {
			close(firstCreated)
			<-reject
			_ = wsjson.Write(ctx, c, map[string]any{"type": "error", "error": map[string]string{"code": "request_rejected"}})
			return
		}
		_ = wsjson.Write(ctx, c, map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{}}})
	}))
	defer up.Close()
	s := wsBridge(t, up, func(cfg *Config) { cfg.UpstreamRPS = 1 })
	// Isolate create fencing. Dial-queue fencing has its own regression test.
	s.dialRates.configure("acct-test", 0, 1)
	s = s.WithStartAuthorization("auth", 0)
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- postResponses(t, s.Handler(), basicBody()) }()
	<-firstCreated
	pending := make(chan *httptest.ResponseRecorder, 1)
	go func() { pending <- postResponses(t, s.Handler(), basicBody()) }()
	waitRateCount(t, s, "acct-test", 1)
	close(reject)
	if body := (<-firstDone).Body.String(); !strings.Contains(body, "request_rejected") {
		t.Fatalf("lost upstream rejection %s", body)
	}
	if response := <-pending; response.Code != 409 || !strings.Contains(response.Body.String(), "bps_start_authorization_blocked") {
		t.Fatalf("pending request not fenced: %d %s", response.Code, response.Body.String())
	}
	if created.Load() != 1 {
		t.Fatalf("pending create escaped: %d", created.Load())
	}
	resumed := postResponses(t, s.WithStartAuthorization("auth", 1).Handler(), basicBody())
	if resumed.Code != 200 || created.Load() != 2 {
		t.Fatalf("new revision did not resume: %d %s", resumed.Code, resumed.Body.String())
	}
	waitMS, _ := strconv.Atoi(resumed.Header().Get("X-BPS-Rate-Wait-Ms"))
	if waitMS < 500 {
		t.Fatalf("resume reset account window or missing timing: %d", waitMS)
	}
}

func TestStartRateProxyClonesShareStateAndLegacySSEWaits(t *testing.T) {
	var requests atomic.Int32
	s, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
	})
	if err := s.ConfigureAccountRate("acct-test", 1, 1); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	s = s.WithStartAuthorization("auth", 7).WithStartDeadline(deadline)
	clone, closeTransport, err := s.WithProxyURL("")
	if err != nil {
		t.Fatal(err)
	}
	defer closeTransport()
	if clone.startRates != s.startRates || clone.dialRates != s.dialRates || clone.startAuthorization != s.startAuthorization || !clone.startDeadline.Equal(deadline) {
		t.Fatal("proxy clone lost rate/auth/deadline")
	}
	lastStart := func() time.Time {
		t.Helper()
		rate := s.startRates.account("acct-test")
		rate.mu.Lock()
		defer rate.mu.Unlock()
		if len(rate.starts) != 1 {
			t.Fatalf("expected one start in the shared window, got %d", len(rate.starts))
		}
		return rate.starts[0]
	}
	if rec := postResponses(t, s.Handler(), basicBody()); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	// Compare the two recorded starts. The first response can consume part
	// of the rate window before the second request is made.
	firstStart := lastStart()
	rec := postResponses(t, clone.Handler(), basicBody())
	gap := lastStart().Sub(firstStart)
	if rec.Code != 200 || requests.Load() != 2 || gap < startRateWindow {
		t.Fatalf("SSE bypassed shared window: %d requests=%d start gap=%s", rec.Code, requests.Load(), gap)
	}
}

func TestStartRateLegacySSEDoesNotHoldGateUntilResponseHeaders(t *testing.T) {
	var requests atomic.Int32
	firstWritten := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	finish := func() { releaseOnce.Do(func() { close(release) }) }
	defer finish()
	s, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			close(firstWritten)
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
	})
	if err := s.ConfigureAccountRate("acct-test", 10, 1); err != nil {
		t.Fatal(err)
	}
	s = s.WithStartAuthorization("same-auth", 1)
	firstDone := make(chan struct{})
	go func() { postResponses(t, s.Handler(), basicBody()); close(firstDone) }()
	<-firstWritten
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(basicBody())).WithContext(ctx)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	finish()
	<-firstDone
	if requests.Load() != 2 || !strings.Contains(rec.Body.String(), "response.completed") {
		t.Fatalf("dispatch gate serialized HTTP response waits: requests=%d body=%s", requests.Load(), rec.Body.String())
	}
}
