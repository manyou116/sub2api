package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const openAIImagesMarkdownContentType = "text/markdown; charset=utf-8"

type openAIImageMarkdownItem struct {
	Src string
	Alt string
}

func wantsOpenAIImagesMarkdownResponse(c *gin.Context, responseFormat string) bool {
	if isOpenAIImagesMarkdownFormat(responseFormat) {
		return true
	}
	if c == nil {
		return false
	}
	if isOpenAIImagesMarkdownFormat(c.GetHeader("X-Sub2API-Image-Response-Format")) {
		return true
	}
	if isOpenAIImagesMarkdownFormat(c.Query("response_format")) || isOpenAIImagesMarkdownFormat(c.Query("format")) {
		return true
	}
	return strings.Contains(strings.ToLower(c.GetHeader("Accept")), "text/markdown")
}

func isOpenAIImagesMarkdownFormat(format string) bool {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "markdown", "md", "text/markdown":
		return true
	default:
		return false
	}
}

func normalizeOpenAIImagesUpstreamResponseFormat(format string) string {
	if isOpenAIImagesMarkdownFormat(format) {
		return "b64_json"
	}
	return strings.ToLower(strings.TrimSpace(format))
}

func openAIImageDataURI(b64 string, outputFormat string) string {
	b64 = strings.TrimSpace(b64)
	if b64 == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(b64), "data:") {
		return b64
	}
	return "data:" + openAIImageOutputMIMEType(outputFormat) + ";base64," + b64
}

func buildOpenAIImagesMarkdown(items []openAIImageMarkdownItem) []byte {
	var buf bytes.Buffer
	for _, item := range items {
		src := sanitizeOpenAIImageMarkdownSrc(item.Src)
		if src == "" {
			continue
		}
		if buf.Len() > 0 {
			_, _ = buf.WriteString("\n\n")
		}
		_, _ = buf.WriteString("![")
		_, _ = buf.WriteString(sanitizeOpenAIImageMarkdownAlt(item.Alt))
		_, _ = buf.WriteString("](")
		_, _ = buf.WriteString(src)
		_, _ = buf.WriteString(")")
	}
	if buf.Len() == 0 {
		return nil
	}
	_ = buf.WriteByte('\n')
	return buf.Bytes()
}

func sanitizeOpenAIImageMarkdownSrc(src string) string {
	src = strings.TrimSpace(src)
	src = strings.ReplaceAll(src, "\r", "")
	src = strings.ReplaceAll(src, "\n", "")
	src = strings.ReplaceAll(src, " ", "%20")
	src = strings.ReplaceAll(src, ")", "%29")
	return src
}

func sanitizeOpenAIImageMarkdownAlt(alt string) string {
	alt = strings.TrimSpace(alt)
	if alt == "" {
		return "image"
	}
	alt = strings.ReplaceAll(alt, "\r", " ")
	alt = strings.ReplaceAll(alt, "\n", " ")
	alt = strings.ReplaceAll(alt, "]", "\\]")
	return alt
}

func buildOpenAIImagesMarkdownFromResponsesResults(results []openAIResponsesImageResult) ([]byte, bool) {
	items := make([]openAIImageMarkdownItem, 0, len(results))
	for _, img := range results {
		src := openAIImageDataURI(img.Result, img.OutputFormat)
		if src == "" {
			continue
		}
		items = append(items, openAIImageMarkdownItem{
			Src: src,
			Alt: img.RevisedPrompt,
		})
	}
	body := buildOpenAIImagesMarkdown(items)
	return body, len(body) > 0
}

func buildOpenAIImagesMarkdownFromAPIResponse(body []byte) ([]byte, int, bool) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return nil, 0, false
	}
	data := gjson.GetBytes(body, "data")
	if !data.IsArray() {
		return nil, 0, false
	}
	rootOutputFormat := gjson.GetBytes(body, "output_format").String()
	items := make([]openAIImageMarkdownItem, 0, len(data.Array()))
	for _, entry := range data.Array() {
		src := strings.TrimSpace(entry.Get("url").String())
		if src == "" {
			outputFormat := entry.Get("output_format").String()
			if strings.TrimSpace(outputFormat) == "" {
				outputFormat = rootOutputFormat
			}
			src = openAIImageDataURI(entry.Get("b64_json").String(), outputFormat)
		}
		if src == "" {
			continue
		}
		items = append(items, openAIImageMarkdownItem{
			Src: src,
			Alt: entry.Get("revised_prompt").String(),
		})
	}
	markdown := buildOpenAIImagesMarkdown(items)
	return markdown, len(items), len(markdown) > 0
}

