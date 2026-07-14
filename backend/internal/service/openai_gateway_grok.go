package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	grokComposerImageBridgeVisionModel     = "grok-build-0.1"
	grokComposerImageBridgeMaxOutputTokens = 512
	grokUpstreamUserAgent                  = "sub2api-grok/1.0"
	grokCLIVersion                         = "0.2.93"
	grokRateLimitFallbackCooldown          = 2 * time.Minute
)

func (s *OpenAIGatewayService) forwardGrokResponses(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	originalModel string,
	reqStream bool,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	if account.Type != AccountTypeOAuth && account.Type != AccountTypeAPIKey {
		return nil, fmt.Errorf("grok account type %s is not supported by Responses forwarding", account.Type)
	}

	upstreamModel := account.GetMappedModel(originalModel)
	if strings.TrimSpace(upstreamModel) == "" {
		upstreamModel = "grok-4.3"
	}
	cacheIdentity := resolveGrokCacheIdentity(c, body, "", upstreamModel)
	patchedBody, err := patchGrokResponsesBody(body, upstreamModel)
	if err != nil {
		return nil, err
	}
	patchedBody, err = applyGrokResponsesCacheIdentity(patchedBody, body, cacheIdentity, account.IsGrokOAuth())
	if err != nil {
		return nil, fmt.Errorf("apply grok prompt cache identity: %w", err)
	}

	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}

	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	defer releaseUpstreamCtx()
	upstreamReq, err := buildGrokResponsesRequest(upstreamCtx, c, account, patchedBody, token, cacheIdentity)
	if err != nil {
		return nil, err
	}

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	upstreamStart := time.Now()
	resp, err := s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(upstreamStart).Milliseconds())
	if err != nil {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody := s.readUpstreamErrorBody(resp)
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
		upstreamMsg := sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(respBody))
		if upstreamMsg == "" {
			upstreamMsg = fmt.Sprintf("xAI upstream returned status %d", resp.StatusCode)
		}
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  firstNonEmpty(resp.Header.Get("x-request-id"), resp.Header.Get("xai-request-id")),
			Kind:               "failover",
			Message:            upstreamMsg,
		})
		s.handleGrokAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody)
		if s.shouldFailoverUpstreamError(resp.StatusCode) {
			return nil, &UpstreamFailoverError{
				StatusCode:             resp.StatusCode,
				ResponseBody:           respBody,
				RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
			}
		}
		return s.handleErrorResponse(ctx, resp, c, account, patchedBody, upstreamModel)
	}

	s.updateGrokUsageSnapshot(ctx, account, xai.ParseQuotaHeaders(resp.Header, resp.StatusCode))

	var usage *OpenAIUsage
	var firstTokenMs *int
	responseID := ""
	if reqStream {
		streamResult, err := s.handleStreamingResponse(ctx, resp, c, account, startTime, originalModel, upstreamModel)
		if err != nil {
			return nil, err
		}
		usage = streamResult.usage
		firstTokenMs = streamResult.firstTokenMs
		responseID = strings.TrimSpace(streamResult.responseID)
	} else {
		nonStreamResult, err := s.handleNonStreamingResponse(ctx, resp, c, account, originalModel, upstreamModel)
		if err != nil {
			return nil, err
		}
		usage = nonStreamResult.usage
		responseID = strings.TrimSpace(nonStreamResult.responseID)
	}

	if usage == nil {
		usage = &OpenAIUsage{}
	}
	reasoningEffort := extractOpenAIReasoningEffortFromBody(patchedBody, originalModel)
	return &OpenAIForwardResult{
		RequestID:       firstNonEmpty(resp.Header.Get("x-request-id"), resp.Header.Get("xai-request-id")),
		ResponseID:      responseID,
		Usage:           *usage,
		Model:           originalModel,
		UpstreamModel:   upstreamModel,
		ReasoningEffort: reasoningEffort,
		Stream:          reqStream,
		OpenAIWSMode:    false,
		ResponseHeaders: resp.Header.Clone(),
		Duration:        time.Since(startTime),
		FirstTokenMs:    firstTokenMs,
	}, nil
}

