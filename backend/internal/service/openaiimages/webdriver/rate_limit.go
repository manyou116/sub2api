package webdriver

import (
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// Parse explicit errors before trimming messages; never scan user prompts for quota keywords.
func extractTaskRateLimitError(stage string, body []byte) *Error {
	for _, path := range []string{
		"v.message.metadata.error_metadata", "message.metadata.error_metadata",
		"metadata.error_metadata", "error", "items.0.error", "0.error",
	} {
		obj := gjson.GetBytes(body, path)
		code := strings.TrimSpace(obj.Get("error_code").String())
		if code == "" {
			code = strings.TrimSpace(obj.Get("code").String())
		}
		msg := strings.TrimSpace(obj.Get("error_message").String())
		if msg == "" {
			msg = strings.TrimSpace(obj.Get("message").String())
		}
		if !looksLikeRateLimitMessage(code) && !looksLikeRateLimitMessage(msg) {
			continue
		}
		if msg == "" {
			msg = code
		}
		err := NewError(ErrorKindRateLimited, stage, msg, http.StatusTooManyRequests, true)
		err.Code = code
		if reset, e := time.Parse(time.RFC3339, obj.Get("reset_at").String()); e == nil && reset.After(time.Now()) {
			err.ResetAt = &reset
		}
		return err
	}
	// A conversation response stores error metadata on assistant/tool nodes.
	var found *Error
	gjson.GetBytes(body, "mapping").ForEach(func(_, node gjson.Result) bool {
		role := node.Get("message.author.role").String()
		if role == "assistant" || role == "tool" {
			found = extractTaskRateLimitError(stage, []byte(node.Raw))
		}
		return found == nil
	})
	if found != nil {
		return found
	}
	for _, path := range []string{"v.message", "message"} {
		msg := gjson.GetBytes(body, path)
		role := msg.Get("author.role").String()
		if role != "assistant" && role != "tool" {
			continue
		}
		var texts []string
		for _, part := range msg.Get("content.parts").Array() {
			if part.Type == gjson.String {
				texts = append(texts, part.String())
			}
		}
		if text := strings.Join(texts, "\n"); looksLikeRateLimitMessage(text) {
			return NewError(ErrorKindRateLimited, stage, text, http.StatusTooManyRequests, true)
		}
	}
	for _, path := range []string{"detail", "message", "error"} {
		value := gjson.GetBytes(body, path)
		if value.Type == gjson.String && looksLikeRateLimitMessage(value.String()) {
			return NewError(ErrorKindRateLimited, stage, value.String(), http.StatusTooManyRequests, true)
		}
	}
	msg := scanConversationAssistantText(body, looksLikeRateLimitMessage, conversationTextScanOptions{
		includeTool: true, skipInternalDefinitions: true,
	})
	if msg != "" {
		return NewError(ErrorKindRateLimited, stage, msg, http.StatusTooManyRequests, true)
	}
	return nil
}
