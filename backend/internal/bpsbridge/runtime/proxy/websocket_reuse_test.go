package proxy

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/stretchr/testify/require"
)

func TestBPSWebSocketSessionReuse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*reuseTestRequest)
		mode   string
		hit    bool
	}{
		{name: "same-session-two-turns", hit: true},
		{name: "different-user", change: func(r *reuseTestRequest) { r.scope = "another-user" }},
		{name: "missing-identity", change: func(r *reuseTestRequest) { r.scope = "" }},
		{name: "different-task", change: func(r *reuseTestRequest) { r.slot = "another-task" }},
		{name: "different-grant", change: func(r *reuseTestRequest) { r.token += "-updated" }},
		{name: "different-proxy-identity", change: func(r *reuseTestRequest) { r.proxy = "changed" }},
		{name: "expired"},
		{name: "remote-closed"},
		{name: "idle-unexpected-frame"},
		{name: "failed-turn"},
		{name: "misattributed-second-turn", mode: "wrong-turn", hit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dials atomic.Int32
			var submissions atomic.Int32
			betweenTurns := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
					Subprotocols: []string{"responses"}, OriginPatterns: []string{"bps.openai.com"},
				})
				if err != nil {
					t.Errorf("accept: %v", err)
					return
				}
				defer func() { _ = conn.CloseNow() }()
				connection := dials.Add(1)
				ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
				defer cancel()
				for {
					var payload map[string]any
					if wsjson.Read(ctx, conn, &payload) != nil {
						return
					}
					turn := submissions.Add(1)
					id := fmt.Sprintf("resp_%d", turn)
					if turn == 2 && tc.mode == "wrong-turn" {
						id = "resp_1"
					}
					if turn == 1 && payload["basispoints_request_id"] != r.URL.Query().Get("bps_ws_affinity") {
						t.Error("cold dial must retain the original affinity")
					}
					sendWS(t, ctx, conn, map[string]any{"type": "response.created", "response": map[string]any{"id": id}})
					terminal, status := "response.completed", "completed"
					if turn == 1 && tc.name == "failed-turn" {
						terminal, status = "response.failed", "failed"
					}
					sendWS(t, ctx, conn, map[string]any{"type": terminal, "response": map[string]any{
						"id": id, "status": status, "model": "gpt-6-astra", "output": []any{},
					}})
					if connection == 1 && turn == 1 && (tc.name == "remote-closed" || tc.name == "idle-unexpected-frame") {
						select {
						case <-betweenTurns:
						case <-ctx.Done():
							return
						}
						if tc.name == "idle-unexpected-frame" {
							sendWS(t, ctx, conn, map[string]any{"type": "response.output_text.delta", "delta": "stale"})
							_, _, _ = conn.Read(ctx)
						}
						return
					}
				}
			}))
			defer upstream.Close()
			server := wsBridge(t, upstream, nil)
			defer func() {
				_ = takeReusable(reuseSlot{server.startRates, reuseTestSlotID("task")}, reuseIdentity{})
				_ = takeReusable(reuseSlot{server.startRates, reuseTestSlotID("another-task")}, reuseIdentity{})
			}()
			in := reuseTestRequest{scope: "first-user/session", slot: "task", token: fakeStore(t).token}
			got, reused := issueReuseRequest(t, server, in)
			require.False(t, reused)
			if tc.name == "failed-turn" {
				require.Contains(t, got, "response.failed")
			} else {
				require.Contains(t, got, "response.completed")
			}
			if tc.name == "expired" {
				slot := reuseSlot{server.startRates, reuseTestSlotID("task")}
				idleReuse.Lock()
				entry := idleReuse.slots[slot]
				if entry != nil {
					entry.since = time.Now().Add(-reuseIdleTTL)
				}
				idleReuse.Unlock()
				require.NotNil(t, entry)
			}
			if tc.name == "remote-closed" || tc.name == "idle-unexpected-frame" {
				close(betweenTurns)
				require.Eventually(t, func() bool {
					idleReuse.Lock()
					defer idleReuse.Unlock()
					return idleReuse.slots[reuseSlot{server.startRates, reuseTestSlotID("task")}] == nil
				}, time.Second, time.Millisecond)
			}
			if tc.change != nil {
				tc.change(&in)
			}
			got, reused = issueReuseRequest(t, server, in)
			require.Equal(t, tc.hit, reused)
			if tc.mode == "wrong-turn" {
				require.Contains(t, got, "basispoints_ws_protocol_error")
				require.NotContains(t, got, "response.completed")
			} else {
				require.Contains(t, got, "resp_2")
				require.Contains(t, got, "response.completed")
			}
			require.EqualValues(t, 2, submissions.Load())
			if tc.hit {
				require.EqualValues(t, 1, dials.Load())
			} else {
				require.EqualValues(t, 2, dials.Load())
			}
		})
	}
}

type reuseTestRequest struct {
	scope, slot, token, proxy string
}

func issueReuseRequest(t *testing.T, server *Server, input reuseTestRequest) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	taskID := reuseTestSlotID(input.slot)
	turnID := fmt.Sprintf("%x", sha256.Sum256([]byte(newRequestID())))
	ctx = WithUpstreamTask(ctx, taskID, turnID)
	ctx, reused := WithWebSocketReuse(ctx, input.scope)
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/responses", strings.NewReader(basicBody()))
	out := httptest.NewRecorder()
	clone := *server
	clone.cfg.ProxyURL = input.proxy
	clone.WithCredentials(testCredentials{token: input.token}).ServeHTTP(out, req)
	require.Equal(t, http.StatusOK, out.Code, out.Body.String())
	if reused != nil && reused.Load() {
		require.Empty(t, out.Header().Get("X-BPS-Ws-Dial-Start-Unix-Ms"))
	} else {
		require.NotEmpty(t, out.Header().Get("X-BPS-Ws-Dial-Start-Unix-Ms"))
	}
	return out.Body.String(), reused != nil && reused.Load()
}

func reuseTestSlotID(name string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(name))) }

func TestBPSWebSocketIdlePoolCaps(t *testing.T) {
	newIdle := func(slot reuseSlot) context.Context {
		ctx, stop := context.WithCancel(t.Context())
		t.Cleanup(func() {
			_ = takeReusable(slot, reuseIdentity{})
			stop()
		})
		parkReusable(slot, reuseIdentity{scope: "test"}, &reusableSocket{ctx: ctx, stop: stop})
		return ctx
	}
	worker := &startRates{}
	first := newIdle(reuseSlot{worker, "first"})
	second := newIdle(reuseSlot{worker, "second"})
	require.ErrorIs(t, first.Err(), context.Canceled)
	require.NoError(t, second.Err())
	for i := 0; i < maxIdleReuseSockets; i++ {
		newIdle(reuseSlot{worker: &startRates{}, task: fmt.Sprint(i)})
	}
	require.ErrorIs(t, second.Err(), context.Canceled)
	idleReuse.Lock()
	count := len(idleReuse.slots)
	idleReuse.Unlock()
	require.Equal(t, maxIdleReuseSockets, count)
}
