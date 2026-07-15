package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConvertKiroTools_Basic(t *testing.T) {
	tools := []kiroOpenAITool{
		{Type: "function", Function: kiroOpenAIToolFunc{
			Name:        "get_time",
			Description: "Get time",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
		}},
	}
	out := convertKiroTools(tools)
	if len(out) != 1 {
		t.Fatalf("expected 1, got %d", len(out))
	}
	spec := out[0].ToolSpecification
	if spec.Name != "get_time" || spec.Description != "Get time" {
		t.Fatalf("spec: %+v", spec)
	}
	if spec.InputSchema.JSON["type"] != "object" {
		t.Fatalf("schema: %+v", spec.InputSchema)
	}
}

func TestConvertKiroTools_EmptyDescriptionFallback(t *testing.T) {
	// Kiro upstream returns 400 when description is empty; we fall back to name.
	tools := []kiroOpenAITool{
		{Type: "function", Function: kiroOpenAIToolFunc{Name: "fn", Parameters: map[string]any{"type": "object"}}},
	}
	out := convertKiroTools(tools)
	if out[0].ToolSpecification.Description != "fn" {
		t.Fatalf("expected fallback to name, got %q", out[0].ToolSpecification.Description)
	}
}

func TestConvertKiroTools_NilParametersDefaulted(t *testing.T) {
	tools := []kiroOpenAITool{
		{Type: "function", Function: kiroOpenAIToolFunc{Name: "fn", Description: "d"}},
	}
	out := convertKiroTools(tools)
	if out[0].ToolSpecification.InputSchema.JSON["type"] != "object" {
		t.Fatalf("expected default object schema, got %+v", out[0].ToolSpecification.InputSchema)
	}
}

func TestConvertKiroTools_SkipsNonFunction(t *testing.T) {
	tools := []kiroOpenAITool{
		{Type: "code_interpreter"},
		{Type: "function", Function: kiroOpenAIToolFunc{Name: "ok", Description: "d", Parameters: map[string]any{"type": "object"}}},
	}
	out := convertKiroTools(tools)
	if len(out) != 1 || out[0].ToolSpecification.Name != "ok" {
		t.Fatalf("got %+v", out)
	}
}

func TestConvertKiroTools_Empty(t *testing.T) {
	if convertKiroTools(nil) != nil {
		t.Fatal("expected nil for empty input")
	}
}

func TestConvertKiroToolCalls_ParsesArguments(t *testing.T) {
	calls := []kiroOpenAIToolCall{
		{ID: "c1", Function: kiroOpenAIToolCallFunc{Name: "f", Arguments: `{"x":1}`}},
	}
	out := convertKiroToolCalls(calls)
	if len(out) != 1 {
		t.Fatalf("got %d", len(out))
	}
	if out[0].ToolUseID != "c1" || out[0].Name != "f" {
		t.Fatalf("call: %+v", out[0])
	}
	if v, _ := out[0].Input["x"].(float64); v != 1 {
		t.Fatalf("input: %+v", out[0].Input)
	}
}

func TestConvertKiroToolCalls_EmptyArguments(t *testing.T) {
	calls := []kiroOpenAIToolCall{{ID: "c1", Function: kiroOpenAIToolCallFunc{Name: "f", Arguments: ""}}}
	out := convertKiroToolCalls(calls)
	if out[0].Input == nil {
		t.Fatal("expected non-nil empty map")
	}
	if len(out[0].Input) != 0 {
		t.Fatalf("expected empty, got %+v", out[0].Input)
	}
}

func TestConvertKiroToolCalls_InvalidJSONArguments(t *testing.T) {
	// Should not panic; input ends up empty map.
	calls := []kiroOpenAIToolCall{{ID: "c1", Function: kiroOpenAIToolCallFunc{Name: "f", Arguments: `not-json`}}}
	out := convertKiroToolCalls(calls)
	if out[0].Input == nil || len(out[0].Input) != 0 {
		t.Fatalf("expected empty map, got %+v", out[0].Input)
	}
}

// ============ buildKiroPayload ============

func mkMsg(role, content string) kiroOpenAIMessage {
	c, _ := json.Marshal(content)
	return kiroOpenAIMessage{Role: role, Content: c}
}