func patchGrokResponsesBody(body []byte, upstreamModel string) ([]byte, error) {
	if !json.Valid(body) {
		return nil, fmt.Errorf("invalid json request body")
	}
	out, err := sjson.SetBytes(body, "model", upstreamModel)
	if err != nil {
		return nil, err
	}
	out, err = sanitizeGrokResponsesModelCapabilities(out, upstreamModel)
	if err != nil {
		return nil, err
	}
	out, err = normalizeGrokOpenAIClientBody(out, upstreamModel, false)
	if err != nil {
		return nil, err
	}
	out, err = sanitizeGrokResponsesUnsupportedFields(out)
	if err != nil {
		return nil, err
	}
	out, err = sanitizeGrokResponsesInput(out)
	if err != nil {
		return nil, err
	}
	out, err = sanitizeGrokResponsesTools(out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- Grok OpenAI-compat (new-api / Codex → xAI) ---
// Keep this block self-contained so upstream merges only conflict here.
// Fields OpenAI clients (new-api/SDK) send that xAI Grok rejects.
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

func sanitizeGrokResponsesModelCapabilities(body []byte, upstreamModel string) ([]byte, error) {
	if !grokModelRejectsReasoningEffort(upstreamModel) {
		return body, nil
	}

	out := body
	for _, field := range []string{"reasoning", "reasoning_effort", "reasoningEffort"} {
		if !gjson.GetBytes(out, field).Exists() {
			continue
		}
		var err error
		out, err = sjson.DeleteBytes(out, field)
		if err != nil {
			return nil, fmt.Errorf("remove unsupported Grok Composer %s: %w", field, err)
		}
	}
	return out, nil
}

func grokModelRejectsReasoningEffort(model string) bool {
	model = strings.TrimSpace(strings.ToLower(model))
	if slash := strings.LastIndex(model, "/"); slash >= 0 {
		model = strings.TrimSpace(model[slash+1:])
	}
	switch model {
	case "grok-composer", "grok-composer-2.5-fast", "composer-2.5":
		return true
	default:
		return false
	}
}

var grokResponsesUnsupportedRecursiveFields = map[string]struct{}{
	"external_web_access": {},
}

func sanitizeGrokResponsesUnsupportedFields(body []byte) ([]byte, error) {
	if !bytes.Contains(body, []byte(`"external_web_access"`)) {
		return body, nil
	}

	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if !deleteJSONFields(payload, grokResponsesUnsupportedRecursiveFields) {
		return body, nil
	}
	return json.Marshal(payload)
}

func deleteJSONFields(value any, fields map[string]struct{}) bool {
	switch typed := value.(type) {
	case map[string]any:
		changed := false
		for field := range fields {
			if _, ok := typed[field]; ok {
				delete(typed, field)
				changed = true
			}
		}
		for _, child := range typed {
			if deleteJSONFields(child, fields) {
				changed = true
			}
		}
		return changed
	case []any:
		changed := false
		for _, child := range typed {
			if deleteJSONFields(child, fields) {
				changed = true
			}
		}
		return changed
	default:
		return false
	}
}

// sanitizeGrokResponsesInput aligns Codex/OpenAI Responses input with xAI ModelInput.
// Strategy (CPA-compatible):
//  1. Drop private/unsupported item types (additional_tools, compaction_trigger, tool-call residues…)
//  2. Keep compaction only when encrypted_content validates as Grok-shaped; else drop the item
//  3. Strip invalid reasoning.encrypted_content; delete null content; merge adjacent reasoning summaries
//
// Never "strip everything and hope" — that produces 422 ModelInput errors.
func sanitizeGrokResponsesInput(body []byte) ([]byte, error) {
	input := gjson.GetBytes(body, "input")
	if !input.Exists() || !input.IsArray() {
		return body, nil
	}

	items := make([]json.RawMessage, 0, len(input.Array()))
	changed := false
	for _, item := range input.Array() {
		itemType := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
		role := strings.TrimSpace(item.Get("role").String())

		switch itemType {
		case "additional_tools", "compaction_trigger":
			changed = true
			continue
		case "web_search_call", "file_search_call", "code_interpreter_call",
			"image_generation_call", "computer_call", "computer_call_output",
			"mcp_call", "mcp_list_tools", "mcp_approval_request", "mcp_approval_response",
			"item_reference", "local_shell_call", "local_shell_call_output",
			"shell_call", "shell_call_output", "apply_patch_call", "apply_patch_call_output":
			// Codex/OpenAI tool residue — not valid xAI ModelInput variants.
			changed = true
			continue
		case "compaction":
			enc := item.Get("encrypted_content")
			if enc.Type != gjson.String || !isValidGrokEncryptedContent(enc.String()) {
				changed = true
				continue
			}
			items = append(items, json.RawMessage(item.Raw))
			continue
		case "reasoning":
			next, itemChanged, keep := normalizeGrokReasoningInputItem(item)
			if itemChanged {
				changed = true
			}
			if !keep {
				continue
			}
			// Merge consecutive reasoning summary-only items (CPA behavior).
			if len(items) > 0 && canMergeGrokReasoningSummary(items[len(items)-1], gjson.ParseBytes(next)) {
				merged, ok := mergeGrokReasoningSummary(items[len(items)-1], gjson.ParseBytes(next))
				if ok {
					items[len(items)-1] = json.RawMessage(merged)
					changed = true
					continue
				}
			}
			items = append(items, json.RawMessage(next))
			continue
		case "message", "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output":
			items = append(items, json.RawMessage(item.Raw))
			continue
		case "":
			// Role-based messages without type (user/system/assistant/developer).
			if role == "" {
				// Orphan object (often bare encrypted_content) — drop.
				if item.Get("encrypted_content").Exists() {
					changed = true
					continue
				}
				changed = true
				continue
			}
			items = append(items, json.RawMessage(item.Raw))
			continue
		default:
			// Unknown typed item: drop rather than 422 ModelInput.
			changed = true
			continue
		}
	}

	if !changed {
		return body, nil
	}
	if len(items) == 0 {
		// Keep a minimal user turn so the request remains valid.
		items = []json.RawMessage{json.RawMessage(`{"role":"user","content":"continue"}`)}
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	return sjson.SetRawBytes(body, "input", encoded)
}

func normalizeGrokReasoningInputItem(item gjson.Result) (next []byte, changed, keep bool) {
	raw := []byte(item.Raw)
	changed = false

	// xAI rejects content:null on reasoning items.
	if content := item.Get("content"); content.Exists() && content.Type == gjson.Null {
		cleaned, err := sjson.DeleteBytes(raw, "content")
		if err != nil {
			return raw, false, true
		}
		raw, changed = cleaned, true
	}

	if enc := item.Get("encrypted_content"); enc.Exists() {
		if enc.Type != gjson.String || !isValidGrokEncryptedContent(enc.String()) {
			cleaned, err := sjson.DeleteBytes(raw, "encrypted_content")
			if err != nil {
				return raw, changed, true
			}
			raw, changed = cleaned, true
		}
	}

	parsed := gjson.ParseBytes(raw)
	hasSummary := false
	if summary := parsed.Get("summary"); summary.IsArray() {
		for _, s := range summary.Array() {
			if strings.TrimSpace(s.Get("text").String()) != "" {
				hasSummary = true
				break
			}
		}
	}
	enc := parsed.Get("encrypted_content")
	hasEC := enc.Exists() && enc.Type == gjson.String && isValidGrokEncryptedContent(enc.String())
	if !hasSummary && !hasEC {
		return raw, true, false
	}
	return raw, changed, true
}

func canMergeGrokReasoningSummary(previous json.RawMessage, current gjson.Result) bool {
	prev := gjson.ParseBytes(previous)
	if prev.Get("type").String() != "reasoning" || current.Get("type").String() != "reasoning" {
		return false
	}
	// Only merge summary-only items (no encrypted_content on either side).
	if prev.Get("encrypted_content").Exists() || current.Get("encrypted_content").Exists() {
		return false
	}
	return current.Get("summary").IsArray()
}

func mergeGrokReasoningSummary(previous json.RawMessage, current gjson.Result) ([]byte, bool) {
	prev := gjson.ParseBytes(previous)
	summary := prev.Get("summary")
	buf := []byte(`[]`)
	if summary.IsArray() {
		for _, s := range summary.Array() {
			var err error
			buf, err = sjson.SetRawBytes(buf, "-1", []byte(s.Raw))
			if err != nil {
				return nil, false
			}
		}
	}
	if current.Get("summary").IsArray() {
		for _, s := range current.Get("summary").Array() {
			var err error
			buf, err = sjson.SetRawBytes(buf, "-1", []byte(s.Raw))
			if err != nil {
				return nil, false
			}
		}
	}
	out, err := sjson.SetRawBytes(previous, "summary", buf)
	if err != nil {
		return nil, false
	}
	return out, true
}

// isValidGrokEncryptedContent is a transport-shape check (CPA-inspired, not crypto).
func isValidGrokEncryptedContent(raw string) bool {
	sig := strings.TrimSpace(raw)
	if len(sig) < 64 || sig != raw {
		return false
	}
	if strings.HasPrefix(sig, "gAAAA") || strings.Contains(sig, "=") {
		return false
	}
	for _, r := range sig {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '+', r == '/':
		default:
			return false
		}
	}
	// Reject tiny decoded payloads (foreign stubs).
	// base64 length 64 ≈ 48 bytes decoded — require longer for real Grok blobs.
	if len(sig) < 80 {
		return false
	}
	return true
}

var grokResponsesSupportedToolTypes = map[string]struct{}{
	"code_execution":     {},
	"code_interpreter":   {},
	"collections_search": {},
	"file_search":        {},
	"function":           {},
	"mcp":                {},
	"shell":              {},
	"web_search":         {},
	"x_search":           {},
}

func sanitizeGrokResponsesTools(body []byte) ([]byte, error) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return body, nil
	}

	rawTools := tools.Array()
	filteredTools := make([]json.RawMessage, 0, len(rawTools))
	for _, tool := range rawTools {
		toolType := strings.TrimSpace(tool.Get("type").String())
		if _, ok := grokResponsesSupportedToolTypes[toolType]; ok {
			filteredTools = append(filteredTools, json.RawMessage(tool.Raw))
		}
	}

	var err error
	if len(filteredTools) != len(rawTools) {
		if len(filteredTools) == 0 {
			body, err = sjson.DeleteBytes(body, "tools")
		} else {
			var encoded []byte
			encoded, err = json.Marshal(filteredTools)
			if err != nil {
				return nil, err
			}
			body, err = sjson.SetRawBytes(body, "tools", encoded)
		}
		if err != nil {
			return nil, err
		}
	}

	toolChoice := gjson.GetBytes(body, "tool_choice")
	if !toolChoice.Exists() {
		return body, nil
	}
	if shouldDropGrokToolChoice(toolChoice, filteredTools) {
		body, err = sjson.DeleteBytes(body, "tool_choice")
		if err != nil {
			return nil, err
		}
	}
	return body, nil
}