func appendOpenAIImagesMarkdownToStreamPayload(payload []byte) []byte {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}
	if strings.TrimSpace(gjson.GetBytes(payload, "markdown").String()) != "" {
		return payload
	}

	b64 := gjson.GetBytes(payload, "b64_json").String()
	if strings.TrimSpace(b64) == "" {
		b64 = gjson.GetBytes(payload, "partial_image_b64").String()
	}
	if strings.TrimSpace(b64) == "" {
		return payload
	}
	outputFormat := gjson.GetBytes(payload, "output_format").String()
	alt := gjson.GetBytes(payload, "revised_prompt").String()
	markdown := buildOpenAIImagesMarkdown([]openAIImageMarkdownItem{{
		Src: openAIImageDataURI(b64, outputFormat),
		Alt: alt,
	}})
	if len(markdown) == 0 {
		return payload
	}
	rewritten, err := sjson.SetBytes(payload, "markdown", strings.TrimSpace(string(markdown)))
	if err != nil {
		return payload
	}
	return rewritten
}

func BuildOpenAIImageCompatRequestBodyFromChatCompletions(body []byte) ([]byte, bool, error) {
	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if !isOpenAIImageGenerationModel(model) {
		return nil, false, nil
	}
	prompt := extractOpenAIImagePromptFromChatCompletions(body)
	if prompt == "" {
		return nil, true, fmt.Errorf("chat completions image generation requires a user text prompt")
	}
	compatBody, err := buildOpenAIImageCompatRequestBody(body, prompt)
	return compatBody, true, err
}

func BuildOpenAIImageCompatRequestBodyFromResponses(body []byte) ([]byte, bool, error) {
	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if !isOpenAIImageGenerationModel(model) {
		return nil, false, nil
	}
	prompt := extractOpenAIImagePromptFromResponses(body)
	if prompt == "" {
		return nil, true, fmt.Errorf("responses image generation requires input text")
	}
	compatBody, err := buildOpenAIImageCompatRequestBody(body, prompt)
	return compatBody, true, err
}

func BuildOpenAIImagesMarkdownChatCompletionsResponse(markdown []byte, model string, usage OpenAIUsage) (*apicompat.ChatCompletionsResponse, error) {
	content, err := json.Marshal(string(markdown))
	if err != nil {
		return nil, fmt.Errorf("marshal markdown chat content: %w", err)
	}

	resp := &apicompat.ChatCompletionsResponse{
		ID:      "chatcmpl-" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   strings.TrimSpace(model),
		Choices: []apicompat.ChatChoice{{
			Index: 0,
			Message: apicompat.ChatMessage{
				Role:    "assistant",
				Content: content,
			},
			FinishReason: "stop",
		}},
	}
	if chatUsage := buildOpenAIImagesMarkdownChatUsage(usage); chatUsage != nil {
		resp.Usage = chatUsage
	}
	return resp, nil
}

func BuildOpenAIImagesMarkdownResponsesResponse(markdown []byte, model string, usage OpenAIUsage) *apicompat.ResponsesResponse {
	resp := &apicompat.ResponsesResponse{
		ID:     "resp_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		Object: "response",
		Model:  strings.TrimSpace(model),
		Status: "completed",
		Output: []apicompat.ResponsesOutput{{
			Type:   "message",
			ID:     "msg_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
			Role:   "assistant",
			Status: "completed",
			Content: []apicompat.ResponsesContentPart{{
				Type: "output_text",
				Text: string(markdown),
			}},
		}},
	}
	if responsesUsage := buildOpenAIImagesMarkdownResponsesUsage(usage); responsesUsage != nil {
		resp.Usage = responsesUsage
	}
	return resp
}

func buildOpenAIImageCompatRequestBody(body []byte, prompt string) ([]byte, error) {
	out := []byte(`{}`)
	var err error

	for field, paths := range map[string][]string{
		"model":              {"model"},
		"size":               {"size", "tools.0.size"},
		"quality":            {"quality", "tools.0.quality"},
		"background":         {"background", "tools.0.background"},
		"output_format":      {"output_format", "tools.0.output_format"},
		"moderation":         {"moderation", "tools.0.moderation"},
		"style":              {"style", "tools.0.style"},
		"n":                  {"n", "tools.0.n"},
		"output_compression": {"output_compression", "tools.0.output_compression"},
		"partial_images":     {"partial_images", "tools.0.partial_images"},
		"prompt_cache_key":   {"prompt_cache_key"},
	} {
		if raw, ok := firstOpenAIImageCompatRaw(body, paths...); ok {
			out, err = sjson.SetRawBytes(out, field, []byte(raw))
			if err != nil {
				return nil, fmt.Errorf("set %s: %w", field, err)
			}
		}
	}

	out, err = sjson.SetBytes(out, "prompt", strings.TrimSpace(prompt))
	if err != nil {
		return nil, fmt.Errorf("set prompt: %w", err)
	}
	out, err = sjson.SetBytes(out, "response_format", "markdown")
	if err != nil {
		return nil, fmt.Errorf("set response_format: %w", err)
	}
	return out, nil
}

