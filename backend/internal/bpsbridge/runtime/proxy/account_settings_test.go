package proxy

import (
	"net/http"
	"testing"
)

func TestCustomHeadersCannotOverrideBPSIdentity(t *testing.T) {
	s := &Server{cfg: Config{Headers: map[string]string{"X-Custom": "value", "Authorization": "bad", "Cookie": "bad", "Sec-WebSocket-Protocol": "bad", "Chatgpt-Account-Id": "bad", "X-Openai-Account-Id": "bad"}}}
	r, _ := http.NewRequest("GET", "https://example.invalid", nil)
	applyUpstreamHeaders(r, "token", "account")
	s.applyCustomHeaders(r)
	if r.Header.Get("X-Custom") != "value" || r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("Chatgpt-Account-Id") != "account" || r.Header.Get("Cookie") != "" || r.Header.Get("Sec-WebSocket-Protocol") != "" {
		t.Fatal("unsafe header merge")
	}
}
