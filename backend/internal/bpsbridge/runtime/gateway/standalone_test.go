package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/bpsbridge/runtime/auth"
	"github.com/Wei-Shaw/sub2api/internal/bpsbridge/runtime/proxy"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func standaloneFixture(t *testing.T, store proxy.CredentialSource, concurrency int, serve func(context.Context, *websocket.Conn, *http.Request, map[string]any), onDial ...func()) *StandaloneHandler {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, observe := range onDial {
			observe()
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"responses"}, OriginPatterns: []string{"bps.openai.com"}})
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.CloseNow() }()
		var body map[string]any
		if err = wsjson.Read(ctx, conn, &body); err != nil {
			if ctx.Err() == nil {
				t.Error(err)
			}
			return
		}
		serve(ctx, conn, r, body)
	}))
	bridge, err := proxy.New(proxy.Config{Upstream: up.URL, UpstreamRPS: 0}, store)
	if err != nil {
		cancel()
		up.Close()
		t.Fatal(err)
	}
	h, err := NewStandaloneHandler(bridge, store, strings.Repeat("s", 32), Limits{PerAccount: concurrency, Queue: 8, Wait: 10 * time.Second})
	if err != nil {
		cancel()
		up.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close(); cancel(); h.Wait(); up.Close() })
	return h
}

func standaloneComplete(t *testing.T, ctx context.Context, conn *websocket.Conn, answer string) {
	t.Helper()
	err := wsjson.Write(ctx, conn, map[string]any{"type": "response.completed", "response": map[string]any{
		"id": "resp_standalone", "status": "completed", "output": []any{sessionMessage("assistant", answer)},
	}})
	if err != nil {
		if ctx.Err() == nil {
			t.Error(err)
		}
		return
	}
	_, _, err = conn.Read(ctx)
	if ctx.Err() == nil && websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Errorf("terminal request did not close normally: %v", err)
	}
}

func standaloneRequest(t *testing.T, payload map[string]any) *http.Request {
	t.Helper()
	payload["model"] = "gpt-6-astra"
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(raw)))
}

func standaloneCall(h *StandaloneHandler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func standaloneReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("standalone fixture timed out")
		var zero T
		return zero
	}
}

func TestStandaloneConcurrentSlotsReuseTaskAndRenewTurn(t *testing.T) {
	seen := make(chan map[string]any, 4)
	gates := []chan struct{}{make(chan struct{}), make(chan struct{})}
	store := fixed{token: userGrant("standalone-user", "token").AccessToken, account: "shared-workspace"}
	h := standaloneFixture(t, store, 2, func(ctx context.Context, conn *websocket.Conn, _ *http.Request, body map[string]any) {
		seen <- body
		prompt := fixtureValue[string](fixtureValue[map[string]any](fixtureValue[[]any](fixtureValue[map[string]any](sessionInput(body)[0])["content"])[0])["text"])
		gate := gates[0]
		if strings.HasPrefix(prompt, "round1-") {
			gate = gates[1]
		}
		select {
		case <-gate:
		case <-ctx.Done():
			return
		}
		standaloneComplete(t, ctx, conn, prompt)
	})
	tasks, turns, requests, resumes := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for round := range 2 {
		type result struct {
			response *httptest.ResponseRecorder
			prompt   string
		}
		results := make(chan result, 2)
		for client := range 2 {
			prompt := fmt.Sprintf("round%d-client%d", round, client)
			r := standaloneRequest(t, map[string]any{"input": []any{sessionMessage("user", prompt)}, "stream": true})
			// An identical public session must not serialize independent callers.
			r.Header.Set("session_id", "untrusted-shared-session")
			go func() { results <- result{standaloneCall(h, r), prompt} }()
		}
		batch := map[string]bool{}
		for range 2 {
			body := standaloneReceive(t, seen)
			meta := fixtureValue[map[string]any](body["metadata"])
			task, turn := fixtureValue[string](meta["task_id"]), fixtureValue[string](meta["turn_id"])
			request, resume := fixtureValue[string](body["basispoints_request_id"]), fixtureValue[string](body["basispoints_resume_token"])
			if batch[task] || turns[turn] || requests[request] || resumes[resume] || (round == 1 && !tasks[task]) {
				t.Fatal("overlapping slot, changed task pool, or reused per-request identity")
			}
			batch[task], tasks[task], turns[turn], requests[request], resumes[resume] = true, true, true, true, true
		}
		if h.admission.snapshot()["active"] != 2 {
			t.Fatal("two upstream requests did not hold independent leases")
		}
		close(gates[round])
		for range 2 {
			got := standaloneReceive(t, results)
			if got.response.Code != http.StatusOK || !strings.Contains(got.response.Body.String(), got.prompt) {
				t.Fatal("result was missing or returned to another client", got.response.Code)
			}
			if slot := got.response.Header().Get("X-BPS-Task-Slot"); slot != "1" && slot != "2" {
				t.Fatal("invalid slot observation", slot)
			}
		}
	}
	if len(tasks) != 2 || len(turns) != 4 || h.admission.snapshot()["active"] != 0 {
		t.Fatal("slot pool did not reuse and drain")
	}
}

