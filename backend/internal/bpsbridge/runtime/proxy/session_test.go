package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLogicalSessionContextOverridesSyntheticScopeHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	r.Header.Set("session_id", "client-chat")
	if logicalSessionFor(r) != "client-chat" {
		t.Fatal("standalone client session was ignored")
	}
	for _, session := range []string{"", "gateway-chat"} {
		rr := r.WithContext(WithLogicalSession(context.Background(), session))
		if logicalSessionFor(rr) != session {
			t.Fatal("gateway session presence was confused with its scope header")
		}
	}
	if logicalSessionFor(r) != "client-chat" {
		t.Fatal("request-local identity leaked to the original request")
	}
}

func TestStandaloneLogicalSessionReachesPrepare(t *testing.T) {
	tasks := make(chan string, 2)
	srv, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Metadata map[string]string `json:"metadata"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		tasks <- body.Metadata["task_id"]
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"status\":\"completed\",\"output\":[]}}\n\n")
	})
	for _, input := range []string{"first", "client-trimmed"} {
		r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-6-astra","input":"`+input+`"}`))
		r.Header.Set("session_id", "standalone-chat")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("request failed: %d", w.Code)
		}
	}
	first, second := <-tasks, <-tasks
	if first == "" || first != second {
		t.Fatal("standalone explicit chat did not keep a task")
	}
}
