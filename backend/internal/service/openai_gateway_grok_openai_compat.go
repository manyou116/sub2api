package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// OpenAI-client (new-api / Codex / SDKs) → xAI Grok request normalization.
// Kept in a fork-only file so upstream merges of openai_gateway_grok.go conflict less.
//
// Regression note: after preferring upstream Grok sanitizers in v0.1.155 merge,
// presence_penalty/presencePenalty stopped being stripped on chat/completions
// (bridge eligibility rejects unknown fields → raw path → xAI 400).

var (
	grokDropPenaltyFields = []string{
		"presence_penalty", "presencePenalty",
		"frequency_penalty", "frequencyPenalty",
	}
	grokDropChatNoiseFields = []string{
		"user", "seed", "n", "logit_bias", "logprobs", "top_logprobs",
		"service_tier", "store", "metadata", "modalities", "audio",
		"prediction", "web_search_options", "prompt_cache_retention",
		"safety_identifier", "reasoning_effort",
	}
	grokDropResponsesNoiseFields = []string{
		"stream_options", "metadata", "user", "service_tier", "store",
		"previous_response_id", "prompt_cache_retention", "safety_identifier",
		"truncation", "max_tool_calls", "prompt", "include",
	}
)

const grokMinOutputTokens = 128

func grokBaseModelID(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(model, "/"); i >= 0 {
		model = strings.TrimSpace(model[i+1:])
	}
	return model
}

func deleteGrokTopLevelFields(body []byte, fields []string) ([]byte, error) {
	out := body
	for _, field := range fields {
		if !gjson.GetBytes(out, field).Exists() {
			continue
		}
		next, err := sjson.DeleteBytes(out, field)
		if err != nil {
			return nil, err
		}
		out = next
	}
	return out, nil
}

func clampGrokMinTokens(body []byte, field string) ([]byte, error) {
	v := gjson.GetBytes(body, field)
	if !v.Exists() || v.Type != gjson.Number || v.Int() <= 0 || v.Int() >= grokMinOutputTokens {
		return body, nil
	}
	return sjson.SetBytes(body, field, grokMinOutputTokens)
}

