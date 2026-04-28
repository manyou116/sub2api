package openaiimages

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestImageCachePutGet(t *testing.T) {
	dir := t.TempDir()
	c, err := NewImageCache(dir, time.Hour)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer c.Close()

	id, err := c.Put([]byte("hello-png"), "image/png")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if id == "" || len(id) != 32 {
		t.Fatalf("bad id %q", id)
	}
	data, mime, ok := c.Get(id)
	if !ok || string(data) != "hello-png" || mime != "image/png" {
		t.Fatalf("get: ok=%v mime=%q data=%q", ok, mime, string(data))
	}
	// File must be on disk
	if _, err := os.Stat(filepath.Join(dir, id+".png")); err != nil {
		t.Fatalf("file missing: %v", err)
	}
}

func TestImageCacheExpiry(t *testing.T) {
	dir := t.TempDir()
	c, err := NewImageCache(dir, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer c.Close()
	id, err := c.Put([]byte("x"), "image/jpeg")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	if _, _, ok := c.Get(id); ok {
		t.Fatalf("expected expired")
	}
}

func TestImageCacheScanExisting(t *testing.T) {
	dir := t.TempDir()
	c1, err := NewImageCache(dir, time.Hour)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	id, _ := c1.Put([]byte("persist"), "image/webp")
	c1.Close()
	c2, err := NewImageCache(dir, time.Hour)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer c2.Close()
	data, mime, ok := c2.Get(id)
	if !ok || string(data) != "persist" || mime != "image/webp" {
		t.Fatalf("scan: ok=%v mime=%q", ok, mime)
	}
}

func TestImageCachePutEmpty(t *testing.T) {
	dir := t.TempDir()
	c, _ := NewImageCache(dir, time.Hour)
	defer c.Close()
	if _, err := c.Put(nil, "image/png"); err == nil {
		t.Fatal("expected error for empty data")
	}
}