func TestStandaloneFullInputIsolationPublicIdentityAndSSEContract(t *testing.T) {
	seen := make(chan map[string]any, 3)
	store := fixed{token: userGrant("standalone-user", "token").AccessToken, account: "shared-workspace"}
	h := standaloneFixture(t, store, 1, func(ctx context.Context, conn *websocket.Conn, _ *http.Request, body map[string]any) {
		seen <- body
		standaloneComplete(t, ctx, conn, "OK")
	})
	inputs := [][]any{
		{sessionMessage("user", "private-first")},
		{sessionMessage("user", "another-client"), sessionMessage("assistant", "its-own-history"), sessionMessage("user", "continue")},
		{sessionMessage("user", "trimmed-input-only")},
	}
	wantTask := slotTaskID(h.key, auth.AdmissionKeyFromToken(store.token), 1)
	turns := map[string]bool{}
	for i, input := range inputs {
		payload := map[string]any{"input": input, "store": true, "session_id": "payload-session",
			"metadata": map[string]any{"task_id": strings.Repeat("a", 64), "turn_id": strings.Repeat("b", 64)}}
		if i < 2 {
			payload["stream"] = i == 0
		}
		if i == 0 {
			payload["instructions"] = "first request instruction only"
		}
		r := standaloneRequest(t, payload)
		r.Header.Set("session_id", fmt.Sprintf("untrusted-client-%d", i))
		for _, name := range []string{"X-BPS-Task-ID", "X-BPS-Turn-ID", "Upstream-Task-ID", "Upstream-Turn-ID", "task_id", "turn_id"} {
			r.Header.Set(name, strings.Repeat("c", 64))
		}
		w := standaloneCall(h, r)
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") || !strings.Contains(w.Body.String(), "event: response.completed") {
			t.Fatalf("stream variant %d lost SSE contract: %d", i, w.Code)
		}
		body := standaloneReceive(t, seen)
		if !reflect.DeepEqual(sessionInput(body), input) {
			t.Fatal("another client's input was restored, appended, or changed")
		}
		if i > 0 && len(fixtureValue[[]any](body["input"])) != len(input)+1 {
			t.Fatal("prior request instructions were appended")
		}
		meta := fixtureValue[map[string]any](body["metadata"])
		turn := fixtureValue[string](meta["turn_id"])
		if sessionTask(body) != wantTask || turn == strings.Repeat("b", 64) || turns[turn] {
			t.Fatal("public identity changed slot task or reused turn")
		}
		turns[turn] = true
		if body["previous_response_id"] != nil || body["store"] != false {
			t.Fatal("local storage controls leaked upstream")
		}
	}
}

func TestStandaloneToolReplayRequiresCurrentRequestHistory(t *testing.T) {
	seen := make(chan map[string]any, 2)
	var calls atomic.Int32
	store := fixed{token: userGrant("tools-user", "token").AccessToken, account: "shared-workspace"}
	h := standaloneFixture(t, store, 1, func(ctx context.Context, conn *websocket.Conn, _ *http.Request, body map[string]any) {
		calls.Add(1)
		seen <- body
		standaloneComplete(t, ctx, conn, "OK")
	})
	submit := func(input []any) *httptest.ResponseRecorder {
		r := standaloneRequest(t, map[string]any{"input": input, "tools": []any{map[string]any{
			"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"},
		}}})
		r.Header.Set("session_id", "same-public-session")
		return standaloneCall(h, r)
	}
	full := func(value string) []any {
		return []any{
			map[string]any{"type": "function_call", "call_id": "shared-call", "name": "lookup", "arguments": fmt.Sprintf(`{"value":%q}`, value)},
			map[string]any{"type": "function_call_output", "call_id": "shared-call", "output": "tool-result"},
			sessionMessage("user", "continue"),
		}
	}
	if w := submit(full("private-A")); w.Code != http.StatusOK {
		t.Fatal("complete tool history was rejected", w.Code, w.Body.String())
	}
	standaloneReceive(t, seen)
	if w := submit([]any{map[string]any{"type": "function_call_output", "call_id": "shared-call", "output": "other-client-result"}}); w.Code != http.StatusBadRequest {
		t.Fatal("missing call was filled from another request's cache", w.Code)
	}
	if calls.Load() != 1 {
		t.Fatal("incomplete cross-client tool result reached upstream")
	}
	if w := submit(full("private-B")); w.Code != http.StatusOK {
		t.Fatal("second client's complete tool history was rejected", w.Code, w.Body.String())
	}
	body := standaloneReceive(t, seen)
	raw, _ := json.Marshal(sessionInput(body))
	if strings.Contains(string(raw), "private-A") || !strings.Contains(string(raw), "private-B") {
		t.Fatal("current tool call was replaced with another client's arguments")
	}
}

