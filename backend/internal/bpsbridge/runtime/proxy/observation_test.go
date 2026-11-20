package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestProtocolObserverSeesRawEventsWithoutChangingClientStream(t *testing.T) {
	events := []string{
		`{"type":"basispoints.response.resume_token","resume_token":"secret-resume-token"}`,
		`{"type":"basispoints.response.metadata","headers":{"x-request-id":"upstream-request","authorization":"Bearer secret-metadata-token"}}`,
		`{"type":"basispoints.response.upstream_sent"}`,
		`{"type":"basispoints.response.heartbeat"}`,
		"{\n\"type\":\"response.created\",\"basispoints_replay_cursor\":\"secret-created-cursor\",\"response\":{\"id\":\"resp_test\",\"model\":\"upstream-model\",\"reasoning\":{\"effort\":\"high\"},\"output\":[]}}",
		`{"type":"response.completed","basispoints_replay_cursor":"secret-completed-cursor","response":{"id":"resp_test","status":"completed","model":"upstream-model","reasoning":{"effort":"high"},"output":[]}}`,
	}
	up := wsMock(t, func(ctx context.Context, c *websocket.Conn, _ map[string]any, _ *http.Request) {
		for _, event := range events {
			if err := c.Write(ctx, websocket.MessageText, []byte(event)); err != nil {
				t.Errorf("send event: %v", err)
				return
			}
		}
	})
	s := wsBridge(t, up, func(cfg *Config) { cfg.Verbose = true })
	var logs bytes.Buffer
	oldLog := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldLog) })
	var observed []string
	var handshakeStatus int
	var handshakeHeaders http.Header
	observer := protocolObserver{
		OnHandshake: func(status int, headers http.Header) {
			handshakeStatus, handshakeHeaders = status, headers
		},
		OnEvent: func(event []byte) {
			observed = append(observed, string(event))
			// Corrupting the callback copy must not affect the live stream.
			for i := range event {
				event[i] = '!'
			}
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(basicBody()))
	req = req.WithContext(withProtocolObserver(req.Context(), observer))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if handshakeStatus != http.StatusSwitchingProtocols || handshakeHeaders.Get("Upgrade") != "websocket" || handshakeHeaders.Get("Sec-Websocket-Protocol") != "responses" {
		t.Fatalf("missing handshake observation: status=%d headers=%v", handshakeStatus, handshakeHeaders)
	}
	if !reflect.DeepEqual(observed, events) {
		t.Fatalf("raw observations differ: got %q want %q", observed, events)
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "event: response.created") || !strings.Contains(rec.Body.String(), "event: response.completed") {
		t.Fatalf("unexpected SSE: status=%d %s", rec.Code, rec.Body.String())
	}
	// The same server's next ordinary request must have no observer attached.
	normal := postResponses(t, s.Handler(), basicBody())
	if normal.Body.String() != rec.Body.String() || len(observed) != len(events) {
		t.Fatal("observer changed ordinary SSE or escaped its request context")
	}
	for _, forbidden := range []string{"secret-", "basispoints.response.", "basispoints_replay_cursor", "upstream-request", "authorization"} {
		if strings.Contains(rec.Body.String(), forbidden) || strings.Contains(normal.Body.String(), forbidden) {
			t.Errorf("transport data leaked to client: %q", forbidden)
		}
	}
	// Existing verbose logs include event names, but never raw frames or fields.
	for _, forbidden := range []string{"secret-", "basispoints_replay_cursor", "upstream-request", "authorization", `"reasoning"`} {
		if strings.Contains(logs.String(), forbidden) {
			t.Errorf("raw transport data leaked to log: %q", forbidden)
		}
	}
}

