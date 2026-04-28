package webdriver

import (
	"net/http"
	"strings"
	"time"

	"github.com/imroc/req/v3"
	utls "github.com/refraction-networking/utls"
)

// 与 chatgpt2api/services/openai_backend_api.py 对齐的 Edge 143 / Windows / x86 指纹常量。
// CF 反爬会比对 TLS 指纹（Chromium 系 BoringSSL）+ HTTP 头自报浏览器是否一致；
// 这里整套用 Edge 143 Windows，避免出现 "TLS=Chrome 头=macOS arm Chrome 131" 的不一致。
const (
	chatgptOrigin              = "https://chatgpt.com"
	chatgptReferer             = "https://chatgpt.com/"
	defaultClientVersion       = "prod-be885abbfcfe7b1f511e88b3003d9ee44757fbad"
	defaultClientBuildNumber   = "5955942"
	defaultLanguage            = "zh-CN"
	defaultAcceptLanguage      = "zh-CN,zh;q=0.9,en;q=0.8,en-US;q=0.7"
	defaultSecChUa             = `"Microsoft Edge";v="143", "Chromium";v="143", "Not A(Brand";v="24"`
	defaultSecChUaFullVersion  = `"143.0.3650.96"`
	defaultSecChUaFullVerList  = `"Microsoft Edge";v="143.0.3650.96", "Chromium";v="143.0.7499.147", "Not A(Brand";v="24.0.0.0"`
	defaultSecChUaArch         = `"x86"`
	defaultSecChUaBitness      = `"64"`
	defaultSecChUaPlatform     = `"Windows"`
	defaultSecChUaPlatformVer  = `"19.0.0"`
)

// newHTTPClient 构造带 Chromium TLS 指纹的 imroc/req 客户端。
// imroc/req v3.57.0 没有 Edge impersonate；Edge 143 与 Chrome 143 同为 BoringSSL，
// utls.HelloChrome_133 的 ClientHello 比 HelloChrome_120 更接近 Edge 143。
func newHTTPClient(proxyURL string) (*req.Client, error) {
	c := req.C().
		SetTimeout(180 * time.Second).
		ImpersonateChrome().
		SetTLSFingerprint(utls.HelloChrome_133)
	if trimmed := strings.TrimSpace(proxyURL); trimmed != "" {
		c.SetProxyURL(trimmed)
	}
	return c, nil
}

// buildBootstrapHeaders 构造首页 GET 预热的"浏览器导航"专用头。
// 关键差异：无 Authorization / Content-Type；Sec-Fetch-Mode=navigate；
// Accept 是 HTML 类型；附带 Upgrade-Insecure-Requests:1。
// 参照 chatgpt2api/services/openai_backend_api.py::_bootstrap_headers。
func buildBootstrapHeaders(account AccountInfo) http.Header {
	ua := coalesce(account.UserAgent, defaultUserAgent)
	h := make(http.Header)
	h.Set("User-Agent", ua)
	h.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	h.Set("Accept-Language", defaultAcceptLanguage)
	h.Set("Sec-Ch-Ua", defaultSecChUa)
	h.Set("Sec-Ch-Ua-Mobile", "?0")
	h.Set("Sec-Ch-Ua-Platform", defaultSecChUaPlatform)
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
// 头声明 = Edge 143 / Windows / x86，与 newHTTPClient 的 Chromium TLS 指纹协调一致。
// account 为账号信息（access token、device/session id 等），全部从外层透传。
func buildHeaders(account AccountInfo) http.Header {
	ua := coalesce(account.UserAgent, defaultUserAgent)

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

	h.Set("Sec-Ch-Ua", defaultSecChUa)
	h.Set("Sec-Ch-Ua-Arch", defaultSecChUaArch)
	h.Set("Sec-Ch-Ua-Bitness", defaultSecChUaBitness)
	h.Set("Sec-Ch-Ua-Full-Version", defaultSecChUaFullVersion)
	h.Set("Sec-Ch-Ua-Full-Version-List", defaultSecChUaFullVerList)
	h.Set("Sec-Ch-Ua-Mobile", "?0")
	h.Set("Sec-Ch-Ua-Model", `""`)
	h.Set("Sec-Ch-Ua-Platform", defaultSecChUaPlatform)
	h.Set("Sec-Ch-Ua-Platform-Version", defaultSecChUaPlatformVer)
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "same-origin")

	h.Set("OAI-Client-Version", defaultClientVersion)
	h.Set("OAI-Client-Build-Number", defaultClientBuildNumber)
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

