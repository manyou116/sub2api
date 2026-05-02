package webdriver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// TestNewHTTPClient_NavigationHeadersStrippedFromXHR 验证 ImpersonateChrome 注入的
// 「浏览器导航专用」公共头不会泄漏到 backend-api 这类 XHR/fetch 请求中——这是触发
// Cloudflare JA4H 指纹判定为 bot 并返回 403 的根本原因。
func TestNewHTTPClient_NavigationHeadersStrippedFromXHR(t *testing.T) {
	var (
		mu      sync.Mutex
		gotHdrs http.Header
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotHdrs = r.Header.Clone()
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	client, err := newHTTPClient("", fingerprints[0])
	if err != nil {
		t.Fatalf("newHTTPClient: %v", err)
	}
	headers := buildHeaders(AccountInfo{AccessToken: "t"}, fingerprints[0])
	resp, err := client.R().
		SetContext(context.Background()).
		SetHeaders(headerToMap(headers)).
		Post(srv.URL)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_ = resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if v := gotHdrs.Get("Upgrade-Insecure-Requests"); v != "" {
		t.Errorf("Upgrade-Insecure-Requests should not be sent on XHR, got %q", v)
	}
	if v := gotHdrs.Get("Sec-Fetch-User"); v != "" {
		t.Errorf("Sec-Fetch-User should not be sent on XHR, got %q", v)
	}
	// 预期 buildHeaders 设置的 XHR 风格 Sec-Fetch-* 仍然存在。
	if got := gotHdrs.Get("Sec-Fetch-Mode"); got != "cors" {
		t.Errorf("Sec-Fetch-Mode = %q, want cors", got)
	}
	if got := gotHdrs.Get("Sec-Fetch-Dest"); got != "empty" {
		t.Errorf("Sec-Fetch-Dest = %q, want empty", got)
	}
	if got := gotHdrs.Get("Sec-Fetch-Site"); got != "same-origin" {
		t.Errorf("Sec-Fetch-Site = %q, want same-origin", got)
	}
}

// TestNewHTTPClient_BootstrapHeadersStillNavigation 反向断言：buildBootstrapHeaders
// 显式设置的导航头在 GET 首页时仍然能正常发出（删除公共头不会破坏 bootstrap 流程）。
func TestNewHTTPClient_BootstrapHeadersStillNavigation(t *testing.T) {
	var (
		mu      sync.Mutex
		gotHdrs http.Header
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotHdrs = r.Header.Clone()
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html></html>`))
	}))
	defer srv.Close()

	client, err := newHTTPClient("", fingerprints[0])
	if err != nil {
		t.Fatalf("newHTTPClient: %v", err)
	}
	bootstrapHdrs := buildBootstrapHeaders(AccountInfo{}, fingerprints[0])
	resp, err := client.R().
		SetContext(context.Background()).
		SetHeaders(headerToMap(bootstrapHdrs)).
		Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if got := gotHdrs.Get("Upgrade-Insecure-Requests"); got != "1" {
		t.Errorf("bootstrap Upgrade-Insecure-Requests = %q, want 1", got)
	}
	if got := gotHdrs.Get("Sec-Fetch-User"); got != "?1" {
		t.Errorf("bootstrap Sec-Fetch-User = %q, want ?1", got)
	}
	if got := gotHdrs.Get("Sec-Fetch-Mode"); got != "navigate" {
		t.Errorf("bootstrap Sec-Fetch-Mode = %q, want navigate", got)
	}
}
