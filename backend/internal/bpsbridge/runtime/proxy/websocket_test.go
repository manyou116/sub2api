package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/bpsbridge/runtime/basispoints"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type wsSession func(context.Context, *websocket.Conn, map[string]any, *http.Request)

func wsMock(t *testing.T, session wsSession) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols: []string{"responses"}, OriginPatterns: []string{"bps.openai.com"},
		})
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer func() { _ = conn.CloseNow() }()
		conn.SetReadLimit(2 << 20)
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var request map[string]any
		if err := wsjson.Read(ctx, conn, &request); err != nil {
			t.Errorf("read response.create: %v", err)
			return
		}
		session(ctx, conn, request, r)
	}))
	t.Cleanup(up.Close)
	return up
}

func wsBridge(t *testing.T, up *httptest.Server, change func(*Config)) *Server {
	t.Helper()
	cfg := Config{Upstream: up.URL, WSIdleTimeout: time.Second}
	if change != nil {
		change(&cfg)
	}
	s, err := New(cfg, fakeStore(t))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func sendWS(t *testing.T, ctx context.Context, conn *websocket.Conn, event any) {
	t.Helper()
	if err := wsjson.Write(ctx, conn, event); err != nil {
		t.Errorf("send: %v", err)
	}
}

func completeWS(t *testing.T, ctx context.Context, c *websocket.Conn, output []any) {
	t.Helper()
	sendWS(t, ctx, c, map[string]any{
		"type": "response.completed", "basispoints_replay_cursor": "private-cursor",
		"response": map[string]any{"id": "resp_test", "status": "completed", "model": "reported-model", "output": output},
	})
}

func TestWebSocketHandshakeAndStreaming(t *testing.T) {
	var expectedToken string
	up := wsMock(t, func(ctx context.Context, c *websocket.Conn, doc map[string]any, r *http.Request) {
		if r.Method != "GET" || r.ProtoMajor != 1 || c.Subprotocol() != "responses" {
			t.Errorf("not a Responses HTTP/1.1 upgrade")
		}
		var token string
		for _, protocol := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
			protocol = strings.TrimSpace(protocol)
			if strings.HasPrefix(protocol, "openai-bearer.") {
				token = strings.TrimPrefix(protocol, "openai-bearer.")
			}
		}
		if token == "" || token != expectedToken {
			t.Error("missing or incorrect handshake credentials")
		}
		if strings.Contains(r.URL.RawQuery, token) || r.Header.Get("Cookie") != "" {
			t.Error("credentials in query or copied browser cookie")
		}
		for name := range r.Header {
			k := strings.ToLower(name)
			if k == "authorization" || k == "chatgpt-account-id" || k == "content-type" || strings.HasPrefix(k, "x-openai-") || strings.HasPrefix(k, "x-basispoints-") {
				t.Errorf("unexpected REST header on WebSocket: %s", name)
			}
		}
		if r.Header.Get("Origin") != "https://bps.openai.com" || r.Header.Get("X-Custom") != "retained" {
			t.Error("missing origin or allowed custom header")
		}
		q := r.URL.Query()
		var profile map[string]string
		if json.Unmarshal([]byte(q.Get("bps_client_info")), &profile) != nil || profile["X-Openai-Internal-Basispoints-Client-Agent-Profile"] != "excel" {
			t.Error("missing Excel client profile")
		}
		if q.Get("bps_auth_mode") != "chatgpt" || q.Get("bps_ws_affinity") == "" || q.Get("bps_control_frames") != "upstream_sent,heartbeat" {
			t.Error("missing BPS query parameters")
		}
		if doc["type"] != "response.create" || doc["model"] != "gpt-6-astra" || doc["stream"] != true || doc["store"] != false {
			t.Error("invalid create envelope")
		}
		if doc["basispoints_request_id"] == "" || !strings.HasPrefix(fmt.Sprint(doc["basispoints_resume_token"]), "resume_") {
			t.Error("missing per-request identifiers")
		}
		if _, ok := doc["tools"]; ok {
			t.Error("native tools array must not be sent")
		}
		if fixtureValue[map[string]any](doc["metadata"])["bps_tools_version_id"] != "test-tools-version" {
			t.Error("missing optional tool version")
		}
		// A successful ping requires the client's read loop to send a pong.
		go func() { _, _, _ = c.Read(ctx) }()
		if err := c.Ping(ctx); err != nil {
			t.Errorf("ping/pong: %v", err)
		}
		sendWS(t, ctx, c, map[string]any{"type": "basispoints.response.resume_token", "resume_token": "do-not-forward"})
		sendWS(t, ctx, c, map[string]any{"type": "basispoints.response.metadata", "headers": map[string]string{"private": "do-not-forward"}})
		sendWS(t, ctx, c, map[string]any{"type": "basispoints.response.heartbeat"})
		// Pretty JSON must be compacted before embedding in SSE.
		_ = c.Write(ctx, websocket.MessageText, []byte("{\n\"type\":\"response.output_text.delta\",\n\"delta\":\"你好\\nworld\"\n}"))
		completeWS(t, ctx, c, []any{})
	})
	s := wsBridge(t, up, func(c *Config) {
		c.ToolsVersion = "test-tools-version"
		c.Headers = map[string]string{"X-Custom": "retained", "Authorization": "bad", "Chatgpt-Account-Id": "bad", "X-Openai-Account-Id": "bad", "X-Basispoints-Auth-Mode": "bad", "X-Openai-Internal-Basispoints-Client-Product": "bad", "Cookie": "bad", "Origin": "https://example.invalid", "Sec-WebSocket-Protocol": "bad"}
	})
	expectedToken, _, _ = s.store.Credentials()
	rec := postResponses(t, s.Handler(), basicBody())
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `你好\nworld`) || !strings.Contains(body, "response.completed") || !strings.Contains(body, "reported-model") {
		t.Fatalf("unexpected SSE: status=%d %s", rec.Code, body)
	}
	for _, forbidden := range []string{"do-not-forward", "private-cursor", "basispoints_replay_cursor", "basispoints.response."} {
		if strings.Contains(body, forbidden) {
			t.Errorf("leaked transport metadata %q", forbidden)
		}
	}
}