// normalizeGrokOpenAIClientBody strips OpenAI-client noise for xAI Grok.
// forChat=true: /v1/chat/completions; false: /v1/responses (and patch path).
func normalizeGrokOpenAIClientBody(body []byte, upstreamModel string, forChat bool) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}
	if !json.Valid(body) {
		return nil, fmt.Errorf("invalid json request body")
	}

	drop := append([]string{}, grokDropPenaltyFields...)
	model := grokBaseModelID(upstreamModel)
	if model == "grok-4.5" || strings.HasPrefix(model, "grok-4.5-") {
		drop = append(drop, "stop")
	}
	if forChat {
		drop = append(drop, grokDropChatNoiseFields...)
	} else {
		drop = append(drop, grokDropResponsesNoiseFields...)
	}

	out, err := deleteGrokTopLevelFields(body, drop)
	if err != nil {
		return nil, err
	}
	if !forChat {
		return clampGrokMinTokens(out, "max_output_tokens")
	}

	// Bridge eligibility forbids both max_tokens and max_completion_tokens.
	if gjson.GetBytes(out, "max_tokens").Exists() && gjson.GetBytes(out, "max_completion_tokens").Exists() {
		out, err = sjson.DeleteBytes(out, "max_tokens")
		if err != nil {
			return nil, err
		}
	}
	for _, field := range []string{"max_tokens", "max_completion_tokens"} {
		out, err = clampGrokMinTokens(out, field)
		if err != nil {
			return nil, err
		}
	}

	// Drop empty tools / tool_choice=none so bridge eligibility can pass.
	for _, field := range []string{"tools", "functions"} {
		raw := gjson.GetBytes(out, field)
		if raw.Exists() && (raw.Type == gjson.Null || (raw.IsArray() && len(raw.Array()) == 0)) {
			out, err = sjson.DeleteBytes(out, field)
			if err != nil {
				return nil, err
			}
		}
	}
	for _, field := range []string{"tool_choice", "function_call"} {
		raw := gjson.GetBytes(out, field)
		if !raw.Exists() {
			continue
		}
		if raw.Type == gjson.Null || (raw.Type == gjson.String && strings.EqualFold(raw.String(), "none")) {
			out, err = sjson.DeleteBytes(out, field)
			if err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// reapplyGrokChatRouteSignals restores fields that must remain for bridge vs raw
// routing after OpenAI-client noise stripping (new-api penalties/empty tools).
// stop and reasoning_effort force raw Chat Completions; do not hide them from
// grokChatResponsesBridgeEligibility.
func reapplyGrokChatRouteSignals(original, normalized []byte) []byte {
	if len(original) == 0 || len(normalized) == 0 {
		return normalized
	}
	out := normalized
	for _, field := range []string{"stop", "reasoning_effort"} {
		src := gjson.GetBytes(original, field)
		if !src.Exists() || gjson.GetBytes(out, field).Exists() {
			continue
		}
		next, err := sjson.SetRawBytes(out, field, []byte(src.Raw))
		if err != nil {
			return out
		}
		out = next
	}
	return out
}

// sanitizeGrokResponsesImages normalizes vision parts for xAI Responses.
// Target shape: type=input_image with a plain string image_url (+ optional detail).
// Drops empty base64 data URIs and image-only messages that become empty.
func sanitizeGrokResponsesImages(body []byte) ([]byte, error) {
	if len(body) == 0 || (!bytes.Contains(body, []byte("image_url")) && !bytes.Contains(body, []byte("input_image"))) {
		return body, nil
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	changed := false
	for _, field := range []string{"input", "messages"} {
		raw, ok := req[field]
		if !ok || raw == nil {
			continue
		}
		next, fieldChanged := rewriteGrokImageValue(raw)
		if fieldChanged {
			req[field] = next
			changed = true
		}
	}
	if !changed {
		return body, nil
	}
	return json.Marshal(req)
}

// rewriteGrokImageValue walks arrays/maps and rewrites image parts in place.
// Returns (value, changed). Dropped nodes are omitted from arrays.
func rewriteGrokImageValue(value any) (any, bool) {
	switch typed := value.(type) {
	case []any:
		out := make([]any, 0, len(typed))
		changed := false
		for _, item := range typed {
			next, itemChanged, drop := rewriteGrokImageNode(item)
			if itemChanged || drop {
				changed = true
			}
			if !drop {
				out = append(out, next)
			}
		}
		if !changed {
			return value, false
		}
		return out, true
	default:
		next, changed, drop := rewriteGrokImageNode(value)
		if drop {
			return []any{}, true
		}
		return next, changed
	}
}

// rewriteGrokImageNode returns (node, changed, drop).
func rewriteGrokImageNode(value any) (any, bool, bool) {
	m, ok := value.(map[string]any)
	if !ok {
		return value, false, false
	}

	typeName := strings.ToLower(strings.TrimSpace(fmt.Sprint(m["type"])))
	if typeName == "input_image" || typeName == "image_url" {
		url, okURL := extractGrokImageURL(m)
		if !okURL {
			return nil, true, true
		}
		out := map[string]any{"type": "input_image", "image_url": url}
		if detail, ok := m["detail"].(string); ok {
			if detail = strings.TrimSpace(detail); detail != "" {
				out["detail"] = detail
			}
		}
		// Always rebuild: callers only care that the xAI shape is correct.
		return out, true, false
	}

	content, hasContent := m["content"]
	if !hasContent || content == nil {
		return m, false, false
	}

	switch parts := content.(type) {
	case []any:
		next, contentChanged := rewriteGrokImageValue(parts)
		if !contentChanged {
			return m, false, false
		}
		arr, _ := next.([]any)
		if len(arr) == 0 {
			return nil, true, true
		}
		m["content"] = arr
		return m, true, false
	case map[string]any:
		next, changed, drop := rewriteGrokImageNode(parts)
		if drop {
			delete(m, "content")
			return m, true, false
		}
		if changed {
			m["content"] = next
			return m, true, false
		}
		return m, false, false
	default:
		return m, false, false
	}
}

func extractGrokImageURL(m map[string]any) (string, bool) {
	if m == nil {
		return "", false
	}
	if url, ok := normalizeGrokImageURLValue(m["image_url"]); ok {
		return url, true
	}
	if nested, ok := m["image"].(map[string]any); ok {
		if url, ok := normalizeGrokImageURLValue(nested["image_url"]); ok {
			return url, true
		}
		if url, ok := normalizeGrokImageURLValue(nested["url"]); ok {
			return url, true
		}
	}
	// Some clients put the URL at the top level of an input_image item.
	if strings.EqualFold(strings.TrimSpace(fmt.Sprint(m["type"])), "input_image") {
		return normalizeGrokImageURLValue(m["url"])
	}
	return "", false
}

func normalizeGrokImageURLValue(raw any) (string, bool) {
	switch typed := raw.(type) {
	case string:
		url := strings.TrimSpace(typed)
		if url == "" || isEmptyBase64DataURI(url) {
			return "", false
		}
		return url, true
	case map[string]any:
		for _, key := range []string{"url", "image_url"} {
			if url, ok := normalizeGrokImageURLValue(typed[key]); ok {
				return url, true
			}
		}
		return "", false
	default:
		return "", false
	}
}

// sanitizeGrokMessageNames strips message "name" from non-user roles.
// xAI rejects: "Only message of role `user` can have a name."
func sanitizeGrokMessageNames(body []byte) ([]byte, error) {
	if len(body) == 0 || !bytes.Contains(body, []byte(`"name"`)) {
		return body, nil
	}
	changed := false
	out := body
	for _, field := range []string{"input", "messages"} {
		items := gjson.GetBytes(out, field)
		if !items.Exists() || !items.IsArray() {
			continue
		}
		for i, item := range items.Array() {
			if !item.IsObject() || !item.Get("name").Exists() {
				continue
			}
			role := strings.ToLower(strings.TrimSpace(item.Get("role").String()))
			// Keep user names; drop name on developer/system/assistant/tool/etc.
			if role == "user" {
				continue
			}
			// Also drop when role is empty but type is not a function_call-like item
			// that legitimately uses name (function tools use top-level name).
			itemType := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
			if role == "" && itemType != "" && itemType != "message" {
				continue
			}
			path := fmt.Sprintf("%s.%d.name", field, i)
			next, err := sjson.DeleteBytes(out, path)
			if err != nil {
				return nil, err
			}
			out = next
			changed = true
		}
	}
	if !changed {
		return body, nil
	}
	return out, nil
}
