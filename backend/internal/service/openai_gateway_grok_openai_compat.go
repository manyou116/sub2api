package service

import (
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
		"truncation", "max_tool_calls", "prompt",
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