func TestWebSocketTextArrivesBeforeCompletion(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	up := wsMock(t, func(ctx context.Context, c *websocket.Conn, _ map[string]any, _ *http.Request) {
		sendWS(t, ctx, c, map[string]any{"type": "response.output_text.delta", "delta": "first"})
		select {
		case <-release:
		case <-ctx.Done():
		}
	})
	s := wsBridge(t, up, nil)
	local := httptest.NewServer(s.Handler())
	defer local.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", local.URL+"/responses", strings.NewReader(basicBody()))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "event: response.output_text.delta\n" {
		t.Fatalf("text was not streamed before completion: %q %v", line, err)
	}
}

func TestWebSocketFailuresAreTerminalAndNotRetried(t *testing.T) {
	cases := []struct {
		name, code string
		send       func(context.Context, *websocket.Conn)
	}{
		{"disconnect", "basispoints_ws_disconnected", func(_ context.Context, c *websocket.Conn) { _ = c.CloseNow() }},
		{"timeout", "basispoints_ws_timeout", func(ctx context.Context, c *websocket.Conn) { _, _, _ = c.Read(ctx) }},
		{"invalid-json", "basispoints_ws_protocol_error", func(ctx context.Context, c *websocket.Conn) {
			_ = c.Write(ctx, websocket.MessageText, []byte("not JSON"))
		}},
		{"injected-type", "basispoints_ws_protocol_error", func(ctx context.Context, c *websocket.Conn) {
			sendWS(t, ctx, c, map[string]any{"type": "response.foo\ndata: injected"})
		}},
		{"binary", "basispoints_ws_protocol_error", func(ctx context.Context, c *websocket.Conn) {
			_ = c.Write(ctx, websocket.MessageBinary, []byte(`{"type":"response.completed"}`))
		}},
		{"draining", "basispoints_ws_draining", func(ctx context.Context, c *websocket.Conn) {
			sendWS(t, ctx, c, map[string]any{"type": "basispoints.response.server_draining"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			up := wsMock(t, func(ctx context.Context, c *websocket.Conn, _ map[string]any, _ *http.Request) {
				calls.Add(1)
				tc.send(ctx, c)
			})
			s := wsBridge(t, up, func(c *Config) { c.WSIdleTimeout = 100 * time.Millisecond })
			rec := postResponses(t, s.Handler(), basicBody())
			if !strings.Contains(rec.Body.String(), tc.code) || !strings.Contains(rec.Body.String(), "event: response.failed") {
				t.Fatalf("missing terminal failure: %s", rec.Body.String())
			}
			if calls.Load() != 1 {
				t.Fatal("request was retried")
			}
		})
	}
}

func TestWebSocketHandshakeErrorRedactsSecretsAndDoesNotRedirect(t *testing.T) {
	for _, status := range []int{401, 403, 429, 302, 200} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", "/redirected?secret=do-not-forward")
				w.Header().Set("Retry-After", "17")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, r.Header.Get("Sec-WebSocket-Protocol")+" do-not-forward")
			}))
			defer up.Close()
			s := wsBridge(t, up, nil)
			rec := postResponses(t, s.Handler(), basicBody())
			want := status
			if want < 400 {
				want = 502
			}
			if rec.Header().Get("Retry-After") != "17" {
				t.Fatal("retry hint lost")
			}
			if rec.Code != want || !strings.Contains(rec.Body.String(), fmt.Sprint(status)) || calls.Load() != 1 {
				t.Fatalf("bad error mapping/redirect: %d %s calls=%d", rec.Code, rec.Body.String(), calls.Load())
			}
			if strings.Contains(rec.Body.String(), "eyJ") || strings.Contains(rec.Body.String(), "do-not-forward") {
				t.Fatal("handshake error leaked credentials")
			}
		})
	}
}

