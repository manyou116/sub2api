package webdriver

import (
	"github.com/bogdanfinn/tls-client/profiles"
)

// Fingerprint 描述一组**协调一致**的浏览器指纹（TLS+HTTP/2 profile + UA + sec-ch-ua）。
//
// 上游 ChatGPT 反爬会比对 TLS/HTTP2 指纹与 HTTP 头声明的浏览器是否一致；同时同源短时
// 多账号共享一套指纹会被识别为"同一浏览器多账号切换"，触发更严的 moderation。
//
// 每个 OAuth 账号通过 PickFingerprint(accountID) 得到**稳定**的 Fingerprint —— 同一账号
// 始终复用同一份指纹，使上游把每个账号视作独立的真实浏览器实例。
//
// **底层实现**：使用 bogdanfinn/tls-client 提供的 ClientProfile，覆盖 TLS ClientHello
// + HTTP/2 SETTINGS + HEADERS 顺序 + HPACK encoding，绕过 Cloudflare JA4_H + Akamai_H2
// 联合指纹检测（旧的 utls-only 方案在 chatgpt.com 上稳定 403 challenge）。
type Fingerprint struct {
	Name string

	// Profile 决定 TLS+HTTP/2 字节级伪装；由 tls-client/profiles 提供。
	Profile profiles.ClientProfile

	UserAgent              string
	SecChUa                string
	SecChUaFullVersion     string
	SecChUaFullVersionList string
	SecChUaPlatform        string // 带引号
	SecChUaPlatformVersion string
	SecChUaArch            string
	SecChUaBitness         string

	OAIClientVersion     string
	OAIClientBuildNumber string
}

// fingerprints 是固定的 profile 池。新增/调整时务必保证内部协调一致：
//   - sec-ch-ua 主版本 = UA 中的版本 = profile 名（Chrome_131 → Chrome/131）
//   - 不要使用 Chrome_144/146 等过新版本（CF 当前严打高频被滥用版本）
var fingerprints = []Fingerprint{
	{
		Name:                   "chrome-131-win-x64",
		Profile:                profiles.Chrome_131,
		UserAgent:              "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
		SecChUa:                `"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`,
		SecChUaFullVersion:     `"131.0.6778.205"`,
		SecChUaFullVersionList: `"Google Chrome";v="131.0.6778.205", "Chromium";v="131.0.6778.205", "Not_A Brand";v="24.0.0.0"`,
		SecChUaPlatform:        `"Windows"`,
		SecChUaPlatformVersion: `"19.0.0"`,
		SecChUaArch:            `"x86"`,
		SecChUaBitness:         `"64"`,
		OAIClientVersion:       defaultClientVersion,
		OAIClientBuildNumber:   defaultClientBuildNumber,
	},
	{
		Name:                   "chrome-124-win-x64",
		Profile:                profiles.Chrome_124,
		UserAgent:              "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
		SecChUa:                `"Google Chrome";v="124", "Chromium";v="124", "Not-A.Brand";v="99"`,
		SecChUaFullVersion:     `"124.0.6367.207"`,
		SecChUaFullVersionList: `"Google Chrome";v="124.0.6367.207", "Chromium";v="124.0.6367.207", "Not-A.Brand";v="99.0.0.0"`,
		SecChUaPlatform:        `"Windows"`,
		SecChUaPlatformVersion: `"19.0.0"`,
		SecChUaArch:            `"x86"`,
		SecChUaBitness:         `"64"`,
		OAIClientVersion:       defaultClientVersion,
		OAIClientBuildNumber:   defaultClientBuildNumber,
	},
	{
		Name:                   "safari-ios-17",
		Profile:                profiles.Safari_IOS_17_0,
		UserAgent:              "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1",
		SecChUa:                "",
		SecChUaFullVersion:     "",
		SecChUaFullVersionList: "",
		SecChUaPlatform:        `"iOS"`,
		SecChUaPlatformVersion: `"17.0.0"`,
		SecChUaArch:            "",
		SecChUaBitness:         "",
		OAIClientVersion:       defaultClientVersion,
		OAIClientBuildNumber:   defaultClientBuildNumber,
	},
}

// PickFingerprint 返回与账号绑定的浏览器指纹。
//
// 当前策略：稳定使用 fingerprints[0]（Chrome 131）—— 多 profile 池在历史上曾导致部分
// TLS+UA 组合被 CF 标记。如要恢复账号级隔离，改回 `fingerprints[accountID%int64(len(fingerprints))]`
// 并先用一组账号灰度验证 cf-ray 不再出现 challenge=403。
func PickFingerprint(accountID int64) Fingerprint {
	if len(fingerprints) == 0 {
		return Fingerprint{}
	}
	return fingerprints[0]
}