func TestBuildKiroPayload_BasicUserAssistant(t *testing.T) {
	req := &kiroOpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []kiroOpenAIMessage{
			mkMsg("user", "hi"),
		},
	}
	p, err := buildKiroPayload(req, "anthropic.claude-sonnet-4.5", "", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if p.ConversationState.CurrentMessage.UserInputMessage.Content != "hi" {
		t.Fatalf("content: %+v", p.ConversationState.CurrentMessage)
	}
	if p.ConversationState.CurrentMessage.UserInputMessage.ModelID != "anthropic.claude-sonnet-4.5" {
		t.Fatalf("modelID: %v", p.ConversationState.CurrentMessage.UserInputMessage.ModelID)
	}
}

func TestBuildKiroPayload_SystemPrependedToUser(t *testing.T) {
	req := &kiroOpenAIRequest{
		Model: "x",
		Messages: []kiroOpenAIMessage{
			mkMsg("system", "be brief"),
			mkMsg("user", "explain"),
		},
	}
	p, err := buildKiroPayload(req, "m", "", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	got := p.ConversationState.CurrentMessage.UserInputMessage.Content
	if got != "be brief\n\nexplain" {
		t.Fatalf("merged: %q", got)
	}
}

func TestBuildKiroPayload_EmptyMessagesError(t *testing.T) {
	_, err := buildKiroPayload(&kiroOpenAIRequest{Model: "m"}, "m", "", "")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestBuildKiroPayload_OnlySystemError(t *testing.T) {
	// system without any user/assistant should fail
	req := &kiroOpenAIRequest{
		Model:    "m",
		Messages: []kiroOpenAIMessage{mkMsg("system", "be brief")},
	}
	_, err := buildKiroPayload(req, "m", "", "")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestBuildKiroPayload_ToolResultFoldedToSyntheticUser(t *testing.T) {
	// A trailing tool message should be wrapped by a synthetic user that carries toolResults.
	c, _ := json.Marshal("the answer is 42")
	req := &kiroOpenAIRequest{
		Model: "m",
		Messages: []kiroOpenAIMessage{
			mkMsg("user", "compute"),
			{
				Role: "assistant",
				ToolCalls: []kiroOpenAIToolCall{
					{ID: "tu1", Type: "function", Function: kiroOpenAIToolCallFunc{Name: "calc", Arguments: `{}`}},
				},
				Content: json.RawMessage(`""`),
			},
			{Role: "tool", Content: c, ToolCallID: "tu1"},
		},
	}
	p, err := buildKiroPayload(req, "m", "", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	curr := p.ConversationState.CurrentMessage.UserInputMessage
	if curr.UserInputMessageContext == nil {
		t.Fatalf("expected userInputMessageContext for synthetic user, got nil")
	}
	results := curr.UserInputMessageContext.ToolResults
	if len(results) != 1 || results[0].ToolUseID != "tu1" {
		t.Fatalf("tool results: %+v", results)
	}
	// History should contain the assistant message with toolUses.
	if len(p.ConversationState.History) == 0 {
		t.Fatalf("expected history items")
	}
}

func TestBuildKiroPayload_ToolResultsFlushBeforeFollowingAssistant(t *testing.T) {
	// OpenAI multi-turn tool loop: assistant(tool_calls) -> tool -> assistant(reply) -> user
	// toolResults must sit on a user turn immediately after the toolUses assistant.
	toolContent, _ := json.Marshal(`{"ok":true}`)
	req := &kiroOpenAIRequest{
		Model: "m",
		Messages: []kiroOpenAIMessage{
			mkMsg("user", "compute"),
			{
				Role: "assistant",
				ToolCalls: []kiroOpenAIToolCall{
					{ID: "tu1", Type: "function", Function: kiroOpenAIToolCallFunc{Name: "calc", Arguments: `{"x":1}`}},
				},
				Content: json.RawMessage(`""`),
			},
			{Role: "tool", Content: toolContent, ToolCallID: "tu1"},
			mkMsg("assistant", "the answer is 1"),
			mkMsg("user", "thanks"),
		},
	}
	p, err := buildKiroPayload(req, "m", "", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	hist := p.ConversationState.History
	// expect: user, assistant(toolUses), user(toolResults), assistant(reply)
	if len(hist) != 4 {
		t.Fatalf("history len=%d want 4: %+v", len(hist), hist)
	}
	if hist[1].AssistantResponseMessage == nil || len(hist[1].AssistantResponseMessage.ToolUses) != 1 {
		t.Fatalf("history[1] toolUses: %+v", hist[1])
	}
	if hist[2].UserInputMessage == nil || hist[2].UserInputMessage.UserInputMessageContext == nil {
		t.Fatalf("history[2] missing toolResults user: %+v", hist[2])
	}
	results := hist[2].UserInputMessage.UserInputMessageContext.ToolResults
	if len(results) != 1 || results[0].ToolUseID != "tu1" {
		t.Fatalf("toolResults: %+v", results)
	}
	if hist[3].AssistantResponseMessage == nil || hist[3].AssistantResponseMessage.Content != "the answer is 1" {
		t.Fatalf("history[3] assistant: %+v", hist[3])
	}
	// current user has no orphan toolResults
	curr := p.ConversationState.CurrentMessage.UserInputMessage
	if curr.Content != "thanks" {
		t.Fatalf("current content: %q", curr.Content)
	}
	if curr.UserInputMessageContext != nil && len(curr.UserInputMessageContext.ToolResults) != 0 {
		t.Fatalf("current must not carry toolResults: %+v", curr.UserInputMessageContext)
	}
}

func TestBuildKiroPayload_DropsOrphanToolResults(t *testing.T) {
	// tool result without a matching previous assistant.tool_calls must not be forwarded.
	toolContent, _ := json.Marshal("orphan")
	req := &kiroOpenAIRequest{
		Model: "m",
		Messages: []kiroOpenAIMessage{
			mkMsg("user", "hi"),
			{Role: "tool", Content: toolContent, ToolCallID: "missing"},
			mkMsg("user", "continue"),
		},
	}
	p, err := buildKiroPayload(req, "m", "", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	curr := p.ConversationState.CurrentMessage.UserInputMessage
	if curr.UserInputMessageContext != nil && len(curr.UserInputMessageContext.ToolResults) != 0 {
		t.Fatalf("expected orphan toolResults dropped, got %+v", curr.UserInputMessageContext.ToolResults)
	}
}

func TestBuildKiroPayload_DedupesToolResultsByID(t *testing.T) {
	c1, _ := json.Marshal("first")
	c2, _ := json.Marshal("second")
	req := &kiroOpenAIRequest{
		Model: "m",
		Messages: []kiroOpenAIMessage{
			mkMsg("user", "compute"),
			{
				Role: "assistant",
				ToolCalls: []kiroOpenAIToolCall{
					{ID: "tu1", Type: "function", Function: kiroOpenAIToolCallFunc{Name: "calc", Arguments: `{}`}},
				},
				Content: json.RawMessage(`""`),
			},
			{Role: "tool", Content: c1, ToolCallID: "tu1"},
			{Role: "tool", Content: c2, ToolCallID: "tu1"},
		},
	}
	p, err := buildKiroPayload(req, "m", "", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	results := p.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.ToolResults
	if len(results) != 1 || results[0].ToolUseID != "tu1" || results[0].Content[0].Text != "second" {
		t.Fatalf("deduped results: %+v", results)
	}
}

func TestBuildKiroPayload_ToolsAttachedToCurrentMessage(t *testing.T) {
	req := &kiroOpenAIRequest{
		Model:    "m",
		Messages: []kiroOpenAIMessage{mkMsg("user", "use the tool")},
		Tools: []kiroOpenAITool{
			{Type: "function", Function: kiroOpenAIToolFunc{
				Name: "fn", Description: "do", Parameters: map[string]any{"type": "object"},
			}},
		},
	}
	p, err := buildKiroPayload(req, "m", "", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	ctx := p.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if ctx == nil || len(ctx.Tools) != 1 {
		t.Fatalf("expected tools attached, got %+v", ctx)
	}
	if ctx.Tools[0].ToolSpecification.Name != "fn" {
		t.Fatalf("tool name: %+v", ctx.Tools[0])
	}
}

func TestBuildKiroPayload_ProfileArnPropagates(t *testing.T) {
	req := &kiroOpenAIRequest{
		Model:    "m",
		Messages: []kiroOpenAIMessage{mkMsg("user", "hi")},
	}
	p, err := buildKiroPayload(req, "m", "arn:aws:codewhisperer:::profile/X", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.ProfileArn != "arn:aws:codewhisperer:::profile/X" {
		t.Fatalf("profileArn: %q", p.ProfileArn)
	}
}

func TestResolveKiroInternalModel_AppliesAccountMappingFirst(t *testing.T) {
	account := &Account{
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"gpt-5.4": "claude-sonnet-4.5",
			},
		},
	}

	if !account.IsModelSupported("gpt-5.4") {
		t.Fatal("expected mapped request model to be supported")
	}
	if account.IsModelSupported("gpt-5.5") {
		t.Fatal("expected unrelated request model to be rejected by mapping")
	}

	got := resolveKiroInternalModel(account, "gpt-5.4")
	if got != "claude-sonnet-4.5" {
		t.Fatalf("expected account mapping before Kiro mapping, got %q", got)
	}
}

func TestExtractKiroDelta_ParsesUsageCacheFields(t *testing.T) {
	payload := []byte(`{
		"usage": {
			"input_tokens": 120,
			"output_tokens": 34,
			"cache_creation_input_tokens": 12,
			"input_tokens_details": {"cached_tokens": 56}
		}
	}`)

	ev := extractKiroDelta(payload)

	require.Equal(t, int64(120), ev.InputTokens)
	require.Equal(t, int64(34), ev.OutputTokens)
	require.Equal(t, int64(12), ev.CacheCreationInputTokens)
	require.Equal(t, int64(56), ev.CacheReadInputTokens)
}

func TestApplyKiroFrameUsage_PropagatesCacheFields(t *testing.T) {
	result := &KiroChatResult{InputTokens: 5}
	applyKiroFrameUsage(result, kiroFrameEvent{
		InputTokens:              10,
		OutputTokens:             4,
		CacheCreationInputTokens: 2,
		CacheReadInputTokens:     3,
	})

	require.Equal(t, int64(10), result.InputTokens)
	require.Equal(t, int64(4), result.OutputTokens)
	require.Equal(t, int64(2), result.CacheCreationInputTokens)
	require.Equal(t, int64(3), result.CacheReadInputTokens)
}

func TestKiroChatUsage_IncludesCacheDetails(t *testing.T) {
	usage := kiroChatUsage(&KiroChatResult{
		InputTokens:              100,
		OutputTokens:             20,
		CacheCreationInputTokens: 7,
		CacheReadInputTokens:     11,
	})
	require.NotNil(t, usage)
	require.Equal(t, 100, usage.PromptTokens)
	require.Equal(t, 20, usage.CompletionTokens)
	require.Equal(t, 120, usage.TotalTokens)
	require.NotNil(t, usage.PromptTokensDetails)
	require.Equal(t, 11, usage.PromptTokensDetails.CachedTokens)
	require.Equal(t, 7, usage.PromptTokensDetails.CacheCreationTokens)
}

// ============ extractKiroDelta ============

func TestExtractKiroDelta_Text(t *testing.T) {
	ev := extractKiroDelta([]byte(`{"content":"hello","modelId":"x"}`))
	if ev.Text != "hello" {
		t.Fatalf("text: %q", ev.Text)
	}
}

func TestExtractKiroDelta_LegacyAssistantResponseEvent(t *testing.T) {
	ev := extractKiroDelta([]byte(`{"assistantResponseEvent":{"content":"hey"}}`))
	if ev.Text != "hey" {
		t.Fatalf("text: %q", ev.Text)
	}
}

func TestExtractKiroDelta_ToolUseStart(t *testing.T) {
	ev := extractKiroDelta([]byte(`{"toolUseId":"t1","name":"get_x"}`))
	if ev.ToolUseID != "t1" || ev.ToolName != "get_x" {
		t.Fatalf("ev: %+v", ev)
	}
}

func TestExtractKiroDelta_ToolUseStringInput(t *testing.T) {
	ev := extractKiroDelta([]byte(`{"toolUseId":"t1","input":"{\"k\":1"}`))
	if ev.ToolInputDelta != `{"k":1` {
		t.Fatalf("delta: %q", ev.ToolInputDelta)
	}
}

func TestExtractKiroDelta_ToolUseObjectInput(t *testing.T) {
	ev := extractKiroDelta([]byte(`{"toolUseId":"t1","input":{"k":1}}`))
	if ev.ToolInputDelta == "" {
		t.Fatalf("empty delta")
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(ev.ToolInputDelta), &v); err != nil {
		t.Fatalf("not json: %s", ev.ToolInputDelta)
	}
}

func TestExtractKiroDelta_ToolStop(t *testing.T) {
	ev := extractKiroDelta([]byte(`{"toolUseId":"t1","stop":true}`))
	if !ev.ToolStop {
		t.Fatal("expected stop=true")
	}
}

func TestExtractKiroDelta_Usage(t *testing.T) {
	ev := extractKiroDelta([]byte(`{"usage":{"inputTokens":5,"outputTokens":7}}`))
	if ev.InputTokens != 5 || ev.OutputTokens != 7 {
		t.Fatalf("usage: %+v", ev)
	}
}

func TestExtractKiroDelta_UsageSnakeCase(t *testing.T) {
	ev := extractKiroDelta([]byte(`{"usage":{"input_tokens":3,"output_tokens":4}}`))
	if ev.InputTokens != 3 || ev.OutputTokens != 4 {
		t.Fatalf("usage: %+v", ev)
	}
}

func TestExtractKiroDelta_InvalidJSON(t *testing.T) {
	ev := extractKiroDelta([]byte(`not-json`))
	if ev.Text != "" || ev.ToolUseID != "" {
		t.Fatalf("expected zero ev, got %+v", ev)
	}
}

func TestApplyKiroEstimatedCacheUsage_MultiTurnSetsCacheRead(t *testing.T) {
	req := kiroOpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []kiroOpenAIMessage{
			{Role: "system", Content: mustKiroRawJSON(`long system prompt for cache prefix testing ` + strings.Repeat("rules ", 40))},
			{Role: "user", Content: mustKiroRawJSON("hello")},
			{Role: "assistant", Content: mustKiroRawJSON("hi there")},
			{Role: "user", Content: mustKiroRawJSON("follow up")},
		},
	}
	total := estimateKiroTokens(req)
	result := &KiroChatResult{InputTokens: total}
	applyKiroEstimatedCacheUsage(result, &req, "stable-conversation-id")
	require.Greater(t, result.CacheReadInputTokens, int64(0))
	require.Less(t, result.CacheReadInputTokens, total)
	fresh := estimateKiroFreshInputTokens(&req)
	require.Equal(t, total-fresh, result.CacheReadInputTokens)
}

func TestApplyKiroEstimatedCacheUsage_FirstTurnNoCache(t *testing.T) {
	req := kiroOpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []kiroOpenAIMessage{
			{Role: "user", Content: mustKiroRawJSON("hello only")},
		},
	}
	result := &KiroChatResult{InputTokens: estimateKiroTokens(req)}
	applyKiroEstimatedCacheUsage(result, &req, "stable-conversation-id")
	require.Zero(t, result.CacheReadInputTokens)
	require.Zero(t, result.CacheCreationInputTokens)
}

func TestApplyKiroEstimatedCacheUsage_NoConversationID(t *testing.T) {
	req := kiroOpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []kiroOpenAIMessage{
			{Role: "user", Content: mustKiroRawJSON("hello")},
			{Role: "assistant", Content: mustKiroRawJSON("hi")},
			{Role: "user", Content: mustKiroRawJSON("again")},
		},
	}
	result := &KiroChatResult{InputTokens: estimateKiroTokens(req)}
	applyKiroEstimatedCacheUsage(result, &req, "")
	require.Zero(t, result.CacheReadInputTokens)
}

func TestApplyKiroEstimatedCacheUsage_DoesNotOverrideUpstream(t *testing.T) {
	req := kiroOpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []kiroOpenAIMessage{
			{Role: "user", Content: mustKiroRawJSON("hello")},
			{Role: "assistant", Content: mustKiroRawJSON("hi")},
			{Role: "user", Content: mustKiroRawJSON("again")},
		},
	}
	result := &KiroChatResult{InputTokens: 100, CacheReadInputTokens: 42}
	applyKiroEstimatedCacheUsage(result, &req, "stable")
	require.Equal(t, int64(42), result.CacheReadInputTokens)
}

func mustKiroRawJSON(s string) json.RawMessage {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return b
}
