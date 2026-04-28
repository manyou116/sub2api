package webdriver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestUploadFiles_ThreeStepProtocol(t *testing.T) {
	var (
		azureCalled   atomic.Int32
		uploadedDone  atomic.Int32
		azureURL      string
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/files", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "method", 405)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["use_case"] != "multimodal" {
			t.Errorf("use_case = %v", body["use_case"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"file_id":    "file-abc",
			"upload_url": azureURL,
		})
	})
	mux.HandleFunc("/files/file-abc/uploaded", func(w http.ResponseWriter, r *http.Request) {
		uploadedDone.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success"})
	})

	azure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" {
			http.Error(w, "method", 405)
			return
		}
		if r.Header.Get("x-ms-blob-type") != "BlockBlob" {
			t.Errorf("missing x-ms-blob-type")
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "PNG-DATA" {
			t.Errorf("body = %q", body)
		}
		azureCalled.Add(1)
		w.WriteHeader(201)
	}))
	defer azure.Close()
	azureURL = azure.URL + "/blob"

	srv := httptest.NewServer(mux)
	defer srv.Close()

	client, _ := newHTTPClient("")
	headers := buildHeaders(AccountInfo{AccessToken: "t"})
	out, err := uploadFiles(context.Background(), client, headers, srv.URL+"/files", []Upload{
		{Filename: "x.png", ContentType: "image/png", Data: []byte("PNG-DATA"), Width: 16, Height: 16},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out) != 1 || out[0].FileID != "file-abc" {
		t.Errorf("got %+v", out)
	}
	if azureCalled.Load() != 1 {
		t.Errorf("azure called %d times", azureCalled.Load())
	}
	if uploadedDone.Load() != 1 {
		t.Errorf("uploaded confirm called %d times", uploadedDone.Load())
	}
}

func TestUploadFiles_SkipsAzureWhenURLEmpty(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/files", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"file_id": "f1", "upload_url": ""})
	})
	confirmed := atomic.Int32{}
	mux.HandleFunc("/files/f1/uploaded", func(w http.ResponseWriter, r *http.Request) {
		confirmed.Add(1)
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client, _ := newHTTPClient("")
	headers := buildHeaders(AccountInfo{AccessToken: "t"})
	out, err := uploadFiles(context.Background(), client, headers, srv.URL+"/files", []Upload{
		{ContentType: "image/png", Data: []byte("X")},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out) != 1 || out[0].FileID != "f1" || confirmed.Load() != 1 {
		t.Errorf("got out=%+v confirmed=%d", out, confirmed.Load())
	}
}

func TestUploadFiles_FailsOnEmptyFileID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"file_id":"","upload_url":""}`))
	}))
	defer srv.Close()
	client, _ := newHTTPClient("")
	headers := buildHeaders(AccountInfo{AccessToken: "t"})
	_, err := uploadFiles(context.Background(), client, headers, srv.URL, []Upload{
		{ContentType: "image/png", Data: []byte("X")},
	})
	if err == nil || !strings.Contains(err.Error(), "create upload slot") {
		t.Errorf("expect err mention create upload slot, got %v", err)
	}
}
