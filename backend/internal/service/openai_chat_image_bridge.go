package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Chat image bridge (fork): map /v1/chat/completions + gpt-image-* onto the existing
// /v1/images/generations pipeline, then wrap the images JSON as a chat.completion.

const (
	ChatImageBridgeStyleMarkdownDataURL = "markdown_data_url"
	ChatImageBridgeStyleMultimodalParts = "multimodal_parts"
)

// IsOpenAIImageGenerationModel reports business image models (gpt-image-*, grok-imagine*).
func IsOpenAIImageGenerationModel(model string) bool {
	return isOpenAIImageGenerationModel(model)
}

// ShouldBridgeChatCompletionsToImages is true when model is an image model (caller also
// checks gateway.openai_chat_image_bridge.enabled).
func ShouldBridgeChatCompletionsToImages(model string) bool {
	return isOpenAIImageGenerationModel(model)
}

// BuildOpenAIImagesBodyFromChatCompletions converts a chat.completions body into an
// images/generations JSON body. Supports string content and multimodal text+image_url parts.
func BuildOpenAIImagesBodyFromChatCompletions(chatBody []byte, model string) ([]byte, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		model = strings.TrimSpace(gjson.GetBytes(chatBody, "model").String())
	}
	if model == "" {
		return nil, fmt.Errorf("model is required")
	}

	prompt, imageURLs, err := extractPromptAndImagesFromChatMessages(chatBody)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("prompt is required (extract text from messages)")
	}

	size := firstNonEmptyString(
		gjson.GetBytes(chatBody, "size").String(),
		gjson.GetBytes(chatBody, "image_size").String(),
	)
	quality := firstNonEmptyString(
		gjson.GetBytes(chatBody, "quality").String(),
		gjson.GetBytes(chatBody, "image_quality").String(),
	)
	n := int(gjson.GetBytes(chatBody, "n").Int())
	if n <= 0 {
		n = 1
	}
	responseFormat := strings.TrimSpace(gjson.GetBytes(chatBody, "response_format").String())
	// Bridge always materializes b64 so we can embed in chat content.
	if responseFormat == "" || responseFormat == "url" {
		responseFormat = "b64_json"
	}

	body := []byte(`{}`)
	body, _ = sjson.SetBytes(body, "model", model)
	body, _ = sjson.SetBytes(body, "prompt", prompt)
	body, _ = sjson.SetBytes(body, "n", n)
	body, _ = sjson.SetBytes(body, "response_format", responseFormat)
	body, _ = sjson.SetBytes(body, "stream", false)
	if size != "" {
		body, _ = sjson.SetBytes(body, "size", size)
	}
	if quality != "" {
		body, _ = sjson.SetBytes(body, "quality", quality)
	}
	for i, url := range imageURLs {
		body, _ = sjson.SetBytes(body, fmt.Sprintf("images.%d.image_url", i), url)
	}
	return body, nil
}

func extractPromptAndImagesFromChatMessages(chatBody []byte) (prompt string, imageURLs []string, err error) {
	messages := gjson.GetBytes(chatBody, "messages")
	if !messages.Exists() || !messages.IsArray() || len(messages.Array()) == 0 {
		// Fallback: top-level prompt (some clients mix APIs).
		if p := strings.TrimSpace(gjson.GetBytes(chatBody, "prompt").String()); p != "" {
			return p, nil, nil
		}
		return "", nil, fmt.Errorf("messages is required")
	}

	// Prefer last user message; if none, last message of any role.
	arr := messages.Array()
	var chosen gjson.Result
	for i := len(arr) - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(arr[i].Get("role").String()), "user") {
			chosen = arr[i]
			break
		}
	}
	if !chosen.Exists() {
		chosen = arr[len(arr)-1]
	}

	content := chosen.Get("content")
	var textParts []string
	switch {
	case content.Type == gjson.String:
		textParts = append(textParts, content.String())
	case content.IsArray():
		for _, part := range content.Array() {
			typ := strings.ToLower(strings.TrimSpace(part.Get("type").String()))
			switch typ {
			case "", "text":
				if t := strings.TrimSpace(part.Get("text").String()); t != "" {
					textParts = append(textParts, t)
				}
			case "image_url":
				url := strings.TrimSpace(part.Get("image_url.url").String())
				if url == "" {
					url = strings.TrimSpace(part.Get("image_url").String())
				}
				if url != "" {
					imageURLs = append(imageURLs, url)
				}
			case "input_image":
				url := strings.TrimSpace(part.Get("image_url").String())
				if url == "" {
					url = strings.TrimSpace(part.Get("image_url.url").String())
				}
				if url != "" {
					imageURLs = append(imageURLs, url)
				}
			}
		}
	default:
		// content missing: try message-level prompt-like fields
		if t := strings.TrimSpace(chosen.Get("text").String()); t != "" {
			textParts = append(textParts, t)
		}
	}

	prompt = strings.TrimSpace(strings.Join(textParts, "\n"))
	return prompt, imageURLs, nil
}

