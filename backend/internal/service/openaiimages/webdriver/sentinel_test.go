package webdriver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBootstrap_ParsesScriptsAndDataBuild(t *testing.T) {
	ResetBootstrapCacheForTest()
	html := `<!DOCTYPE html>
<html data-build="prod-2025-01-15">
<head>
<script src="https://chatgpt.com/c/abc123/_buildManifest.js"></script>
<script src="https://chatgpt.com/sentinel/sdk.js"></script>
<script src="https://other-cdn.com/ignored.js"></script>
</head>
</html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(html))
	}))
	defer srv.Close()

	client, _ := newHTTPClient("", fingerprints[0])
	headers := buildHeaders(AccountInfo{AccessToken: "t", UserAgent: "UA"}, fingerprints[0])
	scripts, dataBuild := bootstrap(context.Background(), client, headers, srv.URL+"/")

	if dataBuild != "prod-2025-01-15" {
		t.Errorf("dataBuild = %q, want prod-2025-01-15", dataBuild)
	}
	wantScripts := 2
	got := 0
	for _, s := range scripts {
		if strings.Contains(s, "chatgpt.com") {
			got++
		}
	}
	if got != wantScripts {
		t.Errorf("got %d chatgpt.com scripts, want %d (all=%v)", got, wantScripts, scripts)
	}
}

func TestBootstrap_FallbackOnFailure(t *testing.T) {
	ResetBootstrapCacheForTest()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	client, _ := newHTTPClient("", fingerprints[0])
	headers := buildHeaders(AccountInfo{AccessToken: "t"}, fingerprints[0])
	scripts, _ := bootstrap(context.Background(), client, headers, srv.URL+"/")
	if len(scripts) != 1 || scripts[0] != defaultSentinelSDKURL {
		t.Errorf("expect fallback, got %v", scripts)
	}
}

func TestFetchChatRequirements_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
		  "token":"sentinel-tok-xyz",
		  "turnstile":{"required":false},
		  "arkose":{"required":false},
		  "proofofwork":{"required":false,"seed":"","difficulty":""}
		}`))
	}))
	defer srv.Close()
	client, _ := newHTTPClient("", fingerprints[0])
	headers := buildHeaders(AccountInfo{AccessToken: "t"}, fingerprints[0])
	r, err := fetchChatRequirements(context.Background(), client, headers, srv.URL, []string{defaultSentinelSDKURL}, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if r.Token != "sentinel-tok-xyz" {
		t.Errorf("token = %q", r.Token)
	}
}

func TestFetchChatRequirements_RetriesOn4xx(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"detail":"bad p"}`))
			return
		}
		_, _ = w.Write([]byte(`{"token":"second-try"}`))
	}))
	defer srv.Close()
	client, _ := newHTTPClient("", fingerprints[0])
	headers := buildHeaders(AccountInfo{AccessToken: "t"}, fingerprints[0])
	r, err := fetchChatRequirements(context.Background(), client, headers, srv.URL, nil, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if r.Token != "second-try" {
		t.Errorf("expected retry to succeed, got %q", r.Token)
	}
	if calls != 2 {
		t.Errorf("expect 2 calls, got %d", calls)
	}
}

func TestFetchChatRequirements_RateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"detail":{"message":"slow down","reset_after":60}}`))
	}))
	defer srv.Close()
	client, _ := newHTTPClient("", fingerprints[0])
	headers := buildHeaders(AccountInfo{AccessToken: "t"}, fingerprints[0])
	_, err := fetchChatRequirements(context.Background(), client, headers, srv.URL, nil, "")
	if err == nil {
		t.Fatal("expect error")
	}
	if rl, ok := err.(*RateLimitError); !ok || rl.ResetAfter.Seconds() != 60 {
		t.Errorf("expect RateLimitError reset=60s, got %T %v", err, err)
	}
}

func TestInitConversation_Auth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"detail":"token expired"}`))
	}))
	defer srv.Close()
	client, _ := newHTTPClient("", fingerprints[0])
	headers := buildHeaders(AccountInfo{AccessToken: "t"}, fingerprints[0])
	err := initConversation(context.Background(), client, headers, srv.URL)
	if _, ok := err.(*AuthError); !ok {
		t.Errorf("expect AuthError, got %T %v", err, err)
	}
}
