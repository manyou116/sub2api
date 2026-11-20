package bpsbridge

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/bpsbridge/runtime/gateway"
	"github.com/Wei-Shaw/sub2api/internal/bpsbridge/runtime/proxy"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/stretchr/testify/require"
)

func TestBPSRuntimeWebSocketStreaming(t *testing.T) {
	dir := t.TempDir()
	grant := filepath.Join(dir, "grant.json")
	state := filepath.Join(dir, "task")
	bound := BoundGrant{
		Grant: Grant{AccessToken: "fixture-token"}, Identity: Identity{Account: "fixture-workspace"},
		ExpiresAt: time.Now().Add(time.Hour),
	}
	require.NoError(t, Save(grant, bound))

	releaseFinal := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols: []string{"responses"}, OriginPatterns: []string{"bps.openai.com"},
		})
		if !assertNoError(t, err) {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var req map[string]any
		if !assertNoError(t, wsjson.Read(ctx, conn, &req)) {
			return
		}
		if req["type"] != "response.create" || req["model"] != "gpt-5.4" ||
			!strings.Contains(r.Header.Get("Sec-WebSocket-Protocol"), "openai-bearer.fixture-token") {
			t.Error("unexpected upstream request")
		}
		if !assertNoError(t, wsjson.Write(ctx, conn, map[string]any{
			"type": "response.output_text.delta", "delta": "OK", "item_id": "msg_test",
		})) {
			return
		}
		select {
		case <-releaseFinal:
		case <-ctx.Done():
			t.Error("the first chunk was not delivered before the terminal event")
			return
		}
		_ = assertNoError(t, wsjson.Write(ctx, conn, map[string]any{
			"type":     "response.completed",
			"response": map[string]any{"id": "resp_test", "status": "completed", "model": "gpt-5.4", "output": []any{}},
		}))
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}))
	defer upstream.Close()

	worker, err := (Runtime{}).start(t.Context(), grant, state, 1, proxy.Config{Upstream: upstream.URL})
	require.NoError(t, err)
	defer worker.Stop()
	_, err = gateway.OpenStandaloneState(state)
	require.Error(t, err, "a second worker must not take over the same task slots")
	// Replace the file after the caller selected its grant. The handshake must
	// use the exact snapshot that was checked for this request.
	replacement := bound
	replacement.AccessToken = "replacement-token"
	replacement.Identity.Account = "replacement-workspace"
	require.NoError(t, Save(grant, replacement))
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
	defer cancel()
	resp, err := worker.Responses(ctx, bound, []byte(`{"model":"gpt-5.4","input":"hi","stream":true,"store":false}`))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	require.Equal(t, "1", resp.Header.Get("X-BPS-Task-Slot"))
	rd := bufio.NewReader(resp.Body)
	for {
		line, err := rd.ReadString('\n')
		require.NoError(t, err)
		if strings.Contains(line, `"delta":"OK"`) {
			break
		}
	}
	close(releaseFinal)
	final, err := io.ReadAll(rd)
	require.NoError(t, err)
	require.Contains(t, string(final), "response.completed")
	worker.Stop()
	opened, err := gateway.OpenStandaloneState(state)
	require.NoError(t, err)
	opened.Close()
}

func TestBPSRuntimeClosingBodyCancelsHandler(t *testing.T) {
	handlerDone := make(chan struct{})
	worker := newWorker(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("body waiting for consumer"))
		<-r.Context().Done()
	}), func() {})
	defer worker.Stop()
	resp, err := worker.Responses(t.Context(), BoundGrant{
		Grant: Grant{AccessToken: "fixture-token"}, Identity: Identity{Account: "fixture-workspace"},
		ExpiresAt: time.Now().Add(time.Hour),
	}, nil)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("cancelled response still holds its handler")
	}
}

func assertNoError(t *testing.T, err error) bool {
	t.Helper()
	if err != nil {
		t.Error(err)
	}
	return err == nil
}
