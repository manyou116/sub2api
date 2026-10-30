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
		if code == "" {
			code = strings.TrimSpace(obj.Get("type").String())
		}
		msg := strings.TrimSpace(obj.Get("error_message").String())
		if msg == "" {
			msg = strings.TrimSpace(obj.Get("message").String())
		}
		knownQuota := code == "codex_quota_exhausted" || code == "model_quota_exhausted" || code == "image_quota_exhausted" || code == "image_gen_quota_exceeded"
		if !knownQuota && !looksLikeRateLimitMessage(code) && !looksLikeRateLimitMessage(msg) {
			continue
		}
		if msg == "" {
			msg = code
		}
		err := NewError(ErrorKindRateLimited, stage, msg, http.StatusTooManyRequests, true)
		err.Code = code
		err.ResetAt = explicitRateLimitReset(obj, time.Now())
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

func explicitRateLimitReset(obj gjson.Result, now time.Time) *time.Time {
	for _, key := range []string{"reset_at", "resets_at"} {
		value := obj.Get(key)
		if !value.Exists() {
			continue
		}
		if reset, err := time.Parse(time.RFC3339, value.String()); err == nil && reset.After(now) {
			return &reset
		}
		if seconds := value.Int(); seconds > now.Unix() {
			reset := time.Unix(seconds, 0)
			return &reset
		}
	}
	if seconds := obj.Get("resets_in_seconds").Int(); seconds > 0 && seconds <= 86400*30 {
		reset := now.Add(time.Duration(seconds) * time.Second)
		return &reset
	}
	return nil
}
