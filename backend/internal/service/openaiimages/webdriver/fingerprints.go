package webdriver

import (
	utls "github.com/refraction-networking/utls"
)

// Fingerprint 描述一组**协调一致**的浏览器指纹（TLS ClientHello + UA + sec-ch-ua + 客户端构建号）。
//
// 上游 ChatGPT 反爬会比对 TLS 指纹与 HTTP 头声明的浏览器是否一致；同时同源短时多账号
// 共享一套指纹会被识别为"同一浏览器多账号切换"，触发更严的 moderation。
//
// 每个 OAuth 账号通过 PickFingerprint(accountID) 得到**稳定**的 Fingerprint —— 同一账号
// 始终复用同一份指纹，使上游把每个账号视作独立的真实浏览器实例。
//
// 所有 profile 都是 Edge / Windows / x86，与 chatgpt2api 验证可用的配置同档；
// 仅在浏览器主版本/补丁号 + TLS hello 版本上做温和扩散。
type Fingerprint struct {
	Name string

	// TLSHello 决定 ClientHello 字节序列；同 BoringSSL 系列的 Chrome/Edge 共享。
	TLSHello utls.ClientHelloID

	UserAgent             string
	SecChUa               string
	SecChUaFullVersion    string
	SecChUaFullVersionList string
	SecChUaPlatform       string // 带引号
	SecChUaPlatformVersion string
	SecChUaArch           string
	SecChUaBitness        string

	OAIClientVersion     string
	OAIClientBuildNumber string
}

// fingerprints 是固定的 profile 池。新增/调整时务必保证内部协调一致：
//   - sec-ch-ua 主版本 = UA 中的版本
//   - TLS hello 版本不要早于 UA 中的浏览器版本太多
//   - chatgpt2api 实战可用的 prod-* OAIClientVersion 不必改
var fingerprints = []Fingerprint{
	{
		Name:                   "edge-143-win-x86",
		TLSHello:               utls.HelloChrome_133,
		UserAgent:              "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36 Edg/143.0.0.0",
		SecChUa:                `"Microsoft Edge";v="143", "Chromium";v="143", "Not A(Brand";v="24"`,
		SecChUaFullVersion:     `"143.0.3650.96"`,
		SecChUaFullVersionList: `"Microsoft Edge";v="143.0.3650.96", "Chromium";v="143.0.7499.147", "Not A(Brand";v="24.0.0.0"`,
		SecChUaPlatform:        `"Windows"`,
		SecChUaPlatformVersion: `"19.0.0"`,
		SecChUaArch:            `"x86"`,
		SecChUaBitness:         `"64"`,
		OAIClientVersion:       defaultClientVersion,
		OAIClientBuildNumber:   defaultClientBuildNumber,
	},
	{
		Name:                   "edge-142-win-x86",
		TLSHello:               utls.HelloChrome_131,
		UserAgent:              "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36 Edg/142.0.0.0",
		SecChUa:                `"Microsoft Edge";v="142", "Chromium";v="142", "Not A(Brand";v="24"`,
		SecChUaFullVersion:     `"142.0.3595.32"`,
		SecChUaFullVersionList: `"Microsoft Edge";v="142.0.3595.32", "Chromium";v="142.0.7444.142", "Not A(Brand";v="24.0.0.0"`,
		SecChUaPlatform:        `"Windows"`,
		SecChUaPlatformVersion: `"19.0.0"`,
		SecChUaArch:            `"x86"`,
		SecChUaBitness:         `"64"`,
		OAIClientVersion:       defaultClientVersion,
		OAIClientBuildNumber:   defaultClientBuildNumber,
	},
	{
		Name:                   "chrome-143-win-x86",
		TLSHello:               utls.HelloChrome_133,
		UserAgent:              "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.7499.147 Safari/537.36",
		SecChUa:                `"Google Chrome";v="143", "Chromium";v="143", "Not A(Brand";v="24"`,
		SecChUaFullVersion:     `"143.0.7499.147"`,
		SecChUaFullVersionList: `"Google Chrome";v="143.0.7499.147", "Chromium";v="143.0.7499.147", "Not A(Brand";v="24.0.0.0"`,
		SecChUaPlatform:        `"Windows"`,
		SecChUaPlatformVersion: `"19.0.0"`,
		SecChUaArch:            `"x86"`,
		SecChUaBitness:         `"64"`,
		OAIClientVersion:       defaultClientVersion,
		OAIClientBuildNumber:   defaultClientBuildNumber,
	},
	{
		Name:                   "chrome-142-win-x86",
		TLSHello:               utls.HelloChrome_131,
		UserAgent:              "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.7444.142 Safari/537.36",
		SecChUa:                `"Google Chrome";v="142", "Chromium";v="142", "Not A(Brand";v="24"`,
		SecChUaFullVersion:     `"142.0.7444.142"`,
		SecChUaFullVersionList: `"Google Chrome";v="142.0.7444.142", "Chromium";v="142.0.7444.142", "Not A(Brand";v="24.0.0.0"`,
		SecChUaPlatform:        `"Windows"`,
		SecChUaPlatformVersion: `"19.0.0"`,
		SecChUaArch:            `"x86"`,
		SecChUaBitness:         `"64"`,
		OAIClientVersion:       defaultClientVersion,
		OAIClientBuildNumber:   defaultClientBuildNumber,
	},
}

// PickFingerprint 返回与账号绑定的浏览器指纹。
//
// **当前策略**：实测多 profile 池被 Cloudflare 标记（部分 TLS+UA 组合直接 challenge），
// 暂时统一使用 fingerprints[0]（Edge 143 / HelloChrome_133，与 11a393d9 之前的稳定 prod 行为一致）。
// 池中其余 profile 保留作未来 A/B 测试与 chatgpt2api 对齐参考，**不**经由本函数下发。
//
// 如果未来要恢复账号级指纹隔离，把这里改回 `fingerprints[accountID%int64(len(fingerprints))]`
// 并先用一组账号灰度验证 cf-ray 不再出现 challenge=403 即可。
func PickFingerprint(accountID int64) Fingerprint {
	if len(fingerprints) == 0 {
		return Fingerprint{}
	}
	return fingerprints[0]
}