func shouldDropGrokToolChoice(toolChoice gjson.Result, tools []json.RawMessage) bool {
	if len(tools) == 0 {
		return true
	}
	if !toolChoice.IsObject() {
		return false
	}
	choiceType := strings.TrimSpace(toolChoice.Get("type").String())
	if choiceType == "" {
		return false
	}
	if _, ok := grokResponsesSupportedToolTypes[choiceType]; !ok {
		return true
	}
	if choiceType == "function" {
		choiceName := strings.TrimSpace(toolChoice.Get("name").String())
		if choiceName == "" {
			choiceName = strings.TrimSpace(toolChoice.Get("function.name").String())
		}
		if choiceName == "" {
			return false
		}
		for _, tool := range tools {
			var item struct {
				Type     string `json:"type"`
				Name     string `json:"name"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if err := json.Unmarshal(tool, &item); err != nil {
				continue
			}
			name := strings.TrimSpace(item.Name)
			if name == "" {
				name = strings.TrimSpace(item.Function.Name)
			}
			if strings.TrimSpace(item.Type) == "function" && name == choiceName {
				return false
			}
		}
		return true
	}
	return false
}

func (s *OpenAIGatewayService) bridgeGrokComposerImageInputs(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	token string,
) ([]byte, OpenAIUsage, bool, error) {
	if !shouldBridgeGrokComposerImageInputs(body) {
		return body, OpenAIUsage{}, false, nil
	}

	var reqBody map[string]any
	if err := json.Unmarshal(body, &reqBody); err != nil {
		return body, OpenAIUsage{}, false, fmt.Errorf("parse grok composer image bridge request: %w", err)
	}

	imageURLs := collectGrokComposerImageURLs(reqBody)
	if len(imageURLs) == 0 {
		return body, OpenAIUsage{}, false, nil
	}

	descriptions := make([]string, 0, len(imageURLs))
	var bridgeUsage OpenAIUsage
	for index, imageURL := range imageURLs {
		description, usage, err := s.describeGrokComposerImage(ctx, c, account, token, imageURL, index+1)
		if err != nil {
			return body, bridgeUsage, false, err
		}
		descriptions = append(descriptions, description)
		addOpenAIUsage(&bridgeUsage, usage)
	}

	if !rewriteGrokComposerImagesAsText(reqBody, descriptions) {
		return body, bridgeUsage, false, nil
	}
	bridgedBody, err := marshalOpenAIUpstreamJSON(reqBody)
	if err != nil {
		return body, bridgeUsage, false, fmt.Errorf("serialize grok composer image bridge request: %w", err)
	}
	return bridgedBody, bridgeUsage, true, nil
}

func shouldBridgeGrokComposerImageInputs(body []byte) bool {
	if len(body) == 0 || !isGrokComposerModel(gjson.GetBytes(body, "model").String()) {
		return false
	}
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() {
		return false
	}
	return openAIJSONValueMayContainImageInput(messages)
}

func isGrokComposerModel(model string) bool {
	model = strings.TrimSpace(strings.ToLower(model))
	if model == "" {
		return false
	}
	if strings.Contains(model, "/") {
		parts := strings.Split(model, "/")
		model = strings.TrimSpace(parts[len(parts)-1])
	}
	return strings.Contains(model, "composer")
}

func collectGrokComposerImageURLs(reqBody map[string]any) []string {
	messages, ok := reqBody["messages"].([]any)
	if !ok {
		return nil
	}

	var imageURLs []string
	for _, msg := range messages {
		msgMap, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msgMap["content"].([]any)
		if !ok {
			continue
		}
		for _, part := range parts {
			if imageURL := grokComposerImageURLFromPart(part); imageURL != "" {
				imageURLs = append(imageURLs, imageURL)
			}
		}
	}
	return imageURLs
}

func grokComposerImageURLFromPart(part any) string {
	partMap, ok := part.(map[string]any)
	if !ok {
		return ""
	}
	if strings.TrimSpace(strings.ToLower(fmt.Sprint(partMap["type"]))) != "image_url" {
		return ""
	}
	switch imageURL := partMap["image_url"].(type) {
	case string:
		return normalizeGrokComposerImageURL(imageURL)
	case map[string]any:
		raw, _ := imageURL["url"].(string)
		return normalizeGrokComposerImageURL(raw)
	default:
		return ""
	}
}

func normalizeGrokComposerImageURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || isEmptyBase64DataURI(trimmed) {
		return ""
	}
	return trimmed
}

func (s *OpenAIGatewayService) describeGrokComposerImage(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	token string,
	imageURL string,
	index int,
) (string, OpenAIUsage, error) {
	body, err := buildGrokComposerImageDescriptionBody(imageURL, index)
	if err != nil {
		return "", OpenAIUsage{}, err
	}

	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	// Image-description probes are auxiliary requests, not conversation turns.
	// Do not bind them to the caller's Grok prompt-cache identity.
	upstreamReq, err := buildGrokResponsesRequest(upstreamCtx, c, account, body, token, "")
	releaseUpstreamCtx()
	if err != nil {
		return "", OpenAIUsage{}, fmt.Errorf("build grok composer image bridge request: %w", err)
	}

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	resp, err := s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		return "", OpenAIUsage{}, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody := s.readUpstreamErrorBody(resp)
		upstreamMsg := sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(respBody))
		if upstreamMsg == "" {
			upstreamMsg = fmt.Sprintf("xAI image bridge upstream returned status %d", resp.StatusCode)
		}
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  firstNonEmpty(resp.Header.Get("x-request-id"), resp.Header.Get("xai-request-id")),
			Kind:               "failover",
			Message:            upstreamMsg,
		})
		s.handleGrokAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody)
		if s.shouldFailoverUpstreamError(resp.StatusCode) {
			return "", OpenAIUsage{}, &UpstreamFailoverError{
				StatusCode:             resp.StatusCode,
				ResponseBody:           respBody,
				RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
			}
		}
		return "", OpenAIUsage{}, fmt.Errorf("grok composer image bridge upstream error: %s", upstreamMsg)
	}

	s.updateGrokUsageSnapshot(ctx, account, xai.ParseQuotaHeaders(resp.Header, resp.StatusCode))
	respBody, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, nil)
	if err != nil {
		return "", OpenAIUsage{}, fmt.Errorf("read grok composer image bridge response: %w", err)
	}

	var parsed apicompat.ResponsesResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", OpenAIUsage{}, fmt.Errorf("parse grok composer image bridge response: %w", err)
	}
	description := strings.TrimSpace(grokResponsesOutputText(&parsed))
	if description == "" {
		return "", copyOpenAIUsageFromResponsesUsage(parsed.Usage), fmt.Errorf("grok composer image bridge returned empty description")
	}
	return description, copyOpenAIUsageFromResponsesUsage(parsed.Usage), nil
}

func buildGrokComposerImageDescriptionBody(imageURL string, index int) ([]byte, error) {
	prompt := fmt.Sprintf("Describe image %d in concise, factual text for a downstream coding/composer model. Include visible text, UI elements, diagrams, errors, and spatial relationships. Do not mention that you are an image analysis bridge.", index)
	req := map[string]any{
		"model":             grokComposerImageBridgeVisionModel,
		"stream":            false,
		"store":             false,
		"max_output_tokens": grokComposerImageBridgeMaxOutputTokens,
		"input": []any{
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": prompt},
					map[string]any{"type": "input_image", "image_url": imageURL},
				},
			},
		},
	}
	return marshalOpenAIUpstreamJSON(req)
}

func grokResponsesOutputText(resp *apicompat.ResponsesResponse) string {
	if resp == nil {
		return ""
	}
	var parts []string
	for _, output := range resp.Output {
		for _, content := range output.Content {
			if content.Type == "output_text" || content.Type == "text" || content.Type == "input_text" {
				if text := strings.TrimSpace(content.Text); text != "" {
					parts = append(parts, text)
				}
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

func rewriteGrokComposerImagesAsText(reqBody map[string]any, descriptions []string) bool {
	messages, ok := reqBody["messages"].([]any)
	if !ok {
		return false
	}

	imageIndex := 0
	changed := false
	for _, msg := range messages {
		msgMap, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msgMap["content"].([]any)
		if !ok {
			continue
		}
		var textParts []string
		messageChanged := false
		for _, part := range parts {
			if imageURL := grokComposerImageURLFromPart(part); imageURL != "" {
				if imageIndex < len(descriptions) {
					textParts = append(textParts, fmt.Sprintf("Image %d description: %s", imageIndex+1, strings.TrimSpace(descriptions[imageIndex])))
				}
				imageIndex++
				messageChanged = true
				continue
			}
			if text := grokComposerTextFromPart(part); text != "" {
				textParts = append(textParts, text)
			}
		}
		if messageChanged {
			msgMap["content"] = strings.Join(textParts, "\n\n")
			changed = true
		}
	}
	return changed
}

func grokComposerTextFromPart(part any) string {
	partMap, ok := part.(map[string]any)
	if !ok {
		return ""
	}
	partType := strings.TrimSpace(strings.ToLower(fmt.Sprint(partMap["type"])))
	switch partType {
	case "text", "input_text":
		text, _ := partMap["text"].(string)
		return strings.TrimSpace(text)
	default:
		return ""
	}
}

func addOpenAIUsage(dst *OpenAIUsage, usage OpenAIUsage) {
	if dst == nil {
		return
	}
	dst.InputTokens += usage.InputTokens
	dst.ImageInputTokens += usage.ImageInputTokens
	dst.OutputTokens += usage.OutputTokens
	dst.CacheCreationInputTokens += usage.CacheCreationInputTokens
	dst.CacheReadInputTokens += usage.CacheReadInputTokens
	dst.ImageOutputTokens += usage.ImageOutputTokens
}

func buildGrokResponsesRequest(ctx context.Context, c *gin.Context, account *Account, body []byte, token, cacheIdentity string) (*http.Request, error) {
	targetURL, err := xai.BuildResponsesURL(account.GetGrokBaseURL())
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	applyGrokCLIHeaders(req.Header)
	applyGrokCacheHeaders(req.Header, cacheIdentity)
	if c != nil {
		if v := c.GetHeader("OpenAI-Beta"); strings.TrimSpace(v) != "" {
			req.Header.Set("OpenAI-Beta", v)
		}
	}
	return req, nil
}

// applyGrokCLIHeaders identifies subscription traffic as a supported Grok CLI
// version. The CLI gateway rejects otherwise valid OAuth requests without it.
func applyGrokCLIHeaders(headers http.Header) {
	if headers == nil {
		return
	}
	headers.Set("User-Agent", grokUpstreamUserAgent)
	headers.Set("X-Grok-Client-Version", grokCLIVersion)
}

func (s *OpenAIGatewayService) updateGrokUsageSnapshot(ctx context.Context, account *Account, snapshot *xai.QuotaSnapshot) {
	if s == nil || account == nil || account.ID <= 0 || snapshot == nil {
		return
	}
	accountID := account.ID
	now := time.Now()
	resetAt, hasActiveLimit := grokRateLimitResetAt(snapshot, now)
	if hasActiveLimit {
		normalizeGrokExhaustedWindowResets(snapshot, resetAt, now)
	}
	critical := snapshot.StatusCode == http.StatusTooManyRequests || hasActiveLimit
	if s.codexSnapshotThrottle != nil {
		allowed := s.codexSnapshotThrottle.Allow(accountID, now)
		if !critical && !allowed {
			return
		}
	}

	stateCtx := ctx
	if hasActiveLimit {
		var cancel context.CancelFunc
		stateCtx, cancel = openAIAccountStateContext(ctx)
		defer cancel()
	}
	if s.accountRepo != nil {
		_ = s.accountRepo.UpdateExtra(stateCtx, accountID, map[string]any{
			grokQuotaSnapshotExtraKey: snapshot,
		})
	}
	// Error responses are reconciled by handleGrokAccountUpstreamError, which
	// also installs the immediate in-memory scheduling block. Successful
	// responses can still consume the last available request/token, so persist
	// that exhausted window here as a real rate limit rather than relying only
	// on the passive snapshot scheduler check.
	if hasActiveLimit {
		s.rateLimitGrok(stateCtx, account, resetAt)
	}
}

func parseGrokQuotaSnapshot(headers http.Header, statusCode int, now time.Time) *xai.QuotaSnapshot {
	snapshot := xai.ParseQuotaHeaders(headers, statusCode)
	if snapshot == nil && statusCode == http.StatusTooManyRequests {
		return &xai.QuotaSnapshot{
			StatusCode: statusCode,
			UpdatedAt:  now.UTC().Format(time.RFC3339),
		}
	}
	return snapshot
}

func normalizeGrokExhaustedWindowResets(snapshot *xai.QuotaSnapshot, resetAt, now time.Time) {
	if snapshot == nil || !resetAt.After(now) {
		return
	}
	for _, window := range []*xai.QuotaWindow{snapshot.Requests, snapshot.Tokens} {
		if window == nil || window.Remaining == nil || *window.Remaining > 0 {
			continue
		}
		candidate := time.Time{}
		if window.ResetUnix != nil && *window.ResetUnix > 0 {
			candidate = time.Unix(*window.ResetUnix, 0)
		} else if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(window.ResetAt)); err == nil {
			candidate = parsed
		}
		if !candidate.After(now) {
			candidate = resetAt
		}
		resetUnix := candidate.Unix()
		window.ResetUnix = &resetUnix
		window.ResetAt = candidate.UTC().Format(time.RFC3339)
	}
}

func grokRateLimitResetAt(snapshot *xai.QuotaSnapshot, now time.Time) (time.Time, bool) {
	if snapshot == nil {
		return time.Time{}, false
	}

	// Retry-After is xAI's explicit retry boundary. Use the observation time so
	// a persisted snapshot does not start a fresh cooldown every time it is read.
	retryAfterExpired := false
	var resetAt time.Time
	if snapshot.RetryAfterSeconds != nil && *snapshot.RetryAfterSeconds > 0 {
		observedAt := now
		if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(snapshot.UpdatedAt)); err == nil {
			observedAt = parsed
		}
		retryAfterResetAt := observedAt.Add(time.Duration(*snapshot.RetryAfterSeconds) * time.Second)
		if retryAfterResetAt.After(now) {
			resetAt = retryAfterResetAt
		} else {
			retryAfterExpired = true
		}
	}

	exhausted := false
	for _, window := range []*xai.QuotaWindow{snapshot.Requests, snapshot.Tokens} {
		if window == nil || window.Remaining == nil || *window.Remaining > 0 {
			continue
		}
		exhausted = true
		candidate := time.Time{}
		if window.ResetUnix != nil && *window.ResetUnix > 0 {
			candidate = time.Unix(*window.ResetUnix, 0)
		} else if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(window.ResetAt)); err == nil {
			candidate = parsed
		}
		if candidate.After(now) && candidate.After(resetAt) {
			resetAt = candidate
		}
	}
	if !resetAt.IsZero() {
		return resetAt, true
	}
	// An observed Retry-After is an absolute boundary once combined with the
	// snapshot timestamp. Do not turn an expired persisted snapshot into a new
	// rolling fallback cooldown, but still allow a later explicit window reset.
	if retryAfterExpired {
		return time.Time{}, false
	}
	if exhausted || snapshot.StatusCode == http.StatusTooManyRequests {
		return now.Add(grokRateLimitFallbackCooldown), true
	}
	return time.Time{}, false
}

func normalizeGrokRateLimitResetAt(account *Account, resetAt, now time.Time) time.Time {
	if !resetAt.After(now) {
		resetAt = now.Add(grokRateLimitFallbackCooldown)
	}
	if account != nil && account.RateLimitResetAt != nil && account.RateLimitResetAt.After(resetAt) {
		resetAt = *account.RateLimitResetAt
	}
	return resetAt
}

type grokRateLimitExtendingRepository interface {
	SetRateLimitedIfLater(ctx context.Context, id int64, resetAt time.Time) error
}

func persistGrokRateLimit(ctx context.Context, repo AccountRepository, account *Account, resetAt time.Time) {
	if repo == nil || account == nil || account.ID <= 0 {
		return
	}
	resetAt = normalizeGrokRateLimitResetAt(account, resetAt, time.Now())
	stateCtx, cancel := openAIAccountStateContext(ctx)
	defer cancel()
	var err error
	if extendingRepo, ok := repo.(grokRateLimitExtendingRepository); ok {
		err = extendingRepo.SetRateLimitedIfLater(stateCtx, account.ID, resetAt)
	} else {
		err = repo.SetRateLimited(stateCtx, account.ID, resetAt)
	}
	if err != nil {
		slog.Warn("persist_grok_rate_limit_failed", "account_id", account.ID, "reset_at", resetAt.UTC(), "error", err)
	}
}

func (s *OpenAIGatewayService) rateLimitGrok(ctx context.Context, account *Account, resetAt time.Time) {
	if s == nil || account == nil {
		return
	}
	resetAt = normalizeGrokRateLimitResetAt(account, resetAt, time.Now())

	runtimeUntil := resetAt
	if account.TempUnschedulableUntil != nil && account.TempUnschedulableUntil.After(runtimeUntil) {
		runtimeUntil = *account.TempUnschedulableUntil
	}
	s.BlockAccountScheduling(account, runtimeUntil, "429")
	persistGrokRateLimit(ctx, s.accountRepo, account, resetAt)
}

func applyGrokFreeUsageExhaustedCooldown(snapshot *xai.QuotaSnapshot, now time.Time, body []byte) *xai.QuotaSnapshot {
	if snapshot == nil {
		snapshot = &xai.QuotaSnapshot{}
	}
	secs := int(xai.FreeUsageExhaustedCooldown / time.Second)
	if snapshot.RetryAfterSeconds == nil || *snapshot.RetryAfterSeconds < secs {
		snapshot.RetryAfterSeconds = &secs
	}
	if snapshot.StatusCode == 0 {
		snapshot.StatusCode = http.StatusTooManyRequests
	}
	if strings.TrimSpace(snapshot.UpdatedAt) == "" {
		snapshot.UpdatedAt = now.UTC().Format(time.RFC3339)
	}

	// Free-usage budget is not the same as short x-ratelimit windows. When the
	// body reports actual/limit, write that into the token window so the UI does
	// not keep showing "100% remaining" from stale short-window headers.
	resetAt := now.Add(xai.FreeUsageExhaustedCooldown).UTC()
	resetUnix := resetAt.Unix()
	resetRFC := resetAt.Format(time.RFC3339)
	zero := int64(0)
	if used, limit, ok := xai.FreeUsageTokenWindow(body); ok {
		remaining := limit - used
		if remaining < 0 {
			remaining = 0
		}
		snapshot.Tokens = &xai.QuotaWindow{
			Limit:     &limit,
			Remaining: &remaining,
			ResetUnix: &resetUnix,
			ResetAt:   resetRFC,
		}
	} else if snapshot.Tokens != nil && snapshot.Tokens.Limit != nil {
		snapshot.Tokens.Remaining = &zero
		if snapshot.Tokens.ResetUnix == nil {
			snapshot.Tokens.ResetUnix = &resetUnix
			snapshot.Tokens.ResetAt = resetRFC
		}
	} else {
		// No headers and no parseable window: still mark tokens exhausted for UI.
		lim := int64(1)
		snapshot.Tokens = &xai.QuotaWindow{
			Limit:     &lim,
			Remaining: &zero,
			ResetUnix: &resetUnix,
			ResetAt:   resetRFC,
		}
	}
	// Short request windows often still look "full" while free budget is dead.
	if snapshot.Requests != nil && snapshot.Requests.Limit != nil {
		snapshot.Requests.Remaining = &zero
		if snapshot.Requests.ResetUnix == nil {
			snapshot.Requests.ResetUnix = &resetUnix
			snapshot.Requests.ResetAt = resetRFC
		}
	}
	return snapshot
}

func (s *OpenAIGatewayService) handleGrokAccountUpstreamError(ctx context.Context, account *Account, statusCode int, headers http.Header, responseBody []byte) {
	if s == nil || account == nil {
		return
	}
	now := time.Now()
	snapshot := parseGrokQuotaSnapshot(headers, statusCode, now)
	if statusCode == http.StatusTooManyRequests && xai.FreeUsageExhausted(responseBody) {
		snapshot = applyGrokFreeUsageExhaustedCooldown(snapshot, now, responseBody)
	}
	s.updateGrokUsageSnapshot(ctx, account, snapshot)
	switch statusCode {
	case http.StatusUnauthorized:
		s.tempUnscheduleGrok(ctx, account, 10*time.Minute, "grok credentials unauthorized")
	case http.StatusForbidden:
		s.tempUnscheduleGrok(ctx, account, 30*time.Minute, "grok access or entitlement denied")
	case http.StatusTooManyRequests:
		// updateGrokUsageSnapshot installs both runtime and durable rate-limit state.
	default:
		if statusCode >= 500 {
			s.tempUnscheduleGrok(ctx, account, 2*time.Minute, "grok upstream temporary error")
		}
	}
}

func (s *OpenAIGatewayService) tempUnscheduleGrok(ctx context.Context, account *Account, cooldown time.Duration, reason string) {
	if s == nil || account == nil {
		return
	}
	until := time.Now().Add(cooldown)
	if account.TempUnschedulableUntil != nil && account.TempUnschedulableUntil.After(until) {
		until = *account.TempUnschedulableUntil
	}
	s.BlockAccountScheduling(account, until, reason)
	if s.accountRepo != nil {
		stateCtx, cancel := openAIAccountStateContext(ctx)
		defer cancel()
		_ = s.accountRepo.SetTempUnschedulable(stateCtx, account.ID, until, reason)
	}
}
