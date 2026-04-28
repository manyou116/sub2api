package webdriver

import (
	"context"

	"github.com/imroc/req/v3"
)

// NewProbeClient 暴露给外部包（如 openaiimages.AccountProbe）使用的 client 构造器。
// 与 webdriver 内部对话流共享同一份 TLS 指纹 + 头集合，避免 probe 用简陋请求被
// chatgpt.com 反爬命中而稳定 403。
//
// 调用方典型流程：
//
//	fp := webdriver.PickFingerprint(account.ID)
//	c, _ := webdriver.NewProbeClient(proxyURL, fp)
//	headers := webdriver.BuildBearerHeaders(accessToken, fp)
//	c.R().SetHeaderMultiValues(headers).Get("https://chatgpt.com/backend-api/me")
func NewProbeClient(proxyURL string, fp Fingerprint) (*req.Client, error) {
	return newHTTPClient(proxyURL, fp)
}

// BuildBearerHeadersMap 与 BuildBearerHeaders 等价，返回 imroc/req 偏好的扁平 map（取每个 key 的首个值）。
func BuildBearerHeadersMap(accessToken string, fp Fingerprint) map[string]string {
	return headerToMap(buildHeaders(AccountInfo{AccessToken: accessToken}, fp))
}

// PrimeChatGPTSession 模拟浏览器导航到 https://chatgpt.com/，让 client 的 cookie jar 拿到
// cf_clearance 等 anti-bot cookie。后续 backend-api 调用必须复用同一 client 才有效。
//
// 返回 error 仅用于 transport 失败；HTTP 状态码忽略（即便 403/503 也可能种 cookie）。
func PrimeChatGPTSession(ctx context.Context, c *req.Client, fp Fingerprint) error {
	headers := headerToMap(buildBootstrapHeaders(AccountInfo{}, fp))
	resp, err := c.R().SetContext(ctx).SetHeaders(headers).Get(startURL)
	if err != nil {
		return err
	}
	_ = resp
	return nil
}
