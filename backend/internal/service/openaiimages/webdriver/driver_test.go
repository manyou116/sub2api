package webdriver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestDriver_ForwardEndToEnd 模拟整条 chatgpt.com web 链路：
// bootstrap → chat-requirements → init → prepare → conversation SSE → file download。
func TestDriver_ForwardEndToEnd(t *testing.T) {
	ResetBootstrapCacheForTest()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprint(w, `<html data-build="db1"><script src="https://chatgpt.com/sentinel/sdk.js"></script></html>`)
	})
	mux.HandleFunc("/chat-requirements", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"token":"sentinel-tok","turnstile":{"required":false},"arkose":{"required":false},"proofofwork":{"required":false}}`))
	})
	mux.HandleFunc("/conv-init", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/conv-prepare", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"conduit_token":"conduit-1"}`))
	})
	mux.HandleFunc("/conversation", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("x-request-id", "req-123")
		w.WriteHeader(200)
		// 一个最小化 SSE 流：
		fmt.Fprintln(w, `data: {"conversation_id":"conv-xyz","message":{"content":{"parts":["here is your image"]}}}`)
		fmt.Fprintln(w, `data: {"conversation_id":"conv-xyz","v":{"asset_pointer":"file-service://gen-001"}}`)
		fmt.Fprintln(w, `data: [DONE]`)
	})
	pngBytes := []byte("\x89PNG\r\n\x1a\nGENERATED")
	mux.HandleFunc("/cdn/img.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/files/gen-001/download", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(fmt.Sprintf(`{"download_url":"%s/cdn/img.png"}`, srv.URL)))
	})

	d := New(Endpoints{
		Start:            srv.URL + "/",
		ConversationInit: srv.URL + "/conv-init",
		Conversation:     srv.URL + "/conversation",
		ConversationPrep: srv.URL + "/conv-prepare",
		ChatRequirements: srv.URL + "/chat-requirements",
		Files:            srv.URL + "/files",
		BaseConversation: srv.URL + "/conv",
	})

	res, err := d.Forward(context.Background(), &Request{
		Account: AccountInfo{AccountID: 1, AccessToken: "tok"},
		Model:   "gpt-image-2",
		Prompt:  "a tiger",
		N:       1,
	})
	if err != nil {
		t.Fatalf("forward err: %v", err)
	}
	if res.ConversationID != "conv-xyz" {
		t.Errorf("conversationID = %q", res.ConversationID)
	}
	if len(res.Images) != 1 {
		t.Fatalf("expect 1 image, got %d", len(res.Images))
	}
	if string(res.Images[0].Bytes[:4]) != "\x89PNG" {
		t.Errorf("PNG header missing")
	}
	if res.RequestID != "req-123" {
		t.Errorf("requestID = %q", res.RequestID)
	}
}

func TestDriver_ForwardClassifies429(t *testing.T) {
	ResetBootstrapCacheForTest()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html data-build="db"></html>`))
	})
	mux.HandleFunc("/chat-requirements", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"detail":{"message":"too many","reset_after":120}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	d := New(Endpoints{Start: srv.URL + "/", ChatRequirements: srv.URL + "/chat-requirements"})
	_, err := d.Forward(context.Background(), &Request{
		Account: AccountInfo{AccessToken: "tok"},
		Model:   "gpt-image-2",
		Prompt:  "x",
	})
	rl, ok := err.(*RateLimitError)
	if !ok {
		t.Fatalf("expect RateLimitError, got %T %v", err, err)
	}
	if rl.ResetAfter.Seconds() != 120 {
		t.Errorf("ResetAfter = %v", rl.ResetAfter)
	}
}

func TestDriver_RejectsArkose(t *testing.T) {
	ResetBootstrapCacheForTest()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html data-build="db"></html>`))
	})
	mux.HandleFunc("/chat-requirements", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":    "x",
			"arkose":   map[string]any{"required": true},
			"turnstile": map[string]any{"required": false},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	d := New(Endpoints{Start: srv.URL + "/", ChatRequirements: srv.URL + "/chat-requirements"})
	_, err := d.Forward(context.Background(), &Request{
		Account: AccountInfo{AccessToken: "tok"}, Model: "gpt-image-2", Prompt: "x",
	})
	if _, ok := err.(*ProtocolError); !ok {
		t.Errorf("expect ProtocolError, got %T %v", err, err)
	}
}
