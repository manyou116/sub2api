package openaiimages

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/imroc/req/v3"
)

// APIKeyDriver 直连 https://api.openai.com/v1/images/{generations,edits} 的 driver。
//
// 适用账号：accounts.type == "api_key"（即 sk-xxx）。
// 走标准 OpenAI Images 协议，无需 PoW / sentinel / SSE。
//
// 出错分类：
//   - 401/403 → AuthError（语义同 webdriver.AuthError，由调用方剥换号）
//   - 429    → RateLimitError，ResetAfter 从 Retry-After 头解析
//   - 5xx    → TransportError（可重试）
//   - 其他 4xx → 透传 OpenAI 错误体（封装为 *UpstreamError）
type APIKeyDriver struct {
	BaseURL string         // 默认 https://api.openai.com
	Client  *req.Client    // 默认 req.C().SetTimeout(180s)
	Now     func() time.Time
}

// NewAPIKeyDriver 创建带默认配置的实例。
func NewAPIKeyDriver() *APIKeyDriver {
	return &APIKeyDriver{
		BaseURL: "https://api.openai.com",
		Client:  req.C().SetTimeout(180 * time.Second),
		Now:     time.Now,
	}
}

func (d *APIKeyDriver) Name() string { return "apikey" }

func (d *APIKeyDriver) baseURL() string {
	if d.BaseURL != "" {
		return d.BaseURL
	}
	return "https://api.openai.com"
}

func (d *APIKeyDriver) httpClient() *req.Client {
	if d.Client != nil {
		return d.Client
	}
	d.Client = req.C().SetTimeout(180 * time.Second)
	return d.Client
}

func (d *APIKeyDriver) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Forward 实现 Driver。
func (d *APIKeyDriver) Forward(ctx context.Context, account AccountView, request *ImagesRequest) (*ImageResult, error) {
	apiKey := account.APIKey()
	if apiKey == "" {
		return nil, &AuthError{Reason: "api key missing"}
	}
	client := d.httpClient()
	if proxy := account.ProxyURL(); proxy != "" {
		client = client.Clone().SetProxyURL(proxy)
	}

	var (
		resp *req.Response
		err  error
	)
	if request.Entry == EntryImagesEdits || len(request.Images) > 0 {
		resp, err = d.doEdits(ctx, client, apiKey, request)
	} else {
		resp, err = d.doGenerations(ctx, client, apiKey, request)
	}
	if err != nil {
		return nil, &TransportError{Reason: err.Error()}
	}
	return d.parseResponse(resp, request)
}

func (d *APIKeyDriver) doGenerations(ctx context.Context, client *req.Client, apiKey string, request *ImagesRequest) (*req.Response, error) {
	body := map[string]any{
		"model":  upstreamModel(request.Model),
		"prompt": request.Prompt,
	}
	if request.N > 0 {
		body["n"] = request.N
	}
	if request.Size != "" {
		body["size"] = request.Size
	}
	if request.Quality != "" {
		body["quality"] = request.Quality
	}
	if request.Style != "" {
		body["style"] = request.Style
	}
	if request.Background != "" {
		body["background"] = request.Background
	}
	if request.User != "" {
		body["user"] = request.User
	}
	// API-Key 直连默认 b64_json 让上层统一处理；用户显式指定 url 时透传
	if request.ResponseFormat == ResponseFormatURL {
		body["response_format"] = "url"
	} else {
		body["response_format"] = "b64_json"
	}
	for k, v := range request.Extras {
		if _, exists := body[k]; !exists {
			body[k] = v
		}
	}

	return client.R().SetContext(ctx).
		SetHeader("authorization", "Bearer "+apiKey).
		SetHeader("content-type", "application/json").
		SetBodyJsonMarshal(body).
		Post(d.baseURL() + "/v1/images/generations")
}