func TestStandaloneRejectionFencesNextDialWithPacingDisabled(t *testing.T) {
	var calls, dials atomic.Int32
	store := fixed{token: userGrant("rejected-user", "token").AccessToken, account: "shared-workspace"}
	h := standaloneFixture(t, store, 1, func(ctx context.Context, conn *websocket.Conn, _ *http.Request, _ map[string]any) {
		calls.Add(1)
		_ = wsjson.Write(ctx, conn, map[string]any{"type": "error", "error": map[string]any{"code": "request_rejected", "message": "fixture rejection"}})
		_, _, _ = conn.Read(ctx)
	}, func() { dials.Add(1) })
	w := standaloneCall(h, standaloneRequest(t, map[string]any{"input": "first"}))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "request_rejected") {
		t.Fatal("first upstream rejection was not passed through")
	}
	for range 2 {
		w = standaloneCall(h, standaloneRequest(t, map[string]any{"input": "must-not-dial"}))
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "bps_start_authorization_blocked") {
			t.Fatal("rejected authorization was not fenced", w.Code, w.Body.String())
		}
	}
	if calls.Load() != 1 || dials.Load() != 1 || h.admission.snapshot()["active"] != 0 {
		t.Fatal("rejection allowed another upstream request or leaked a slot")
	}
}

type standaloneMutableCredentials struct {
	mu      sync.Mutex
	token   string
	account string
}

func (s *standaloneMutableCredentials) Credentials() (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token, s.account, nil
}

func (s *standaloneMutableCredentials) Expiry() time.Time { return time.Now().Add(time.Hour) }

func TestStandaloneQueuedRequestKeepsCredentialSnapshot(t *testing.T) {
	tokenA, tokenB := userGrant("snapshot-A", "old-token").AccessToken, userGrant("snapshot-B", "new-token").AccessToken
	store := &standaloneMutableCredentials{token: tokenA, account: "shared-workspace"}
	type observation struct{ bearer, task string }
	seen := make(chan observation, 3)
	gate := make(chan struct{})
	h := standaloneFixture(t, store, 1, func(ctx context.Context, conn *websocket.Conn, r *http.Request, body map[string]any) {
		bearer := ""
		for _, protocol := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
			protocol = strings.TrimSpace(protocol)
			if strings.HasPrefix(protocol, "openai-bearer.") {
				bearer = strings.TrimPrefix(protocol, "openai-bearer.")
			}
		}
		seen <- observation{bearer, sessionTask(body)}
		select {
		case <-gate:
		case <-ctx.Done():
			return
		}
		standaloneComplete(t, ctx, conn, "OK")
	})
	results := make(chan *httptest.ResponseRecorder, 2)
	first := standaloneRequest(t, map[string]any{"input": "first"})
	go func() { results <- standaloneCall(h, first) }()
	gotFirst := standaloneReceive(t, seen)
	second := standaloneRequest(t, map[string]any{"input": "queued-before-credential-change"})
	go func() { results <- standaloneCall(h, second) }()
	deadline := time.Now().Add(5 * time.Second)
	for h.admission.snapshot()["queued"] != 1 {
		if time.Now().After(deadline) {
			t.Fatal("second request did not enter queue")
		}
		time.Sleep(time.Millisecond)
	}
	store.mu.Lock()
	store.token = tokenB
	store.mu.Unlock()
	close(gate)
	gotSecond := standaloneReceive(t, seen)
	for range 2 {
		if w := standaloneReceive(t, results); w.Code != http.StatusOK {
			t.Fatal("snapshotted request failed", w.Code)
		}
	}
	if gotFirst.bearer != tokenA || gotSecond.bearer != tokenA || gotFirst.task != gotSecond.task {
		t.Fatal("queued request switched grant/account behind its admitted slot")
	}
	if w := standaloneCall(h, standaloneRequest(t, map[string]any{"input": "after-credential-change"})); w.Code != http.StatusOK {
		t.Fatal("new grant request failed", w.Code)
	}
	gotThird := standaloneReceive(t, seen)
	if gotThird.bearer != tokenB || gotThird.task == gotFirst.task {
		t.Fatal("new user reused the old user's credentials or task")
	}
}