func TestWebSocketClosingBodyCancelsUpstream(t *testing.T) {
	closed := make(chan struct{})
	up := wsMock(t, func(ctx context.Context, c *websocket.Conn, _ map[string]any, _ *http.Request) {
		_, _, _ = c.Read(ctx)
		close(closed)
	})
	s := wsBridge(t, up, nil)
	prepared, _, err := basispoints.Prepare([]byte(basicBody()), "test", &basispoints.ReplayCache{})
	if err != nil {
		t.Fatal(err)
	}
	body, err := s.openUpstream(context.Background(), prepared, "test-token", "test-account")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, body); close(done) }()
	_ = body.Close()
	for _, ch := range []chan struct{}{closed, done} {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatal("close did not interrupt upstream read")
		}
	}
}

func TestWebSocketToolRoundTrip(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(fmt.Sprintf("custom=%t", custom), func(t *testing.T) {
			tool := map[string]any{"type": "function", "name": "test_tool", "parameters": map[string]any{"type": "object"}}
			outer := map[string]any{"summary": "Call test tool", "extended_summary": "", "destructive": false, "references": []any{}, "code": `{"name":"test_tool","arguments":{"value":42}}`}
			if custom {
				tool = map[string]any{"type": "custom", "name": "test_tool"}
				outer["summary"] = "codex2api.custom/test_tool"
				outer["code"] = "text(\"raw\\ninput\");"
			}
			args, _ := json.Marshal(outer)
			native := map[string]any{"type": "function_call", "name": "run_officejs", "id": "fc_native", "call_id": "call_native", "status": "completed", "arguments": string(args)}
			var turns atomic.Int32
			up := wsMock(t, func(ctx context.Context, c *websocket.Conn, doc map[string]any, _ *http.Request) {
				if turns.Add(1) == 1 {
					sendWS(t, ctx, c, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": native})
					completeWS(t, ctx, c, []any{native})
					return
				}
				foundCall, foundOutput := false, false
				for _, raw := range fixtureValue[[]any](doc["input"]) {
					x := fixtureValue[map[string]any](raw)
					if x["type"] == "function_call" {
						foundCall = x["name"] == "run_officejs" && x["arguments"] == string(args) && x["call_id"] == "call_native"
					}
					if x["type"] == "function_call_output" {
						foundOutput = x["call_id"] == "call_native" && x["output"] == "result"
					}
				}
				if !foundCall || !foundOutput {
					t.Error("tool identity or result was lost in history replay")
				}
				completeWS(t, ctx, c, []any{})
			})
			s := wsBridge(t, up, nil)
			input := []any{map[string]any{"type": "message", "role": "user", "content": "test"}}
			doc := map[string]any{"model": "test-model", "input": input, "tools": []any{tool}}
			raw, _ := json.Marshal(doc)
			first := postResponses(t, s.Handler(), string(raw))
			var translated map[string]any
			for _, line := range strings.Split(first.Body.String(), "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var event map[string]any
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event)
				if event["type"] == "response.output_item.done" {
					translated, _ = event["item"].(map[string]any)
				}
			}
			if translated == nil || translated["name"] != "test_tool" {
				t.Fatalf("tool was not converted: %s", first.Body.String())
			}
			outputType := "function_call_output"
			if custom {
				outputType = "custom_tool_call_output"
			}
			doc["input"] = append(input, translated, map[string]any{"type": outputType, "call_id": "call_native", "output": "result"})
			raw, _ = json.Marshal(doc)
			second := postResponses(t, s.Handler(), string(raw))
			if !strings.Contains(second.Body.String(), "response.completed") || turns.Load() != 2 {
				t.Fatalf("continuation failed: %s", second.Body.String())
			}
		})
	}
}

