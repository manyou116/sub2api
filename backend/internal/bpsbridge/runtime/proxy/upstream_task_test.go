package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func taskIdentityHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func TestInternalUpstreamTaskContextReachesWireWithHashedObservation(t *testing.T) {
	metadata := make(chan map[string]string, 2)
	srv, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Metadata map[string]string `json:"metadata"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		metadata <- body.Metadata
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"status\":\"completed\",\"output\":[]}}\n\n")
	})
	task := strings.Repeat("a", 64)
	for i, scope := range []string{"caller-A", "caller-B"} {
		turn := strings.Repeat([]string{"b", "c"}[i], 64)
		r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-6-astra","input":"hello"}`))
		r.Header.Set("session_id", scope)
		r = r.WithContext(WithUpstreamTask(r.Context(), task, turn))
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("request failed: %d %s", w.Code, w.Body.String())
		}
		got := <-metadata
		if got["task_id"] != task || got["turn_id"] != turn {
			t.Fatal("internal leased identities not sent upstream")
		}
		if w.Header().Get("X-BPS-Task-ID-Hash") != taskIdentityHash(got["task_id"]) || w.Header().Get("X-BPS-Turn-ID-Hash") != taskIdentityHash(got["turn_id"]) {
			t.Fatal("hash observation does not describe actual prepared identities")
		}
		for _, values := range w.Header() {
			for _, value := range values {
				if strings.Contains(value, task) || strings.Contains(value, turn) {
					t.Fatal("raw internal identity leaked in response headers")
				}
			}
		}
	}
}

func TestPublicTaskHeadersCannotInjectInternalIdentity(t *testing.T) {
	task := strings.Repeat("a", 64)
	srv, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Metadata map[string]string `json:"metadata"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Metadata["task_id"] == task || body.Metadata["turn_id"] == task {
			t.Error("public request injected server-owned task")
		}
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"status\":\"completed\",\"output\":[]}}\n\n")
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-6-astra","input":"hello"}`))
	for _, header := range []string{"X-BPS-Task-ID", "X-BPS-Turn-ID", "Upstream-Task-ID", "Upstream-Turn-ID", "task_id", "turn_id"} {
		r.Header.Set(header, task)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Header().Get("X-BPS-Task-ID-Hash") != "" || w.Header().Get("X-BPS-Turn-ID-Hash") != "" {
		t.Fatal("public headers activated internal task diagnostics")
	}
}

func TestInvalidInternalUpstreamTaskDoesNotSend(t *testing.T) {
	srv, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid task reached upstream") })
	for _, ids := range [][2]string{{"", ""}, {strings.Repeat("a", 64), ""}, {"invalid", strings.Repeat("b", 64)}} {
		r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-6-astra","input":"hello"}`))
		r = r.WithContext(WithUpstreamTask(context.Background(), ids[0], ids[1]))
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatal("invalid internal task did not fail locally")
		}
	}
}
