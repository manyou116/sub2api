package webdriver

import (
	"net/http"
	"strings"
	"time"
)

// ChatGPT Web 协议常量。
const (
	startURL              = "https://chatgpt.com/"
	conversationInitURL   = "https://chatgpt.com/backend-api/conversation/init"
	conversationURL       = "https://chatgpt.com/backend-api/f/conversation"
	conversationPrepareURL = "https://chatgpt.com/backend-api/f/conversation/prepare"
	chatRequirementsURL   = "https://chatgpt.com/backend-api/sentinel/chat-requirements"
	filesURL              = "https://chatgpt.com/backend-api/files"
	defaultSentinelSDKURL = "https://chatgpt.com/backend-api/sentinel/sdk.js"

	requirementsTokenDifficulty = "0fffff"
	defaultUserAgent            = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36 Edg/143.0.0.0"
	maxImageDownloadBytes       = 20 << 20

	lifecycleTimeout = 5 * time.Minute
	pollDeadline     = 4 * time.Minute
)

func cloneHTTPHeader(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for k, v := range src {
		copied := make([]string, len(v))
		copy(copied, v)
		dst[k] = copied
	}
	return dst
}

func headerToMap(h http.Header) map[string]string {
	m := make(map[string]string, len(h))
	for k := range h {
		m[k] = h.Get(k)
	}
	return m
}

// extractSSEDataLine 从单行 SSE 中提取 `data:` 之后的 payload。
func extractSSEDataLine(line string) (string, bool) {
	const prefix = "data:"
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	return strings.TrimSpace(line[len(prefix):]), true
}

func tzOffsetMinutes() int {
	_, off := time.Now().Zone()
	return off / 60
}

func tzName() string {
	name, _ := time.Now().Zone()
	if name == "" {
		return "UTC"
	}
	return name
}

func coalesce(s, fallback string) string {
	if strings.TrimSpace(s) != "" {
		return s
	}
	return fallback
}
