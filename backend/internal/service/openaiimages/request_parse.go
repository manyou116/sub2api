package openaiimages

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// MaxImageBytes 限制单张输入图片尺寸（10 MiB），与 OpenAI 官方 /v1/images/edits 对齐。
const MaxImageBytes = 10 << 20

// MaxImagesPerRequest 单请求最多接受的输入图片数。
const MaxImagesPerRequest = 16

// ErrUnsupportedModel 表示请求里的 model 不在图片能力表内。
var ErrUnsupportedModel = errors.New("openaiimages: model not in image capability table")

// PeekModel 仅解析请求 body 顶层的 "model" 字段，用于 chat/responses 入口
// 在不完整解析的前提下快速判断是否要分流到图片网关。
//
// 失败（非 JSON / 字段缺失）一律返回 ""，调用方应视为"非图片请求"。
func PeekModel(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var raw struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return ""
	}
	return strings.TrimSpace(raw.Model)
}

// ParseImagesGenerations 解析 POST /v1/images/generations 的 JSON body。
func ParseImagesGenerations(body []byte) (*ImagesRequest, error) {
	var raw struct {
		Model          string `json:"model"`
		Prompt         string `json:"prompt"`
		N              *int   `json:"n"`
		Size           string `json:"size"`
		Quality        string `json:"quality"`
		Style          string `json:"style"`
		Background     string `json:"background"`
		ResponseFormat string `json:"response_format"`
		User           string `json:"user"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	if strings.TrimSpace(raw.Prompt) == "" {
		return nil, errors.New("prompt is required")
	}
	req := &ImagesRequest{
		Entry:          EntryImagesGenerations,
		Model:          raw.Model,
		Prompt:         strings.TrimSpace(raw.Prompt),
		N:              valueOr(raw.N, 1),
		Size:           raw.Size,
		Quality:        raw.Quality,
		Style:          raw.Style,
		Background:     raw.Background,
		ResponseFormat:         parseResponseFormat(raw.ResponseFormat, ResponseFormatB64JSON),
		ResponseFormatExplicit: strings.TrimSpace(raw.ResponseFormat) != "",
		User:                   raw.User,
		StartedAt:              time.Now(),
	}
	if err := validateCommon(req); err != nil {
		return nil, err
	}
	return req, nil
}

// ParseImagesEdits 解析 POST /v1/images/edits 的 multipart/form-data 请求。
//
// 字段约定（与 OpenAI 官方对齐 + chatgpt2api 扩展）：
//
//	image:           主图（必需）；可重复出现多张作为参考集
//	mask:            遮罩图（可选，仅 dall-e-2 实际使用）
//	prompt:          文本指令（必需）
//	n / size / quality / style / response_format / model / user / background
func ParseImagesEdits(r *http.Request) (*ImagesRequest, error) {
	if err := r.ParseMultipartForm(MaxImageBytes * (MaxImagesPerRequest + 1)); err != nil {
		return nil, fmt.Errorf("parse multipart: %w", err)
	}
	form := r.MultipartForm
	if form == nil {
		return nil, errors.New("missing multipart form")
	}

	prompt := strings.TrimSpace(formFirst(form, "prompt"))
	if prompt == "" {
		return nil, errors.New("prompt is required")
	}

	images, err := readMultipartImages(form, "image")
	if err != nil {
		return nil, err
	}
	// 兼容客户端写法：image[] (Cherry Studio / 多图惯例) 与 image_0/image_1/... 序号字段
	if extra, err := readMultipartImages(form, "image[]"); err != nil {
		return nil, err
	} else if len(extra) > 0 {
		images = append(images, extra...)
	}
	for k := range form.File {
		if k == "image" || k == "image[]" || k == "mask" {
			continue
		}
		if !strings.HasPrefix(k, "image[") && !strings.HasPrefix(k, "image_") {
			continue
		}
		extra, err := readMultipartImages(form, k)
		if err != nil {
			return nil, err
		}
		images = append(images, extra...)
	}
	if len(images) == 0 {
		return nil, errors.New("at least one image file is required")
	}

	if mask, err := readMultipartImages(form, "mask"); err != nil {
		return nil, err
	} else if len(mask) > 0 {
		images = append(images, mask...)
	}

	n := 1
	if v := formFirst(form, "n"); v != "" {
		if parsed, perr := strconv.Atoi(v); perr == nil && parsed > 0 {
			n = parsed
		}
	}

	req := &ImagesRequest{
		Entry:          EntryImagesEdits,
		Model:          formFirst(form, "model"),
		Prompt:         prompt,
		N:              n,
		Size:           formFirst(form, "size"),
		Quality:        formFirst(form, "quality"),
		Style:          formFirst(form, "style"),
		Background:     formFirst(form, "background"),
		ResponseFormat:         parseResponseFormat(formFirst(form, "response_format"), ResponseFormatB64JSON),
		ResponseFormatExplicit: strings.TrimSpace(formFirst(form, "response_format")) != "",
		User:                   formFirst(form, "user"),
		Images:                 images,
		StartedAt:              time.Now(),
	}
	if err := validateCommon(req); err != nil {
		return nil, err
	}
	return req, nil
}

// ParseFromChatCompletions 把 chat/completions 请求体（model 命中图片别名时）
// 折叠成 ImagesRequest。规则：
//   - 把最后一条 role=user 的 message 抽取 text 作为 prompt
//   - 该 message 里所有 image_url 形式的 part 转 SourceImage（图生图）
//   - stream 字段透传
//   - response_format 默认 markdown
func ParseFromChatCompletions(body []byte) (*ImagesRequest, error) {
	var raw struct {
		Model    string            `json:"model"`
		Messages []json.RawMessage `json:"messages"`
		Stream   bool              `json:"stream"`
		N        *int              `json:"n"`
		User     string            `json:"user"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	if len(raw.Messages) == 0 {
		return nil, errors.New("messages is required")
	}

	prompt, images, err := extractPromptAndImagesFromMessages(raw.Messages)
	if err != nil {
		return nil, err
	}
	if prompt == "" && len(images) == 0 {
		return nil, errors.New("could not extract prompt from messages")
	}

	req := &ImagesRequest{
		Entry:          EntryChatCompletions,
		Model:          raw.Model,
		Prompt:         prompt,
		N:              valueOr(raw.N, 1),
		ResponseFormat: ResponseFormatMarkdown,
		Stream:         raw.Stream,
		User:           raw.User,
		Images:         images,
		StartedAt:      time.Now(),
	}
	if err := validateCommon(req); err != nil {
		return nil, err
	}
	return req, nil
}

// ParseFromResponses 把 /v1/responses body 折叠成 ImagesRequest。
// input 字段可能是字符串、消息数组、或 ChatGPT desktop 形态的 input_text/input_image 数组。
func ParseFromResponses(body []byte) (*ImagesRequest, error) {
	var raw struct {
		Model        string          `json:"model"`
		Input        json.RawMessage `json:"input"`
		Instructions string          `json:"instructions"`
		Stream       bool            `json:"stream"`
		User         string          `json:"user"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}

	prompt, images, err := extractPromptAndImagesFromResponsesInput(raw.Input)
	if err != nil {
		return nil, err
	}
	if instr := strings.TrimSpace(raw.Instructions); instr != "" {
		if prompt != "" {
			prompt = instr + "\n\n" + prompt
		} else {
			prompt = instr
		}
	}
	if prompt == "" && len(images) == 0 {
		return nil, errors.New("could not extract prompt from input")
	}

	req := &ImagesRequest{
		Entry:          EntryResponses,
		Model:          raw.Model,
		Prompt:         prompt,
		N:              1,
		ResponseFormat: ResponseFormatMarkdown,
		Stream:         raw.Stream,
		User:           raw.User,
		Images:         images,
		StartedAt:      time.Now(),
	}
	if err := validateCommon(req); err != nil {
		return nil, err
	}
	return req, nil
}

// ─── helpers ────────────────────────────────────────────────────────────────

func parseResponseFormat(raw string, fallback ResponseFormat) ResponseFormat {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "b64_json":
		return ResponseFormatB64JSON
	case "url":
		return ResponseFormatURL
	case "markdown":
		return ResponseFormatMarkdown
	}
	return fallback
}

func validateCommon(req *ImagesRequest) error {
	if req.N <= 0 {
		req.N = 1
	}
	if req.N > 10 {
		return fmt.Errorf("n must be in [1, 10], got %d", req.N)
	}
	if len(req.Images) > MaxImagesPerRequest {
		return fmt.Errorf("too many input images (max %d)", MaxImagesPerRequest)
	}
	return nil
}

func valueOr(p *int, fallback int) int {
	if p != nil && *p > 0 {
		return *p
	}
	return fallback
}

func formFirst(form *multipart.Form, key string) string {
	if form == nil {
		return ""
	}
	if vs := form.Value[key]; len(vs) > 0 {
		return vs[0]
	}
	return ""
}

func readMultipartImages(form *multipart.Form, key string) ([]SourceImage, error) {
	if form == nil {
		return nil, nil
	}
	files := form.File[key]
	out := make([]SourceImage, 0, len(files))
	for _, fh := range files {
		if fh.Size > MaxImageBytes {
			return nil, fmt.Errorf("image %q exceeds %d bytes", fh.Filename, MaxImageBytes)
		}
		f, err := fh.Open()
		if err != nil {
			return nil, fmt.Errorf("open %q: %w", fh.Filename, err)
		}
		buf, rerr := io.ReadAll(io.LimitReader(f, MaxImageBytes+1))
		_ = f.Close()
		if rerr != nil {
			return nil, fmt.Errorf("read %q: %w", fh.Filename, rerr)
		}
		if len(buf) > MaxImageBytes {
			return nil, fmt.Errorf("image %q exceeds %d bytes", fh.Filename, MaxImageBytes)
		}
		ct := fh.Header.Get("Content-Type")
		if ct == "" {
			ct = http.DetectContentType(buf)
		}
		out = append(out, SourceImage{Filename: fh.Filename, ContentType: ct, Data: buf})
	}
	return out, nil
}

func extractPromptAndImagesFromMessages(msgs []json.RawMessage) (string, []SourceImage, error) {
	// 倒序找最后一条 user message。
	for i := len(msgs) - 1; i >= 0; i-- {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(msgs[i], &m); err != nil {
			continue
		}
		if !strings.EqualFold(m.Role, "user") {
			continue
		}
		return parseChatContent(m.Content)
	}
	return "", nil, errors.New("no user message found")
}

func parseChatContent(raw json.RawMessage) (string, []SourceImage, error) {
	if len(raw) == 0 {
		return "", nil, nil
	}
	// 字符串形态
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s), nil, nil
	}
	// 数组形态
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil, fmt.Errorf("unsupported content shape: %w", err)
	}
	var (
		textBuf strings.Builder
		images  []SourceImage
	)
	for _, p := range parts {
		switch strings.ToLower(p.Type) {
		case "text", "input_text":
			if t := strings.TrimSpace(p.Text); t != "" {
				if textBuf.Len() > 0 {
					textBuf.WriteString("\n")
				}
				textBuf.WriteString(t)
			}
		case "image_url", "input_image":
			img, err := decodeDataURLOrSkip(p.ImageURL.URL)
			if err != nil {
				return "", nil, err
			}
			if img != nil {
				images = append(images, *img)
			}
		}
	}
	return textBuf.String(), images, nil
}

func extractPromptAndImagesFromResponsesInput(raw json.RawMessage) (string, []SourceImage, error) {
	if len(raw) == 0 {
		return "", nil, nil
	}
	// 字符串
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s), nil, nil
	}
	// 消息数组（与 chat 共用结构）
	var msgs []json.RawMessage
	if err := json.Unmarshal(raw, &msgs); err == nil && len(msgs) > 0 {
		// 先尝试当成 chat 风格 messages
		if first := firstByteNonSpace(msgs[0]); first == '{' {
			if prompt, imgs, err := extractPromptAndImagesFromMessages(msgs); err == nil {
				return prompt, imgs, nil
			}
		}
		// 否则当 part 数组
		merged, _ := json.Marshal(msgs)
		return parseChatContent(merged)
	}
	// 单个 part 对象
	return parseChatContent(raw)
}

func firstByteNonSpace(b []byte) byte {
	for _, c := range b {
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			return c
		}
	}
	return 0
}

// decodeDataURLOrSkip 解析 data:image/...;base64,... ；http(s) 链接暂不下载（返回 nil）。
func decodeDataURLOrSkip(url string) (*SourceImage, error) {
	url = strings.TrimSpace(url)
	if !strings.HasPrefix(url, "data:") {
		return nil, nil // 远程 URL 留给 driver 自行处理；此处不阻塞 parse
	}
	semi := strings.IndexByte(url, ';')
	comma := strings.IndexByte(url, ',')
	if semi < 0 || comma < 0 || comma < semi {
		return nil, errors.New("malformed data URL")
	}
	mime := url[5:semi]
	enc := url[semi+1 : comma]
	payload := url[comma+1:]
	var data []byte
	switch strings.ToLower(enc) {
	case "base64":
		raw, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return nil, fmt.Errorf("decode data URL base64: %w", err)
		}
		data = raw
	default:
		return nil, fmt.Errorf("unsupported data URL encoding: %s", enc)
	}
	if len(data) > MaxImageBytes {
		return nil, fmt.Errorf("data URL image exceeds %d bytes", MaxImageBytes)
	}
	return &SourceImage{Filename: "image", ContentType: mime, Data: data}, nil
}
