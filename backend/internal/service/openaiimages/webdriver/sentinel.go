package webdriver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/imroc/req/v3"
	"go.uber.org/zap"

	pkglogger "github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// sentinel.go：抓 chatgpt.com 首页解析 SDK 资源 + chat-requirements 握手。

var (
	scriptSrcRe = regexp.MustCompile(`<script[^>]+src="(https?://[^"]+)"`)
	dataBuildRe = regexp.MustCompile(`data-build="([^"]+)"`)
	sdkPathRe   = regexp.MustCompile(`/c/[^/]*/_`)
)

const bootstrapTTL = 5 * time.Minute

type bootstrapEntry struct {
	scripts   []string
	dataBuild string
	expiry    time.Time
}

var (
	bootstrapMu    sync.RWMutex
	bootstrapCache *bootstrapEntry
)

func loadBootstrap() ([]string, string, bool) {
	bootstrapMu.RLock()
	defer bootstrapMu.RUnlock()
	if bootstrapCache == nil || time.Now().After(bootstrapCache.expiry) {
		return nil, "", false
	}
	scripts := append([]string(nil), bootstrapCache.scripts...)
	return scripts, bootstrapCache.dataBuild, true
}

func storeBootstrap(scripts []string, dataBuild string) {
	bootstrapMu.Lock()
	defer bootstrapMu.Unlock()
	bootstrapCache = &bootstrapEntry{
		scripts:   append([]string(nil), scripts...),
		dataBuild: dataBuild,
		expiry:    time.Now().Add(bootstrapTTL),
	}
}

// ResetBootstrapCacheForTest 测试辅助：重置 sentinel 资源缓存。
func ResetBootstrapCacheForTest() {
	bootstrapMu.Lock()
	bootstrapCache = nil
	bootstrapMu.Unlock()
}

// bootstrap 预热 chatgpt.com 并解析 sentinel SDK 资源。失败安全：返回兜底。
func bootstrap(ctx context.Context, client *req.Client, headers http.Header, baseURL string) ([]string, string) {
	if scripts, db, ok := loadBootstrap(); ok && baseURL == startURL {
		return scripts, db
	}
	resp, err := client.R().
		SetContext(ctx).
		SetHeaders(headerToMap(headers)).
		DisableAutoReadResponse().
		Get(baseURL)
	if err != nil || resp == nil || resp.Body == nil {
		return []string{defaultSentinelSDKURL}, ""
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()

	html := string(body)
	var scripts []string
	for _, m := range scriptSrcRe.FindAllStringSubmatch(html, -1) {
		src := m[1]
		if strings.Contains(src, "chatgpt.com") || strings.Contains(src, "127.0.0.1") || strings.Contains(src, "localhost") {
			scripts = append(scripts, src)
		}
	}
	if len(scripts) == 0 {
		scripts = []string{defaultSentinelSDKURL}
	}

	dataBuild := ""
	if m := dataBuildRe.FindStringSubmatch(html); len(m) > 1 {
		dataBuild = m[1]
	}
	if dataBuild == "" {
		for _, s := range scripts {
			if m := sdkPathRe.FindString(s); m != "" {
				dataBuild = m
				break
			}
		}
	}
	if baseURL == startURL {
		storeBootstrap(scripts, dataBuild)
	}
	return scripts, dataBuild
}

// initConversation 调 /backend-api/conversation/init。失败不阻塞主流程（与上游 web 行为一致）。
func initConversation(ctx context.Context, client *req.Client, headers http.Header, baseURL string) error {
	resp, err := client.R().
		SetContext(ctx).
		SetHeaders(headerToMap(headers)).
		SetBodyJsonMarshal(map[string]any{
			"gizmo_id":                nil,
			"requested_default_model": nil,
			"conversation_id":         nil,
			"timezone_offset_min":     tzOffsetMinutes(),
			"system_hints":            []string{"picture_v2"},
		}).
		Post(baseURL)
	if err != nil {
		return &TransportError{Wrapped: err}
	}
	if !resp.IsSuccessState() {
		return classifyHTTPError(resp, "conversation init failed")
	}
	return nil
}

// fetchChatRequirements 拿 sentinel token + PoW 参数。先用 requirements token 试，失败回退 nil。
func fetchChatRequirements(
	ctx context.Context,
	client *req.Client,
	headers http.Header,
	baseURL string,
	scriptSources []string,
	dataBuild string,
) (*chatRequirements, error) {
	ua := headers.Get("User-Agent")
	reqToken := buildRequirementsToken(ua, scriptSources, dataBuild)

	payloads := []map[string]any{
		{"p": reqToken},
		{"p": nil},
	}
	var lastErr error
	for i, payload := range payloads {
		var result chatRequirements
		resp, err := client.R().
			SetContext(ctx).
			SetHeaders(headerToMap(headers)).
			SetBodyJsonMarshal(payload).
			SetSuccessResult(&result).
			Post(baseURL)
		if err != nil {
			lastErr = &TransportError{Wrapped: err}
			continue
		}
		if resp.IsSuccessState() {
			return &result, nil
		}
		body, _ := resp.ToBytes()
		bodyPreview := string(body)
		if len(bodyPreview) > 600 {
			bodyPreview = bodyPreview[:600]
		}
		logPreview := bodyPreview
		pkglogger.L().Warn("openaiimages.chat_requirements_failed",
			zap.Int("attempt", i),
			zap.Int("status", resp.StatusCode),
			zap.String("cf_ray", resp.Header.Get("CF-Ray")),
			zap.String("server", resp.Header.Get("Server")),
			zap.String("content_type", resp.Header.Get("Content-Type")),
			zap.String("body_preview", logPreview),
		)
		lastErr = classifyHTTPError(resp, "chat-requirements failed")
	}
	if lastErr == nil {
		lastErr = errors.New("chat-requirements failed")
	}
	return nil, lastErr
}
