package proxy

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/bpsbridge/runtime/basispoints"
)

func TestInlineImageReachesUpstreamAsFetchableHTTPS(t *testing.T) {
	relay, err := basispoints.NewImageRelay("https://images.example", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = relay.Close() }()
	var img bytes.Buffer
	_ = png.Encode(&img, image.NewRGBA(image.Rect(0, 0, 4, 4)))
	rawURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(img.Bytes())
	body := strings.Replace(basicBody(), `{"type":"input_text","text":"hi"}`, `{"type":"input_image","image_url":"`+rawURL+`"}`, 1)
	called := false
	srv, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		raw, _ := io.ReadAll(r.Body)
		s := string(raw)
		if strings.Contains(s, "data:image") {
			t.Error("inline image leaked upstream")
		}
		start := strings.Index(s, "https://images.example"+basispoints.ImageRelayPath)
		if start < 0 {
			t.Error("missing image URL")
			w.WriteHeader(400)
			return
		}
		link := strings.SplitN(s[start:], `"`, 2)[0]
		download := httptest.NewRecorder()
		relay.ServeHTTP(download, httptest.NewRequest("GET", link, nil))
		if download.Code != 200 || !bytes.Equal(download.Body.Bytes(), img.Bytes()) {
			t.Error("rewritten image cannot be fetched")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
	})
	srv.SetImageRelay(relay)
	rec := postResponses(t, srv.Handler(), body)
	if rec.Code != 200 || !called {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}
