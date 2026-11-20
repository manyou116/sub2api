package proxy

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type testCredentials struct{ token string }

func (c testCredentials) Credentials() (string, string, error) { return c.token, "acct-test", nil }
func (c testCredentials) Expiry() time.Time                    { return time.Unix(2_000_000_000, 0) }

func fakeStore(t *testing.T) testCredentials {
	t.Helper()
	claims := map[string]any{
		"exp": time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-test",
			"chatgpt_plan_type":  "pro",
		},
	}
	payload, _ := json.Marshal(claims)
	return testCredentials{token: "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"}
}

func newTestServer(t *testing.T, upstream http.HandlerFunc) (*Server, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	srv, err := New(Config{Upstream: up.URL, UpstreamTransport: "sse", Verbose: false}, fakeStore(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv, up
}

func postResponses(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func basicBody() string {
	return `{"model":"gpt-6-astra","stream":true,"store":false,` +
		`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],` +
		`"reasoning":{"effort":"low"}}`
}

// The bridge must send the ChatGPT auth headers Basispoints requires.
func TestSendsRequiredUpstreamHeaders(t *testing.T) {
	var got http.Header
	srv, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
	})

	rec := postResponses(t, srv.Handler(), basicBody())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	checks := map[string]string{
		"X-Basispoints-Auth-Mode":                            "chatgpt",
		"Chatgpt-Account-Id":                                 "acct-test",
		"X-Openai-Account-Id":                                "acct-test",
		"X-Openai-Internal-Basispoints-Client-Product":       "basispoints-excel-plugin",
		"X-Openai-Internal-Basispoints-Client-Agent-Profile": "excel",
	}
	for k, want := range checks {
		if got.Get(k) != want {
			t.Errorf("header %s = %q, want %q", k, got.Get(k), want)
		}
	}
	if !strings.HasPrefix(got.Get("Authorization"), "Bearer ") {
		t.Errorf("Authorization header missing bearer prefix")
	}
}

// web_search must be stripped, not forwarded, or upstream rejects the turn.
func TestStripsWebSearchBeforeForwarding(t *testing.T) {
	var upstreamBody []byte
	srv, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
	})

	inbound := `{"model":"gpt-6-astra","stream":true,` +
		`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],` +
		`"tools":[{"type":"function","name":"shell_command","description":"run","parameters":{"type":"object"}},` +
		`{"type":"web_search","external_web_access":true}]}`

	if rec := postResponses(t, srv.Handler(), inbound); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(string(upstreamBody), "web_search") {
		t.Fatalf("web_search leaked upstream: %s", upstreamBody)
	}
	if !strings.Contains(string(upstreamBody), "shell_command") {
		t.Fatalf("client tool was dropped: %s", upstreamBody)
	}
}

// Upstream errors must surface as a structured client error, not a hang.
func TestUpstreamErrorIsReported(t *testing.T) {
	srv, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad token"}}`)
	})
	rec := postResponses(t, srv.Handler(), basicBody())
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	var doc struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
	}
	if doc.Error.Code != "basispoints_upstream_error" {
		t.Fatalf("code = %q", doc.Error.Code)
	}
	if !strings.Contains(doc.Error.Message, "401") {
		t.Fatalf("message should mention the status: %q", doc.Error.Message)
	}
}

// Malformed JSON from the client is a 400, never forwarded.
func TestInvalidClientJSON(t *testing.T) {
	called := false
	srv, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	rec := postResponses(t, srv.Handler(), `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if called {
		t.Fatal("invalid request must not reach upstream")
	}
}

func TestRejectsNonPost(t *testing.T) {
	srv, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {})
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// Health must not leak the token, only an expiry.
func TestHealthzDoesNotLeakToken(t *testing.T) {
	srv, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {})
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "eyJ") || strings.Contains(body, "Bearer") {
		t.Fatalf("health endpoint leaked credentials: %s", body)
	}
	if !strings.Contains(body, "token_expires_at") {
		t.Fatalf("expected expiry in body, got %s", body)
	}
}

func TestIsLoopback(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8790", true},
		{"127.0.0.1", true},
		{"localhost:8790", true},
		{"localhost", true},
		{"[::1]:8790", true},
		{"::1", true},
		{"", true},
		{"0.0.0.0:8790", false},
		{"0.0.0.0", false},
		{"192.168.1.5:8790", false},
		{"[2001:db8::1]:8790", false},
		{"example.com:8790", false},
	}
	for _, c := range cases {
		if got := IsLoopback(c.addr); got != c.want {
			t.Errorf("IsLoopback(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}
