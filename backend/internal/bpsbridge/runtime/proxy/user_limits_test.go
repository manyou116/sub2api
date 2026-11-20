package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/bpsbridge/runtime/auth"
)

func proxyUserToken(subject string) string {
	payload, _ := json.Marshal(map[string]any{"sub": subject,
		"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": "shared-workspace"}})
	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestSSEUserScopedLimitsUseTokenIdentityAndKeepRealAccountHeaders(t *testing.T) {
	var requests atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Chatgpt-Account-Id") != "shared-workspace" || r.Header.Get("X-Openai-Account-Id") != "shared-workspace" || r.Header.Get("Authorization") != "Bearer "+proxyUserToken("second") {
			t.Error("local user admission key replaced real upstream credentials")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer up.Close()
	s, err := New(Config{Upstream: up.URL, UpstreamTransport: "sse"}, fakeStore(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.CloseStartRate()
	for _, subject := range []string{"first", "second"} {
		if err := s.ConfigureAccountRate(auth.AdmissionKey("shared-workspace", subject), 1, 1); err != nil {
			t.Fatal(err)
		}
	}
	first := proxyUserToken("first")
	key := auth.AdmissionKeyFromToken(first)
	if got := admissionKeyForToken(first, "untrusted-explicit-account"); got != key {
		t.Fatal("explicit account overrode JWT admission identity")
	}
	if got := admissionKeyForToken("legacy-token", "legacy-account"); got != "legacy-account" {
		t.Fatal("standalone legacy credentials lost their account fallback")
	}
	if err := s.startRates.run(context.Background(), key, startAuthorization{}, time.Time{}, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if body, err := s.openUpstream(ctx, []byte(`{}`), first, "shared-workspace"); err == nil {
		_ = body.Close()
		t.Fatal("SSE bypassed the user's already consumed window")
	}
	body, err := s.openUpstream(context.Background(), []byte(`{}`), proxyUserToken("second"), "shared-workspace")
	if err != nil {
		t.Fatal("another workspace member inherited the first user's wait", err)
	}
	_, _ = io.Copy(io.Discard, body)
	_ = body.Close()
	if requests.Load() != 1 {
		t.Fatal("unexpected upstream requests")
	}
}
