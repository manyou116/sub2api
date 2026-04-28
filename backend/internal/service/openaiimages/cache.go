package openaiimages

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ImageCache 把生成的图片字节落盘到本地，签发可在 TTL 内通过 HTTP 访问的短链。
//
// 设计目标：
//   - 用户请求 response_format=url 时，对 WebDriver / ResponsesToolDriver 路径
//     （上游不直接返回可用 url）也能返回 url 形式；
//   - 重启后仍可访问（落盘），TTL 24h；
//   - 后台 goroutine 定期 GC 过期文件；
//   - 内存索引用于 O(1) 查找 mime/exp，不存字节。
type ImageCache struct {
	dir string
	ttl time.Duration

	mu      sync.RWMutex
	entries map[string]*cacheEntry

	stopOnce sync.Once
	stopCh   chan struct{}
}

type cacheEntry struct {
	mime    string
	expires time.Time
}

// NewImageCache 创建并启动一个 cache。dir 为空使用 ./data/image_cache。ttl<=0 使用 24h。
func NewImageCache(dir string, ttl time.Duration) (*ImageCache, error) {
	if dir == "" {
		dir = filepath.Join(".", "data", "image_cache")
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create image cache dir: %w", err)
	}
	c := &ImageCache{
		dir:     dir,
		ttl:     ttl,
		entries: make(map[string]*cacheEntry),
		stopCh:  make(chan struct{}),
	}
	c.scanExisting()
	go c.gcLoop()
	return c, nil
}

// Close 停止 GC 循环（测试用）。
func (c *ImageCache) Close() {
	c.stopOnce.Do(func() { close(c.stopCh) })
}

// Put 写入字节并返回随机 id。mime 用于决定扩展名与读取时回放。
func (c *ImageCache) Put(data []byte, mime string) (string, error) {
	if len(data) == 0 {
		return "", errors.New("empty image data")
	}
	if mime == "" {
		mime = "image/png"
	}
	id, err := newCacheID()
	if err != nil {
		return "", err
	}
	path := c.fileFor(id, mime)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write cache file: %w", err)
	}
	c.mu.Lock()
	c.entries[id] = &cacheEntry{mime: mime, expires: time.Now().Add(c.ttl)}
	c.mu.Unlock()
	return id, nil
}

// Get 读取字节与 mime；过期或不存在返回 ok=false。
func (c *ImageCache) Get(id string) ([]byte, string, bool) {
	c.mu.RLock()
	e, ok := c.entries[id]
	c.mu.RUnlock()
	if !ok {
		return nil, "", false
	}
	if time.Now().After(e.expires) {
		c.deleteEntry(id, e.mime)
		return nil, "", false
	}
	data, err := os.ReadFile(c.fileFor(id, e.mime))
	if err != nil {
		return nil, "", false
	}
	return data, e.mime, true
}

func (c *ImageCache) fileFor(id, mime string) string {
	return filepath.Join(c.dir, id+extForMime(mime))
}

func (c *ImageCache) deleteEntry(id, mime string) {
	c.mu.Lock()
	delete(c.entries, id)
	c.mu.Unlock()
	_ = os.Remove(c.fileFor(id, mime))
}

func (c *ImageCache) gcLoop() {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-t.C:
			c.gcOnce()
		}
	}
}

func (c *ImageCache) gcOnce() {
	now := time.Now()
	type victim struct{ id, mime string }
	var victims []victim
	c.mu.RLock()
	for id, e := range c.entries {
		if now.After(e.expires) {
			victims = append(victims, victim{id, e.mime})
		}
	}
	c.mu.RUnlock()
	for _, v := range victims {
		c.deleteEntry(v.id, v.mime)
	}
}

// scanExisting 启动时把已落盘的文件挂回索引（按文件 mtime + ttl 推断过期）。
func (c *ImageCache) scanExisting() {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	now := time.Now()
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		ext := filepath.Ext(name)
		id := strings.TrimSuffix(name, ext)
		if len(id) < 16 {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			continue
		}
		exp := info.ModTime().Add(c.ttl)
		if now.After(exp) {
			_ = os.Remove(filepath.Join(c.dir, name))
			continue
		}
		c.entries[id] = &cacheEntry{mime: mimeForExt(ext), expires: exp}
	}
}

func newCacheID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

func extForMime(mime string) string {
	switch strings.ToLower(mime) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ".png"
	}
}

func mimeForExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	default:
		return "image/png"
	}
}