func TestProtocolObserverSeesFailedHandshakeWithoutChangingRejection(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusFound, http.StatusOK} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "17")
				w.Header().Set("X-Request-ID", "upstream-request")
				w.Header().Set("Location", "/?token=secret-redirect-token")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "secret-error-body")
			}))
			defer up.Close()
			s := wsBridge(t, up, nil)
			handshakes, eventCount := 0, 0
			ctx := withProtocolObserver(context.Background(), protocolObserver{
				OnHandshake: func(got int, headers http.Header) {
					handshakes++
					if got != status || headers.Get("X-Request-ID") != "upstream-request" || headers.Get("Retry-After") != "17" {
						t.Errorf("wrong handshake observation: status=%d headers=%v", got, headers)
					}
					headers.Set("Retry-After", "999")
				},
				OnEvent: func([]byte) { eventCount++ },
			})
			req := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(basicBody())).WithContext(ctx)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			want := status
			if status < 400 {
				want = http.StatusBadGateway
			}
			if handshakes != 1 || eventCount != 0 || rec.Code != want || rec.Header().Get("Retry-After") != "17" {
				t.Fatalf("observation changed rejection: handshakes=%d events=%d status=%d retry=%q", handshakes, eventCount, rec.Code, rec.Header().Get("Retry-After"))
			}
			if strings.Contains(rec.Body.String(), "secret-") || rec.Header().Get("Location") != "" || rec.Header().Get("X-Request-ID") != "" {
				t.Fatal("upstream rejection details leaked to client")
			}
		})
	}
}

func TestProtocolObserverSeesMalformedTextBeforeValidation(t *testing.T) {
	const malformed = `{"type":"response.bad","secret-payload":`
	up := wsMock(t, func(ctx context.Context, c *websocket.Conn, _ map[string]any, _ *http.Request) {
		if err := c.Write(ctx, websocket.MessageText, []byte(malformed)); err != nil {
			t.Errorf("write: %v", err)
		}
	})
	s := wsBridge(t, up, nil)
	var observed string
	ctx := withProtocolObserver(context.Background(), protocolObserver{OnEvent: func(event []byte) { observed = string(event) }})
	req := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(basicBody())).WithContext(ctx)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if observed != malformed || !strings.Contains(rec.Body.String(), "basispoints_ws_protocol_error") || strings.Contains(rec.Body.String(), "secret-payload") {
		t.Fatalf("malformed text observation or sanitization failed: observed=%q body=%s", observed, rec.Body.String())
	}
}

func TestProtocolObserverCancellationClosesUpstream(t *testing.T) {
	closed := make(chan struct{})
	up := wsMock(t, func(ctx context.Context, c *websocket.Conn, _ map[string]any, _ *http.Request) {
		sendWS(t, ctx, c, map[string]any{"type": "basispoints.response.resume_token", "resume_token": "secret-cancel-token"})
		_, _, _ = c.Read(ctx)
		close(closed)
	})
	s := wsBridge(t, up, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seen := make(chan struct{}, 1)
	var mu sync.Mutex
	var events []string
	ctx = withProtocolObserver(ctx, protocolObserver{OnEvent: func(event []byte) {
		mu.Lock()
		events = append(events, string(event))
		mu.Unlock()
		seen <- struct{}{}
	}})
	req := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(basicBody())).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		s.Handler().ServeHTTP(rec, req)
		close(done)
	}()
	select {
	case <-seen:
	case <-time.After(2 * time.Second):
		t.Fatal("observer did not see control event")
	}
	cancel()
	for _, ch := range []<-chan struct{}{done, closed} {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatal("cancellation did not stop the handler and upstream read")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 {
		t.Fatalf("observer received synthetic or additional events: %q", events)
	}
	if strings.Contains(rec.Body.String(), "secret-") || strings.Contains(rec.Body.String(), "basispoints.response.") {
		t.Fatal("cancellation leaked transport data")
	}
}

func TestProtocolObserverDoesNotAttachToLegacySSE(t *testing.T) {
	s, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n")
	})
	ctx := withProtocolObserver(context.Background(), protocolObserver{
		OnHandshake: func(int, http.Header) { t.Error("legacy SSE invoked WS handshake observer") },
		OnEvent:     func([]byte) { t.Error("legacy SSE invoked WS event observer") },
	})
	req := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(basicBody())).WithContext(ctx)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "event: response.completed") {
		t.Fatalf("legacy SSE changed: status=%d body=%s", rec.Code, rec.Body.String())
	}
}