// WrapImagesJSONAsChatCompletion turns an OpenAI images response into chat.completion JSON.
// style: markdown_data_url (default) | multimodal_parts
func WrapImagesJSONAsChatCompletion(imagesJSON []byte, model, style string) ([]byte, error) {
	if !gjson.ValidBytes(imagesJSON) {
		return nil, fmt.Errorf("invalid images response json")
	}
	// Pass through structured errors unchanged (already OpenAI-shaped).
	if gjson.GetBytes(imagesJSON, "error").Exists() && !gjson.GetBytes(imagesJSON, "data").Exists() {
		return imagesJSON, nil
	}

	style = strings.ToLower(strings.TrimSpace(style))
	if style == "" {
		style = ChatImageBridgeStyleMarkdownDataURL
	}

	data := gjson.GetBytes(imagesJSON, "data")
	if !data.Exists() || !data.IsArray() || len(data.Array()) == 0 {
		return nil, fmt.Errorf("images response missing data")
	}

	var content any
	switch style {
	case ChatImageBridgeStyleMultimodalParts:
		parts := make([]map[string]any, 0, len(data.Array()))
		for _, item := range data.Array() {
			url := imageDataURLFromImagesItem(item)
			if url == "" {
				continue
			}
			parts = append(parts, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": url},
			})
		}
		if len(parts) == 0 {
			return nil, fmt.Errorf("images response has no embeddable image data")
		}
		content = parts
	default:
		// markdown_data_url
		var b strings.Builder
		for i, item := range data.Array() {
			url := imageDataURLFromImagesItem(item)
			if url == "" {
				continue
			}
			if i > 0 {
				_ = b.WriteByte('\n')
			}
			_, _ = b.WriteString("![image](")
			_, _ = b.WriteString(url)
			_ = b.WriteByte(')')
		}
		if b.Len() == 0 {
			return nil, fmt.Errorf("images response has no embeddable image data")
		}
		content = b.String()
	}

	if strings.TrimSpace(model) == "" {
		model = "gpt-image-2"
	}
	id := "chatcmpl-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	out := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": content,
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		},
	}
	return json.Marshal(out)
}

func imageDataURLFromImagesItem(item gjson.Result) string {
	if b64 := strings.TrimSpace(item.Get("b64_json").String()); b64 != "" {
		if strings.HasPrefix(b64, "data:") {
			return b64
		}
		return "data:image/png;base64," + b64
	}
	if url := strings.TrimSpace(item.Get("url").String()); url != "" {
		return url
	}
	return ""
}

// NormalizeChatImageBridgeStyle returns a known style or the default.
func NormalizeChatImageBridgeStyle(style string) string {
	switch strings.ToLower(strings.TrimSpace(style)) {
	case ChatImageBridgeStyleMultimodalParts, "multimodal", "parts":
		return ChatImageBridgeStyleMultimodalParts
	default:
		return ChatImageBridgeStyleMarkdownDataURL
	}
}