func TestWebSocketUsesConfiguredHTTPProxy(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"responses"}, OriginPatterns: []string{"bps.openai.com"}})
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = c.CloseNow() }()
		var doc map[string]any
		if err := wsjson.Read(r.Context(), c, &doc); err != nil {
			t.Error(err)
			return
		}
		completeWS(t, r.Context(), c, []any{})
	}))
	defer up.Close()
	var connects atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" {
			t.Error("expected CONNECT")
			w.WriteHeader(400)
			return
		}
		connects.Add(1)
		target, err := net.Dial("tcp", strings.TrimPrefix(up.URL, "https://"))
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = target.Close() }()
		client, rw, err := fixtureValue[http.Hijacker](w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = client.Close() }()
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = rw.Flush()
		go func() { _, _ = io.Copy(target, rw); _ = target.Close() }()
		_, _ = io.Copy(client, target)
	}))
	defer proxy.Close()
	s := wsBridge(t, up, func(c *Config) { c.ProxyURL = proxy.URL })
	roots := x509.NewCertPool()
	roots.AddCert(up.Certificate())
	fixtureValue[*http.Transport](s.client.Transport).TLSClientConfig = &tls.Config{RootCAs: roots}
	rec := postResponses(t, s.Handler(), basicBody())
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "response.completed") || connects.Load() != 1 {
		t.Fatalf("WSS did not use CONNECT proxy: status=%d connects=%d body=%s", rec.Code, connects.Load(), rec.Body.String())
	}
}

func TestInvalidUpstreamConfig(t *testing.T) {
	for _, cfg := range []Config{
		{UpstreamTransport: "sse"},
		{Upstream: "https://BPS.OPENAI.COM:443/basispoints/api/responses", UpstreamTransport: "sse"},
		{Upstream: "wss://bps.openai.com./basispoints/api/responses", UpstreamTransport: "sse"},
		{UpstreamTransport: "unknown"}, {Upstream: "file:///tmp/a"},
		{Upstream: "ws://example.com/responses"}, {WSIdleTimeout: -time.Second},
	} {
		if _, err := New(cfg, fakeStore(t)); err == nil {
			t.Errorf("accepted invalid config: %+v", cfg)
		}
	}
}