func firstOpenAIImageCompatRaw(body []byte, paths ...string) (string, bool) {
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		value := gjson.GetBytes(body, path)
		if value.Exists() && strings.TrimSpace(value.Raw) != "" {
			return value.Raw, true
		}
	}
	return "", false
}

func extractOpenAIImagePromptFromChatCompletions(body []byte) string {
	if prompt := strings.TrimSpace(gjson.GetBytes(body, "prompt").String()); prompt != "" {
		return prompt
	}
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return ""
	}
	items := messages.Array()
	for idx := len(items) - 1; idx >= 0; idx-- {
		item := items[idx]
		if role := strings.TrimSpace(item.Get("role").String()); role != "" && role != "user" {
			continue
		}
		if prompt := extractOpenAIImagePromptFromContent(item.Get("content")); prompt != "" {
			return prompt
		}
	}
	return ""
}

func extractOpenAIImagePromptFromResponses(body []byte) string {
	if prompt := strings.TrimSpace(gjson.GetBytes(body, "prompt").String()); prompt != "" {
		return prompt
	}
	input := gjson.GetBytes(body, "input")
	switch {
	case input.Type == gjson.String:
		return strings.TrimSpace(input.String())
	case input.IsArray():
		items := input.Array()
		for idx := len(items) - 1; idx >= 0; idx-- {
			item := items[idx]
			if role := strings.TrimSpace(item.Get("role").String()); role != "" && role != "user" {
				continue
			}
			if prompt := extractOpenAIImagePromptFromContent(item.Get("content")); prompt != "" {
				return prompt
			}
			if prompt := strings.TrimSpace(item.String()); prompt != "" && item.Type == gjson.String {
				return prompt
			}
		}
	case input.Exists():
		if prompt := extractOpenAIImagePromptFromContent(input.Get("content")); prompt != "" {
			return prompt
		}
	}
	return strings.TrimSpace(gjson.GetBytes(body, "instructions").String())
}

func extractOpenAIImagePromptFromContent(content gjson.Result) string {
	if !content.Exists() {
		return ""
	}
	if content.Type == gjson.String {
		return strings.TrimSpace(content.String())
	}
	if content.IsArray() {
		parts := make([]string, 0, len(content.Array()))
		for _, part := range content.Array() {
			partType := strings.TrimSpace(part.Get("type").String())
			if partType != "" &&
				partType != "text" &&
				partType != "input_text" &&
				partType != "output_text" {
				continue
			}
			if text := strings.TrimSpace(firstNonEmptyString(part.Get("text").String(), part.String())); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	}
	if text := strings.TrimSpace(content.Get("text").String()); text != "" {
		return text
	}
	return ""
}

func buildOpenAIImagesMarkdownChatUsage(usage OpenAIUsage) *apicompat.ChatUsage {
	if usage.InputTokens == 0 && usage.OutputTokens == 0 {
		return nil
	}
	result := &apicompat.ChatUsage{
		PromptTokens:     usage.InputTokens,
		CompletionTokens: usage.OutputTokens,
		TotalTokens:      usage.InputTokens + usage.OutputTokens,
	}
	if usage.CacheReadInputTokens > 0 {
		result.PromptTokensDetails = &apicompat.ChatTokenDetails{
			CachedTokens: usage.CacheReadInputTokens,
		}
	}
	return result
}

func buildOpenAIImagesMarkdownResponsesUsage(usage OpenAIUsage) *apicompat.ResponsesUsage {
	if usage.InputTokens == 0 && usage.OutputTokens == 0 {
		return nil
	}
	result := &apicompat.ResponsesUsage{
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		TotalTokens:  usage.InputTokens + usage.OutputTokens,
	}
	if usage.CacheReadInputTokens > 0 {
		result.InputTokensDetails = &apicompat.ResponsesInputTokensDetails{
			CachedTokens: usage.CacheReadInputTokens,
		}
	}
	return result
}
