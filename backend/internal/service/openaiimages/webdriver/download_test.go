package webdriver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchDownloadURL_FileService(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/files/file-xyz/download") {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"download_url":"https://cdn.example/img.png"}`))
	}))
	defer srv.Close()
	client, _ := newHTTPClient("", fingerprints[0])
	headers := buildHeaders(AccountInfo{AccessToken: "t"}, fingerprints[0])
	url, err := fetchDownloadURL(context.Background(), client, headers, srv.URL+"/files", "https://chatgpt.com/backend-api/conversation", "conv-1", "file-service://file-xyz")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if url != "https://cdn.example/img.png" {
		t.Errorf("got %q", url)
	}
}

func TestFetchDownloadURL_UnsupportedPointer(t *testing.T) {
	client, _ := newHTTPClient("", fingerprints[0])
	headers := buildHeaders(AccountInfo{AccessToken: "t"}, fingerprints[0])
	_, err := fetchDownloadURL(context.Background(), client, headers, "x", "y", "c", "https://random/url")
	if err == nil || !strings.Contains(err.Error(), "unsupported pointer") {
		t.Errorf("expect unsupported, got %v", err)
	}
}

func TestDownloadBytes_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nXYZ"))
	}))
	defer srv.Close()
	client, _ := newHTTPClient("", fingerprints[0])
	headers := buildHeaders(AccountInfo{AccessToken: "t"}, fingerprints[0])
	data, ct, err := downloadBytes(context.Background(), client, headers, srv.URL)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ct != "image/png" || len(data) == 0 {
		t.Errorf("got ct=%q len=%d", ct, len(data))
	}
}

func TestDownloadBytes_5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()
	client, _ := newHTTPClient("", fingerprints[0])
	headers := buildHeaders(AccountInfo{AccessToken: "t"}, fingerprints[0])
	_, _, err := downloadBytes(context.Background(), client, headers, srv.URL)
	if err == nil {
		t.Fatal("expect error")
	}
}

func TestClassifyHTTPError_RateLimit(t *testing.T) {
	// 直接构造一个返回 429 的 server，再让 fetch 命中。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"detail":{"message":"hold on","reset_after":30}}`))
	}))
	defer srv.Close()
	client, _ := newHTTPClient("", fingerprints[0])
	headers := buildHeaders(AccountInfo{AccessToken: "t"}, fingerprints[0])
	_, err := fetchDownloadURL(context.Background(), client, headers, srv.URL+"/files", "y", "c", "file-service://x")
	if err == nil {
		t.Fatal("expect err")
	}
	rl, ok := err.(*RateLimitError)
	if !ok {
		t.Fatalf("expect RateLimitError, got %T %v", err, err)
	}
	if rl.ResetAfter.Seconds() != 30 {
		t.Errorf("ResetAfter = %v", rl.ResetAfter)
	}
}
