package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/bpsbridge/runtime/proxy"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Admission and pacing remain independent: a 2/s start window can fill three
// active slots. Check every observed rolling window for both dial and create.
func TestStandaloneRollingPacingFillsConcurrentSlots(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dials, creates := make(chan time.Time, 6), make(chan time.Time, 6)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dials <- time.Now()
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"responses"}, OriginPatterns: []string{"bps.openai.com"}})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		var body map[string]any
		if wsjson.Read(ctx, conn, &body) != nil {
			return
		}
		creates <- time.Now()
		select {
		case <-release:
		case <-ctx.Done():
			return
		}
		_ = wsjson.Write(ctx, conn, map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_pacing", "status": "completed", "output": []any{}}})
		_, _, _ = conn.Read(ctx)
	}))
	t.Cleanup(up.Close)
	store := fixed{token: userGrant("pacing-user", "token").AccessToken, account: "shared-workspace"}
	bridge, err := proxy.New(proxy.Config{Upstream: up.URL, UpstreamRPS: 2, RateWaitTimeout: 8 * time.Second}, store)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewStandaloneHandler(bridge, store, strings.Repeat("p", 32), Limits{PerAccount: 3, Queue: 3, Wait: 8 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { handler.Close(); handler.Wait() })
	results := make(chan *httptest.ResponseRecorder, 6)
	for range 6 {
		go func() {
			r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-6-astra","input":"hello","stream":true}`)).WithContext(ctx)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			results <- w
		}()
	}
	var createTimes []time.Time
	for range 3 {
		select {
		case at := <-creates:
			createTimes = append(createTimes, at)
		case <-ctx.Done():
			t.Fatal("pacing did not fill three simultaneous generation slots")
		}
	}
	if active := handler.admission.snapshot()["active"]; active != 3 {
		t.Fatal("start rate incorrectly reduced concurrency to two", active)
	}
	unblock()
	for range 6 {
		select {
		case w := <-results:
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "response.completed") {
				t.Fatal("paced request failed", w.Code, w.Body.String())
			}
		case <-ctx.Done():
			t.Fatal("paced requests did not drain")
		}
	}
	for range 3 {
		createTimes = append(createTimes, <-creates)
	}
	var dialTimes []time.Time
	for range 6 {
		dialTimes = append(dialTimes, <-dials)
	}
	auditRollingReceives(t, "standalone dial", dialTimes, 2)
	auditRollingReceives(t, "standalone create", createTimes, 2)
	if snapshot := handler.admission.snapshot(); snapshot["active"] != 0 || snapshot["queued"] != 0 {
		t.Fatal("paced handler leaked admission", snapshot)
	}
}
