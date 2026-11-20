package service

import (
	"crypto/sha256"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/imroc/req/v3"
)

type webImageClientEntry struct {
	client *req.Client
	used   time.Time
}
type webImageClientPool struct {
	sync.Mutex
	entries map[[32]byte]webImageClientEntry
}

func newWebImageClientPool() *webImageClientPool {
	return &webImageClientPool{entries: make(map[[32]byte]webImageClientEntry)}
}

// Share immutable transports, not authentication or cookie state. Proxy changes
// naturally get a new pool entry; bounded eviction closes only idle connections.
func (p *webImageClientPool) client(proxy string) (*req.Client, error) {
	p.Lock()
	defer p.Unlock()
	key := sha256.Sum256([]byte(proxy))
	entry, ok := p.entries[key]
	if !ok {
		if len(p.entries) >= 64 {
			var oldest [32]byte
			var used time.Time
			for k, e := range p.entries {
				if used.IsZero() || e.used.Before(used) {
					oldest, used = k, e.used
				}
			}
			p.entries[oldest].client.GetTransport().CloseIdleConnections()
			delete(p.entries, oldest)
		}
		client := req.C().ImpersonateChrome().SetCookieJar(nil).SetTimeout(10*time.Minute).
			SetDial((&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext).
			SetTLSHandshakeTimeout(15*time.Second).
			SetRedirectPolicy(req.MaxRedirectPolicy(5), webImageRedirectPolicy)
		client.GetTransport().SetIdleConnTimeout(90 * time.Second).SetResponseHeaderTimeout(45 * time.Second).SetMaxIdleConns(16)
		if proxy != "" {
			client.SetProxyURL(proxy)
		}
		entry.client = client
	}
	entry.used = time.Now()
	p.entries[key] = entry
	return entry.client, nil
}

func webImageRedirectPolicy(request *http.Request, _ []*http.Request) error {
	host := strings.ToLower(request.URL.Hostname())
	if request.URL.Scheme == "https" && (host == "chatgpt.com" || strings.HasSuffix(host, ".chatgpt.com")) {
		return nil
	}
	for _, key := range []string{"Authorization", "Cookie", "ChatGPT-Account-Id", "Oai-Did", "Oai-Device-Id", "OAI-Session-Id", "OpenAI-Sentinel-Chat-Requirements-Token", "OpenAI-Sentinel-Proof-Token", "X-Conduit-Token"} {
		request.Header.Del(key)
	}
	return nil
}