func (d *APIKeyDriver) doEdits(ctx context.Context, client *req.Client, apiKey string, request *ImagesRequest) (*req.Response, error) {
	if len(request.Images) == 0 {
		return nil, fmt.Errorf("apikey driver: edits requires at least one image")
	}
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	if err := writeFormField(mw, "model", upstreamModel(request.Model)); err != nil {
		return nil, err
	}
	if err := writeFormField(mw, "prompt", request.Prompt); err != nil {
		return nil, err
	}
	if request.N > 0 {
		_ = writeFormField(mw, "n", strconv.Itoa(request.N))
	}
	if request.Size != "" {
		_ = writeFormField(mw, "size", request.Size)
	}
	if request.Quality != "" {
		_ = writeFormField(mw, "quality", request.Quality)
	}
	if request.User != "" {
		_ = writeFormField(mw, "user", request.User)
	}
	if request.ResponseFormat == ResponseFormatURL {
		_ = writeFormField(mw, "response_format", "url")
	} else {
		_ = writeFormField(mw, "response_format", "b64_json")
	}
	// 多张：第一张作为 image[]，第二张作为 mask；OpenAI edits 协议规定 mask 单字段。
	if err := writeImageField(mw, "image", request.Images[0]); err != nil {
		return nil, err
	}
	if len(request.Images) >= 2 {
		if err := writeImageField(mw, "mask", request.Images[1]); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	return client.R().SetContext(ctx).
		SetHeader("authorization", "Bearer "+apiKey).
		SetHeader("content-type", mw.FormDataContentType()).
		SetBody(buf.Bytes()).
		Post(d.baseURL() + "/v1/images/edits")
}

func writeFormField(mw *multipart.Writer, name, value string) error {
	w, err := mw.CreateFormField(name)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, value)
	return err
}

func writeImageField(mw *multipart.Writer, fieldName string, img SourceImage) error {
	filename := img.Filename
	if filename == "" {
		filename = fieldName + ".png"
	}
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{fmt.Sprintf(`form-data; name=%q; filename=%q`, fieldName, filename)}
	if img.ContentType != "" {
		h["Content-Type"] = []string{img.ContentType}
	} else {
		h["Content-Type"] = []string{"image/png"}
	}
	w, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	_, err = w.Write(img.Data)
	return err
}

// upstreamModel 把内部别名映射到上游真实模型名。
//
// 默认映射：客户端的图片模型别名（gpt-image-2 / codex-gpt-image-2 / auto / gpt-5* 等）
// 在 API-Key 通道一律以 OpenAI 官方 "gpt-image-1" 上传；webdriver 则使用各自 slug。
func upstreamModel(alias string) string {
	switch strings.ToLower(strings.TrimSpace(alias)) {
	case "", "auto", "gpt-image-2", "codex-gpt-image-2":
		return "gpt-image-1"
	case "dall-e-3":
		return "dall-e-3"
	case "dall-e-2":
		return "dall-e-2"
	default:
		return alias
	}
}

func (d *APIKeyDriver) parseResponse(resp *req.Response, request *ImagesRequest) (*ImageResult, error) {
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
		Created int64 `json:"created"`
		Data    []struct {
			B64JSON       string `json:"b64_json"`
			URL           string `json:"url"`
			RevisedPrompt string `json:"revised_prompt"`
		} `json:"data"`
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, &UpstreamError{HTTPStatus: resp.StatusCode, Body: body, Reason: "unparseable JSON: " + err.Error()}
	}
	if len(payload.Data) == 0 {
		return nil, &UpstreamError{HTTPStatus: resp.StatusCode, Body: body, Reason: "no images returned"}
	}
	out := &ImageResult{
		Model:   coalesceStr(payload.Model, request.Model),
		Created: coalesceInt64(payload.Created, d.now().Unix()),
	}
	for _, item := range payload.Data {
		out.Items = append(out.Items, ImageItem{
			B64JSON:       item.B64JSON,
			URL:           item.URL,
			RevisedPrompt: item.RevisedPrompt,
			MimeType:      "image/png",
		})
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

// extractAPIError 尝试从 OpenAI 错误体提取 message；否则返回原始片段。
func extractAPIError(body []byte) string {
	var p struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &p); err == nil && p.Error.Message != "" {
		return p.Error.Message
	}
	if len(body) > 200 {
		return string(body[:200])
	}
	return string(body)
}

// parseRetryAfter 解析 Retry-After 头（秒数 或 HTTP 日期）。
func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if secs, err := strconv.Atoi(value); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(value); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func coalesceStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func coalesceInt64(a, b int64) int64 {
	if a != 0 {
		return a
	}
	return b
}
