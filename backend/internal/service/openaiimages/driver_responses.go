package openaiimages

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/imroc/req/v3"
)

// ResponsesToolDriver 用 OpenAI 官方 /v1/responses 的 image_generation tool
// 让 OAuth 账号在不走 ChatGPT Web 反代的前提下生图。
//
// 适用账号：OAuth 账号 + 未启用 web 生图（Group.OpenAILegacyImagesDefault=disabled
// 且 account.extra.openai_oauth_legacy_images != "enabled"）。
//
// 协议要点：
//   POST https://api.openai.com/v1/responses
//   Body:
//     {
//       "model": "gpt-image-1" 或别名,
//       "input": "<prompt>",
//       "tools": [{"type": "image_generation", "size": "1024x1024", ...}],
//       "tool_choice": {"type": "image_generation"},
//       "stream": false
//     }
//   Response (非流) 中 output[].type == "image_generation_call"
//   字段 result 为 base64-encoded PNG。
//
// 注意：图片编辑场景，把第一张 image 的 base64 放进 tool 的 input_image 字段（OpenAI 当前 spec）。
type ResponsesToolDriver struct {
	BaseURL string
	Client  *req.Client
	Now     func() time.Time
}

// NewResponsesToolDriver 创建带默认配置的实例。
func NewResponsesToolDriver() *ResponsesToolDriver {
	return &ResponsesToolDriver{
		BaseURL: "https://api.openai.com",
		Client:  req.C().SetTimeout(240 * time.Second),
		Now:     time.Now,
	}
}

func (d *ResponsesToolDriver) Name() string { return "responses-tool" }

func (d *ResponsesToolDriver) baseURL() string {
	if d.BaseURL != "" {
		return d.BaseURL
	}
	return "https://api.openai.com"
}

func (d *ResponsesToolDriver) httpClient() *req.Client {
	if d.Client != nil {
		return d.Client
	}
	d.Client = req.C().SetTimeout(240 * time.Second)
	return d.Client
}

func (d *ResponsesToolDriver) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Forward 实现 Driver 接口。
func (d *ResponsesToolDriver) Forward(ctx context.Context, account AccountView, request *ImagesRequest) (*ImageResult, error) {
	token := account.AccessToken()
	if token == "" {
		token = account.APIKey()
	}
	if token == "" {
		return nil, &AuthError{Reason: "missing access token / api key"}
	}

	client := d.httpClient()
	if proxy := account.ProxyURL(); proxy != "" {
		client = client.Clone().SetProxyURL(proxy)
	}

	body := d.buildBody(request)
	resp, err := client.R().SetContext(ctx).
		SetHeader("authorization", "Bearer "+token).
		SetHeader("content-type", "application/json").
		SetHeader("openai-beta", "responses=v1").
		SetBodyJsonMarshal(body).
		Post(d.baseURL() + "/v1/responses")
	if err != nil {
		return nil, &TransportError{Reason: err.Error()}
	}
	return d.parseResponse(resp, request)
}

func (d *ResponsesToolDriver) buildBody(request *ImagesRequest) map[string]any {
	tool := map[string]any{
		"type": "image_generation",
	}
	if request.Size != "" {
		tool["size"] = request.Size
	}
	if request.Quality != "" {
		tool["quality"] = request.Quality
	}
	if request.Background != "" {
		tool["background"] = request.Background
	}

	body := map[string]any{
		"model":       upstreamModel(request.Model),
		"input":       d.buildInput(request),
		"tools":       []any{tool},
		"tool_choice": map[string]any{"type": "image_generation"},
		"stream":      false,
	}
	if request.User != "" {
		body["user"] = request.User
	}
	for k, v := range request.Extras {
		if _, exists := body[k]; !exists {
			body[k] = v
		}
	}
	return body
}

// buildInput 把 prompt 与可选的 source images 合成 Responses input 数组。
//
// 简化策略：
//   - 无图：input 直接 string
//   - 有图：input 是单条 user message，含 input_text + input_image
func (d *ResponsesToolDriver) buildInput(request *ImagesRequest) any {
	if len(request.Images) == 0 {
		return request.Prompt
	}
	content := []any{
		map[string]any{"type": "input_text", "text": request.Prompt},
	}
	for _, img := range request.Images {
		mime := img.ContentType
		if mime == "" {
			mime = "image/png"
		}
		content = append(content, map[string]any{
			"type":      "input_image",
			"image_url": fmt.Sprintf("data:%s;base64,%s", mime, encodeBase64(img.Data)),
		})
	}
	return []any{
		map[string]any{
			"role":    "user",
			"content": content,
		},
	}
}

func (d *ResponsesToolDriver) parseResponse(resp *req.Response, request *ImagesRequest) (*ImageResult, error) {
	if resp == nil {
		return nil, &TransportError{Reason: "empty response"}
	}
	body := resp.Bytes()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, &AuthError{Reason: extractAPIError(body), HTTPStatus: resp.StatusCode}
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, &RateLimitError{
			ResetAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			Reason:     extractAPIError(body),
			HTTPStatus: resp.StatusCode,
		}
	case resp.StatusCode >= 500:
		return nil, &TransportError{Reason: extractAPIError(body), HTTPStatus: resp.StatusCode}
	case resp.StatusCode >= 400:
		return nil, &UpstreamError{HTTPStatus: resp.StatusCode, Body: body}
	}

	var payload struct {
		Output []struct {
			Type   string `json:"type"`
			Result string `json:"result"` // base64 PNG
			Status string `json:"status"`
			Error  *struct {
				Message string `json:"message"`
				Code    string `json:"code"`
			} `json:"error"`
			RevisedPrompt string `json:"revised_prompt"`
		} `json:"output"`
		Model     string `json:"model"`
		CreatedAt int64  `json:"created_at"`
		Usage     *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
		Status string `json:"status"`
		Error  *struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, &UpstreamError{HTTPStatus: resp.StatusCode, Body: body, Reason: "unparseable JSON: " + err.Error()}
	}
	if payload.Error != nil && payload.Error.Message != "" {
		return nil, &UpstreamError{HTTPStatus: resp.StatusCode, Body: body, Reason: payload.Error.Message}
	}

	out := &ImageResult{
		Model:   coalesceStr(payload.Model, request.Model),
		Created: coalesceInt64(payload.CreatedAt, d.now().Unix()),
	}
	for _, item := range payload.Output {
		if !strings.EqualFold(item.Type, "image_generation_call") {
			continue
		}
		if item.Error != nil && item.Error.Message != "" {
			return nil, &UpstreamError{HTTPStatus: resp.StatusCode, Body: body, Reason: item.Error.Message}
		}
		if item.Result == "" {
			continue
		}
		out.Items = append(out.Items, ImageItem{
			B64JSON:       item.Result,
			RevisedPrompt: item.RevisedPrompt,
			MimeType:      "image/png",
		})
	}
	if len(out.Items) == 0 {
		return nil, &UpstreamError{HTTPStatus: resp.StatusCode, Body: body, Reason: "no image_generation_call output"}
	}
	if payload.Usage != nil {
		out.Usage = Usage{
			InputTokens:  payload.Usage.InputTokens,
			OutputTokens: payload.Usage.OutputTokens,
			TotalTokens:  payload.Usage.TotalTokens,
			ImagesCount:  len(out.Items),
		}
	} else {
		out.Usage = Usage{ImagesCount: len(out.Items)}
	}
	return out, nil
}
