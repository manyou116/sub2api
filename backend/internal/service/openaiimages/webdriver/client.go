package webdriver

import (
	"net/http"
	"strings"
	"time"

	"github.com/imroc/req/v3"
)

// 与 chatgpt2api/services/openai_backend_api.py 对齐的浏览器指纹常量。
// 多 profile 定义见 fingerprints.go；下列常量为不随 profile 变化的部分。
const (
	chatgptOrigin            = "https://chatgpt.com"
	chatgptReferer           = "https://chatgpt.com/"
	defaultClientVersion     = "prod-be885abbfcfe7b1f511e88b3003d9ee44757fbad"
	defaultClientBuildNumber = "5955942"
	defaultLanguage          = "zh-CN"
	defaultAcceptLanguage    = "zh-CN,zh;q=0.9,en;q=0.8,en-US;q=0.7"
)

// newHTTPClient 构造带 Chromium TLS 指纹的 imroc/req 客户端。
// 传入的 fingerprint 决定了 TLS ClientHello 字节序列；调用方负责选取与账号绑定的同款 Fingerprint
// 给 buildHeaders / buildBootstrapHeaders，确保 TLS 与 HTTP 头声明的浏览器一致。
func newHTTPClient(proxyURL string, fp Fingerprint) (*req.Client, error) {
	c := req.C().
		SetTimeout(180 * time.Second).
		ImpersonateChrome().
		SetTLSFingerprint(fp.TLSHello)
	if trimmed := strings.TrimSpace(proxyURL); trimmed != "" {
		c.SetProxyURL(trimmed)
	}
	return c, nil
}

// buildBootstrapHeaders 构造首页 GET 预热的"浏览器导航"专用头。
// 关键差异：无 Authorization / Content-Type；Sec-Fetch-Mode=navigate；
// Accept 是 HTML 类型；附带 Upgrade-Insecure-Requests:1。
// 参照 chatgpt2api/services/openai_backend_api.py::_bootstrap_headers。
func buildBootstrapHeaders(account AccountInfo, fp Fingerprint) http.Header {
	ua := coalesce(account.UserAgent, fp.UserAgent)
	h := make(http.Header)
	h.Set("User-Agent", ua)
	h.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	h.Set("Accept-Language", defaultAcceptLanguage)
	h.Set("Sec-Ch-Ua", fp.SecChUa)
	h.Set("Sec-Ch-Ua-Mobile", "?0")
	h.Set("Sec-Ch-Ua-Platform", fp.SecChUaPlatform)
	h.Set("Sec-Fetch-Dest", "document")
	h.Set("Sec-Fetch-Mode", "navigate")
	h.Set("Sec-Fetch-Site", "none")
	h.Set("Sec-Fetch-User", "?1")
	h.Set("Upgrade-Insecure-Requests", "1")
	if account.DeviceID != "" {
		h.Set("Cookie", "oai-did="+account.DeviceID)
	}
	return h
}

// buildHeaders 构造对话/sentinel/upload 等 backend-api 专用头（XHR/fetch 模式）。
// 头声明的浏览器版本来自 fp，须与 newHTTPClient(_, fp) 使用同一份 Fingerprint，
// 以避免 "TLS=Chrome 头=Edge" 的内外不一致而被反爬命中。
func buildHeaders(account AccountInfo, fp Fingerprint) http.Header {
	ua := coalesce(account.UserAgent, fp.UserAgent)

	h := make(http.Header)
	h.Set("Authorization", "Bearer "+account.AccessToken)
	h.Set("Accept", "*/*")
	h.Set("Content-Type", "application/json")
	h.Set("Origin", chatgptOrigin)
	h.Set("Referer", chatgptReferer)
	h.Set("User-Agent", ua)
	h.Set("Accept-Language", defaultAcceptLanguage)
	h.Set("Cache-Control", "no-cache")
	h.Set("Pragma", "no-cache")
	h.Set("Priority", "u=1, i")

	h.Set("Sec-Ch-Ua", fp.SecChUa)
	h.Set("Sec-Ch-Ua-Arch", fp.SecChUaArch)
	h.Set("Sec-Ch-Ua-Bitness", fp.SecChUaBitness)
	h.Set("Sec-Ch-Ua-Full-Version", fp.SecChUaFullVersion)
	h.Set("Sec-Ch-Ua-Full-Version-List", fp.SecChUaFullVersionList)
	h.Set("Sec-Ch-Ua-Mobile", "?0")
	h.Set("Sec-Ch-Ua-Model", `""`)
	h.Set("Sec-Ch-Ua-Platform", fp.SecChUaPlatform)
	h.Set("Sec-Ch-Ua-Platform-Version", fp.SecChUaPlatformVersion)
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "same-origin")

	h.Set("OAI-Client-Version", fp.OAIClientVersion)
	h.Set("OAI-Client-Build-Number", fp.OAIClientBuildNumber)
	h.Set("OAI-Language", defaultLanguage)

	if id := strings.TrimSpace(account.ChatGPTAccountID); id != "" {
		h.Set("Chatgpt-Account-Id", id)
	}
	if account.DeviceID != "" {
		h.Set("Oai-Device-Id", account.DeviceID)
		h.Set("Cookie", "oai-did="+account.DeviceID)
	}
	if account.SessionID != "" {
		h.Set("Oai-Session-Id", account.SessionID)
	}
	return h
}

