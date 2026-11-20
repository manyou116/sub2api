package basispoints

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func imageBody(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(object{"input": []any{object{"role": "user", "content": []any{object{"type": "input_image", "image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(b.Bytes())}}}}})
	return body
}
func imageLink(t *testing.T, b []byte) string {
	t.Helper()
	var v object
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return fixtureValue[string](fixtureValue[map[string]any](fixtureValue[[]any](fixtureValue[map[string]any](fixtureValue[[]any](v["input"])[0])["content"])[0])["image_url"])
}
func TestImageRelayReuseExpiryAndIsolation(t *testing.T) {
	r, err := NewImageRelay("https://images.example", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	body := imageBody(t)
	out, err := r.Rewrite(body, "account-a/session")
	if err != nil {
		t.Fatal(err)
	}
	link := imageLink(t, out)
	token := strings.TrimPrefix(link, "https://images.example"+ImageRelayPath)
	r.mu.Lock()
	img := r.entries[token]
	path := img.path
	img.expires = time.Now().Add(time.Minute)
	before := img.expires
	r.mu.Unlock()
	again, err := r.Rewrite(body, "account-a/session")
	if err != nil {
		t.Fatal(err)
	}
	if imageLink(t, again) != link {
		t.Fatal("repeated image changed URL")
	}
	r.mu.Lock()
	if r.entries[token].path != path || !img.expires.After(before) {
		t.Fatal("reuse must extend expiry without duplicating file")
	}
	r.mu.Unlock()
	files, _ := os.ReadDir(r.dir)
	if len(files) != 1 {
		t.Fatal("duplicate file leaked")
	}
	other, err := r.Rewrite(body, "account-b/session")
	if err != nil {
		t.Fatal(err)
	}
	if imageLink(t, other) == link {
		t.Fatal("cross-account token reused")
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", ImageRelayPath+token, nil))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatal("image unavailable")
	}
	r.mu.Lock()
	img.expires = time.Now().Add(-time.Second)
	r.mu.Unlock()
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", ImageRelayPath+token, nil))
	if rec.Code != 404 {
		t.Fatal("expired image still downloadable")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("expired file not removed")
	}
}
func TestImageRelayActiveReadAndCloseCleanup(t *testing.T) {
	r, err := NewImageRelay("https://images.example", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	out, err := r.Rewrite(imageBody(t), "scope")
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimPrefix(imageLink(t, out), "https://images.example"+ImageRelayPath)
	r.mu.Lock()
	img := r.entries[token]
	img.readers++
	img.expires = time.Now().Add(-time.Second)
	r.pruneLocked(time.Now())
	r.mu.Unlock()
	if _, err := os.Stat(img.path); err != nil {
		t.Fatal("deleted while being read")
	}
	r.finishRead(img)
	if _, err := os.Stat(img.path); !os.IsNotExist(err) {
		t.Fatal("retired image not deleted after download")
	}
	dir := r.dir
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("session directory survived close")
	}
}
func TestImageRelayInvalidBatchAndOrphanCleanup(t *testing.T) {
	root := t.TempDir()
	orphan := filepath.Join(root, "session-abandoned")
	if err := os.Mkdir(orphan, 0700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(orphan, old, old)
	r, err := NewImageRelay("https://images.example", root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("stale session survived")
	}
	body := imageBody(t)
	var v object
	_ = json.Unmarshal(body, &v)
	item := fixtureValue[map[string]any](fixtureValue[[]any](v["input"])[0])
	item["content"] = append(fixtureValue[[]any](item["content"]), object{"type": "input_image", "image_url": "data:image/png;base64,invalid!"})
	bad, _ := json.Marshal(v)
	if _, err := r.Rewrite(bad, "scope"); err == nil {
		t.Fatal("invalid image accepted")
	}
	files, _ := os.ReadDir(r.dir)
	if len(files) != 0 || r.reservedBytes != 0 || r.reservedEntries != 0 {
		t.Fatal("failed batch leaked files or reservations")
	}
}

func TestImageRelayTenMinuteRetention(t *testing.T) {
	if imageRelayTTL != 10*time.Minute {
		t.Fatalf("retention = %v, want 10 minutes", imageRelayTTL)
	}
	r, err := NewImageRelay("https://images.example", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	before := time.Now()
	out, err := r.Rewrite(imageBody(t), "scope")
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	token := strings.TrimPrefix(imageLink(t, out), "https://images.example"+ImageRelayPath)
	r.mu.Lock()
	img := r.entries[token]
	deadlineOK := !img.expires.Before(before.Add(10*time.Minute)) && !img.expires.After(after.Add(10*time.Minute))
	r.pruneLocked(img.expires.Add(-time.Nanosecond))
	presentBefore := r.entries[token] != nil
	r.pruneLocked(img.expires)
	absentAtExpiry := r.entries[token] == nil
	r.mu.Unlock()
	if !deadlineOK || !presentBefore || !absentAtExpiry {
		t.Fatal("incorrect ten-minute expiry boundary")
	}
	if _, err := os.Stat(img.path); !os.IsNotExist(err) {
		t.Fatal("expired file not removed")
	}
}

func TestImageRelayLongHistoryAndCountBound(t *testing.T) {
	for _, count := range []int{22, imageRelayMaxRequestImages, imageRelayMaxRequestImages + 1} {
		r, err := NewImageRelay("https://images.example", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		link := imageLink(t, imageBody(t))
		parts := make([]any, count)
		for i := range parts {
			parts[i] = object{"type": "input_image", "image_url": link}
		}
		raw, _ := json.Marshal(object{"input": []any{object{"role": "user", "content": parts}}})
		out, err := r.Rewrite(raw, "scope")
		if count <= imageRelayMaxRequestImages {
			if err != nil {
				t.Fatal(err)
			}
			var body object
			_ = json.Unmarshal(out, &body)
			got := fixtureValue[[]any](fixtureValue[object](fixtureValue[[]any](body["input"])[0])["content"])
			if len(got) != count {
				t.Fatal("history images dropped")
			}
			for _, v := range got {
				if !strings.HasPrefix(text(fixtureValue[object](v)["image_url"]), "https://images.example/") {
					t.Fatal("image not rewritten")
				}
			}
		} else if err == nil || !strings.Contains(err.Error(), "inline images per request") {
			t.Fatal("image bound not enforced")
		}
		_ = r.Close()
	}
}
