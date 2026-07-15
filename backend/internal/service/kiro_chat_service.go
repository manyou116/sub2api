// Package service - Kiro Chat Completions adapter (P5).
//
// OpenAI /v1/chat/completions <-> Kiro CodeWhisperer generateAssistantResponse:
//   - text messages (system / user / assistant / tool)
//   - model map (client id -> Kiro internal id)
//   - tools / tool_calls (function tools)
//   - stream SSE (OpenAI delta) and non-stream JSON
//   - usage extraction (upstream when present; else estimate)
//   - ProbeTextStream for admin account tests
//   - UpstreamFailoverError for account switch
//
// OpenAI /v1/responses is bridged in kiro_responses_service.go (apicompat).
// Out of scope: vision/multimodal, reasoning/citations, Anthropic Messages,
// Responses subpaths (compact), Responses WebSocket.
//
// Failover / quarantine: see kiro_error_classifier.go + kiro_quarantine.go.
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/Wei-Shaw/sub2api/internal/pkg/kiroeventstream"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	kiroGenerateEndpointTmpl = "https://q.%s.amazonaws.com/generateAssistantResponse"
	kiroChatHTTPTimeout      = 5 * time.Minute
	kiroAgentMode            = "vibe"
)

// KiroChatResult 是单次转发的产物，handler 用来记账。
type KiroChatResult struct {
	UpstreamModel            string
	InternalModel            string
	Stream                   bool
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
	Duration                 time.Duration
	FirstTokenMs             *int
	UpstreamStatus           int
	UpstreamHeaders          http.Header
	AssembledContent         string
}

// KiroChatService 转发 OpenAI Chat Completions 到 Kiro CodeWhisperer。
type KiroChatService struct {
	tokenProvider *KiroTokenProvider // 可选：access_token 自动刷新；nil 时用 account.KiroAccessToken()
}

// NewKiroChatService 构造服务。
func NewKiroChatService() *KiroChatService {
	return &KiroChatService{}
}

// SetTokenProvider 注入 token provider，启用 access_token 自动刷新与 401/403 兜底重试。
// 通常由 wire 在构造 handler 时调用一次。
func (s *KiroChatService) SetTokenProvider(p *KiroTokenProvider) {
	s.tokenProvider = p
}

// resolveAccessToken 返回当前应使用的 access_token。
// 若注入了 provider，则按需 refresh；否则回退到 account 字段。
func (s *KiroChatService) resolveAccessToken(ctx context.Context, account *Account) (string, *Account, error) {
	if s.tokenProvider == nil {
		return account.KiroAccessToken(), account, nil
	}
	tok, err := s.tokenProvider.EnsureFreshToken(ctx, account)
	if err != nil {
		// refresh 失败时回退到现有 token，让上游自己报错（可能仍未过期）；
		// 401/403 阶段会触发 ForceRefresh 兜底。
		logger.L().Warn("kiro_chat.token_ensure_failed_fallback",
			zap.Int64("account_id", account.ID),
			zap.Error(err),
		)
		return account.KiroAccessToken(), account, nil
	}
	return tok, account, nil
}

// isKiroAuthError 判断上游响应是否为可通过 refresh 解决的 token 失效错误。
// 复用 ClassifyKiroError，避免与 quarantine 分类逻辑漂移（旧实现里 "unauthorized"
// 过宽，会把 AccessDenied 误当成可 refresh 的 auth 错误）。
func isKiroAuthError(status int, body []byte) bool {
	return ClassifyKiroError(status, body) == KiroErrAuth
}

// ============== 模型映射 ==============

var openaiToKiroModelMap = map[string]string{
	// gpt-5.x → Kiro Claude
	"gpt-5.5":            "claude-opus-4.7",
	"gpt-5.5-pro":        "claude-opus-4.7",
	"gpt-5.4":            "claude-opus-4.6",
	"gpt-5.4-pro":        "claude-opus-4.6",
	"gpt-5.4-mini":       "claude-sonnet-4.6",
	"gpt-5.4-nano":       "claude-sonnet-4.5",
	"gpt-5":              "claude-opus-4.5",
	"gpt-5-pro":          "claude-opus-4.5",
	"gpt-5-mini":         "claude-sonnet-4.5",
	"gpt-5-nano":         "claude-sonnet-4.5",
	"gpt-5.3-codex":      "claude-opus-4.6",
	"gpt-5.2-codex":      "claude-opus-4.5",
	"gpt-5.1-codex":      "claude-opus-4.5",
	"gpt-5.1-codex-max":  "claude-opus-4.5",
	"gpt-5.1-codex-mini": "claude-sonnet-4.5",
	"gpt-5-codex":        "claude-sonnet-4.5",

	// claude direct
	"claude-opus-4.7":           "claude-opus-4.7",
	"claude-opus-4.7-thinking":  "claude-opus-4.7",
	"claude-opus-4.6":           "claude-opus-4.6",
	"claude-opus-4.6-thinking":  "claude-opus-4.6",
	"claude-sonnet-4.6":         "claude-sonnet-4.6",
	"claude-opus-4.5":           "claude-opus-4.5",
	"claude-sonnet-4.5":         "claude-sonnet-4.5",
	"claude-sonnet-latest":      "claude-sonnet-4.5",
	"claude-haiku-4.5":          "claude-haiku-4.5",
	"claude-haiku-4.5-thinking": "claude-haiku-4.5",
	"claude-sonnet-4":           "claude-sonnet-4",

	// open-source / vendor models (Kiro 直通)
	"deepseek-3.2":     "deepseek-3.2",
	"deepseek":         "deepseek-3.2",
	"minimax-m2.5":     "minimax-m2.5",
	"minimax-m2.1":     "minimax-m2.1",
	"glm-5":            "glm-5",
	"qwen3-coder-next": "qwen3-coder-next",
	"qwen3-coder":      "qwen3-coder-next",
	"qwen3":            "qwen3-coder-next",
	"auto":             "auto",
}

// MapKiroModel 把 OpenAI 模型名映射为 Kiro 上游 internal id；未命中走前缀兜底。
func MapKiroModel(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if v, ok := openaiToKiroModelMap[n]; ok {
		return v
	}
	switch {
	case strings.HasPrefix(n, "gpt-5.5"):
		return "claude-opus-4.7"
	case strings.HasPrefix(n, "gpt-5.4"):
		return "claude-opus-4.6"
	case strings.HasPrefix(n, "gpt-5"):
		return "claude-opus-4.5"
	case strings.HasPrefix(n, "claude-opus-4-7"), strings.HasPrefix(n, "claude-opus-4.7"):
		return "claude-opus-4.7"
	case strings.HasPrefix(n, "claude-opus-4-6"), strings.HasPrefix(n, "claude-opus-4.6"):
		return "claude-opus-4.6"
	case strings.HasPrefix(n, "claude-opus-4-5"), strings.HasPrefix(n, "claude-opus-4.5"):
		return "claude-opus-4.5"
	case strings.HasPrefix(n, "claude-sonnet-4-6"), strings.HasPrefix(n, "claude-sonnet-4.6"):
		return "claude-sonnet-4.6"
	case strings.HasPrefix(n, "claude-sonnet-4-5"), strings.HasPrefix(n, "claude-sonnet-4.5"):
		return "claude-sonnet-4.5"
	case strings.HasPrefix(n, "claude-haiku-4-5"), strings.HasPrefix(n, "claude-haiku-4.5"):
		return "claude-haiku-4.5"
	}
	// 兜底：直传，让上游自行决定
	return n
}

func resolveKiroInternalModel(account *Account, requestedModel string) string {
	if account != nil {
		requestedModel = account.GetMappedModel(requestedModel)
	}
	return MapKiroModel(requestedModel)
}

// KiroAvailableModel 描述一个 UI 可见的 Kiro 测试模型。
type KiroAvailableModel struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
}

// KiroDefaultModels 提供 admin 测试 / /v1/models 列表使用的 Kiro 模型集合。
// 仅暴露 Kiro 内部 ID（claude-sonnet-4 系列等），避免 UI 选了 Anthropic 公开 ID 触发 INVALID_MODEL_ID。
var KiroDefaultModels = []KiroAvailableModel{
	{ID: "auto", Type: "model", DisplayName: "Auto (Kiro)"},
	{ID: "claude-sonnet-4", Type: "model", DisplayName: "Claude Sonnet 4 (Kiro)"},
	{ID: "claude-sonnet-4.5", Type: "model", DisplayName: "Claude Sonnet 4.5 (Kiro)"},
	{ID: "claude-sonnet-4.6", Type: "model", DisplayName: "Claude Sonnet 4.6 (Kiro)"},
	{ID: "claude-haiku-4.5", Type: "model", DisplayName: "Claude Haiku 4.5 (Kiro)"},
	{ID: "claude-opus-4.5", Type: "model", DisplayName: "Claude Opus 4.5 (Kiro)"},
	{ID: "claude-opus-4.6", Type: "model", DisplayName: "Claude Opus 4.6 (Kiro)"},
	{ID: "claude-opus-4.7", Type: "model", DisplayName: "Claude Opus 4.7 (Kiro)"},
	{ID: "deepseek-3.2", Type: "model", DisplayName: "DeepSeek 3.2 (Kiro)"},
	{ID: "glm-5", Type: "model", DisplayName: "GLM 5 (Kiro)"},
	{ID: "minimax-m2.1", Type: "model", DisplayName: "MiniMax M2.1 (Kiro)"},
	{ID: "minimax-m2.5", Type: "model", DisplayName: "MiniMax M2.5 (Kiro)"},
	{ID: "qwen3-coder-next", Type: "model", DisplayName: "Qwen3 Coder Next (Kiro)"},
}

// ============== Payload 构造 ==============

type kiroOpenAIMessage struct {
	Role       string               `json:"role"`
	Content    json.RawMessage      `json:"content"`
	ToolCalls  []kiroOpenAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string               `json:"tool_call_id,omitempty"`
	Name       string               `json:"name,omitempty"`
}

type kiroOpenAIToolCall struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	Function kiroOpenAIToolCallFunc `json:"function"`
}

type kiroOpenAIToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type kiroOpenAIRequest struct {
	Model     string              `json:"model"`
	Messages  []kiroOpenAIMessage `json:"messages"`
	Stream    bool                `json:"stream"`
	MaxTokens int                 `json:"max_tokens,omitempty"`
	Tools     []kiroOpenAITool    `json:"tools,omitempty"`
}

// kiroOpenAITool 对应 OpenAI tool 定义。仅支持 type=function。
type kiroOpenAITool struct {
	Type     string             `json:"type"`
	Function kiroOpenAIToolFunc `json:"function"`
}

type kiroOpenAIToolFunc struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type kiroPayload struct {
	ConversationState kiroConversationState `json:"conversationState"`
	ProfileArn        string                `json:"profileArn,omitempty"`
}

type kiroConversationState struct {
	ChatTriggerType     string             `json:"chatTriggerType"`
	ConversationID      string             `json:"conversationId"`
	AgentContinuationID string             `json:"agentContinuationId"`
	AgentTaskType       string             `json:"agentTaskType"`
	CurrentMessage      kiroCurrentMessage `json:"currentMessage"`
	History             []kiroHistoryItem  `json:"history,omitempty"`
}

type kiroCurrentMessage struct {
	UserInputMessage kiroUserMessage `json:"userInputMessage"`
}

type kiroUserMessage struct {
	Content                 string                       `json:"content"`
	ModelID                 string                       `json:"modelId"`
	Origin                  string                       `json:"origin"`
	UserInputMessageContext *kiroUserInputMessageContext `json:"userInputMessageContext,omitempty"`
}

// kiroUserInputMessageContext 携带 tools 与 toolResults。Kiro 协议要求工具放在
// user 消息的 context 而不是顶层。
type kiroUserInputMessageContext struct {
	Tools       []kiroToolWrapper `json:"tools,omitempty"`
	ToolResults []kiroToolResult  `json:"toolResults,omitempty"`
}

// kiroToolWrapper 对应 KAM 中 KiroTool::ToolSpecification 的序列化形式：
// { "toolSpecification": { "name", "description", "inputSchema": { "json": {...} } } }
type kiroToolWrapper struct {
	ToolSpecification kiroToolSpec `json:"toolSpecification"`
}

type kiroToolSpec struct {
	Name        string              `json:"name"`
	Description string              `json:"description"`
	InputSchema kiroToolInputSchema `json:"inputSchema"`
}

type kiroToolInputSchema struct {
	JSON map[string]any `json:"json"`
}

type kiroToolResult struct {
	ToolUseID string                  `json:"toolUseId"`
	Status    string                  `json:"status"`
	Content   []kiroToolResultContent `json:"content"`
}

type kiroToolResultContent struct {
	Text string `json:"text,omitempty"`
}

type kiroAssistantMessage struct {
	Content  string        `json:"content"`
	ToolUses []kiroToolUse `json:"toolUses,omitempty"`
}

type kiroToolUse struct {
	ToolUseID string         `json:"toolUseId"`
	Name      string         `json:"name"`
	Input     map[string]any `json:"input"`
}

type kiroHistoryItem struct {
	UserInputMessage         *kiroUserMessage      `json:"userInputMessage,omitempty"`
	AssistantResponseMessage *kiroAssistantMessage `json:"assistantResponseMessage,omitempty"`
}

// extractTextContent 从 OpenAI message.content 提取纯文本（兼容 string 和 array of parts）。
func extractKiroTextContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []map[string]any
	if err := json.Unmarshal(raw, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			t, _ := p["type"].(string)
			if t == "text" || t == "input_text" || t == "output_text" {
				if txt, ok := p["text"].(string); ok {
					_, _ = sb.WriteString(txt)
				}
			}
		}
		return sb.String()
	}
	return ""
}

// buildKiroPayload 把 OpenAI request 转成 Kiro payload。
// modelID 已映射，profileArn 为空表示 IdC 流程。
func buildKiroPayload(req *kiroOpenAIRequest, modelID, profileArn, conversationID string) (*kiroPayload, error) {
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("kiro: messages is empty")
	}

	convoID := strings.TrimSpace(conversationID)
	if convoID == "" {
		convoID = uuid.NewString()
	}

	var systemPrompt strings.Builder
	type normMsg struct {
		role      string
		content   string
		toolCalls []kiroOpenAIToolCall // assistant 上一轮发出的 tool 调用
		toolUseID string               // role=tool 时对应的 id
	}
	var msgs []normMsg
	for _, m := range req.Messages {
		text := extractKiroTextContent(m.Content)
		switch m.Role {
		case "system":
			if text == "" {
				continue
			}
			if systemPrompt.Len() > 0 {
				_, _ = systemPrompt.WriteString("\n\n")
			}
			_, _ = systemPrompt.WriteString(text)
		case "user":
			if text == "" {
				// content 可能全是 image / 其他非文本 part；不能 drop（会破坏对话顺序），
				// 给一个占位字符让 CodeWhisperer 校验通过
				text = " "
			}
			msgs = append(msgs, normMsg{role: "user", content: text})
		case "assistant":
			// assistant 可能有 tool_calls 而没有 content
			if text == "" && len(m.ToolCalls) == 0 {
				continue
			}
			if text == "" {
				// 仅有 tool_calls 时给占位字符，避免上游空 content 拒绝
				text = " "
			}
			msgs = append(msgs, normMsg{role: "assistant", content: text, toolCalls: m.ToolCalls})
		case "tool":
			// 工具结果：折叠到下一个 user 消息的 toolResults 中
			msgs = append(msgs, normMsg{role: "tool", content: text, toolUseID: m.ToolCallID})
		}
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("kiro: no user/assistant content after normalization")
	}

	// 工具结果只能挂在 user 消息上：若最后一条是 tool（没有后续 user），
	// 合成一个 user 容纳 toolResults（CodeWhisperer 拒绝空 content，给一个占位字符）
	if msgs[len(msgs)-1].role == "tool" {
		msgs = append(msgs, normMsg{role: "user", content: " "})
	}

	// 把 system prompt 拼到首个 user 消息前
	if systemPrompt.Len() > 0 {
		for i, m := range msgs {
			if m.role == "user" {
				if msgs[i].content != "" {
					msgs[i].content = systemPrompt.String() + "\n\n" + m.content
				} else {
					msgs[i].content = systemPrompt.String()
				}
				break
			}
		}
	}

	// 把工具定义转 Kiro 形式
	toolWrappers := convertKiroTools(req.Tools)

	// 把 normalized msgs 折叠：连续的 tool → 合并到「紧随其后」的 user 的 toolResults。
	// OpenAI 常见顺序是 assistant(tool_calls) → tool* → assistant(续写)。Kiro/Bedrock
	// 要求 toolResults 必须紧跟在对应 toolUses 的 assistant 之后，且数量/ID 对齐；
	// 若 tool 后直接是 assistant，必须先 flush 一个 synthetic user，否则 toolResults
	// 会被错误挂到更后面的 user 上触发 TOOL_USE_RESULT_MISMATCH。
	type collapsed struct {
		role        string
		content     string
		toolCalls   []kiroOpenAIToolCall
		toolResults []kiroToolResult
	}
	var items []collapsed
	var pendingResults []kiroToolResult
	flushPendingToolResults := func(content string) {
		if len(pendingResults) == 0 {
			return
		}
		if content == "" {
			content = " "
		}
		items = append(items, collapsed{role: "user", content: content, toolResults: pendingResults})
		pendingResults = nil
	}
	for _, m := range msgs {
		switch m.role {
		case "tool":
			id := strings.TrimSpace(m.toolUseID)
			if id == "" {
				// 无 tool_call_id 的结果无法对齐 toolUse，丢弃以免上游 400。
				continue
			}
			pendingResults = append(pendingResults, kiroToolResult{
				ToolUseID: id,
				Status:    "success",
				Content:   []kiroToolResultContent{{Text: m.content}},
			})
		case "user":
			// 真实 user 消息吸收挂起的 toolResults。
			items = append(items, collapsed{role: "user", content: m.content, toolResults: pendingResults})
			pendingResults = nil
		case "assistant":
			// tool 结果后若紧跟 assistant（OpenAI 多轮工具续写），先合成 user 轮。
			flushPendingToolResults(" ")
			items = append(items, collapsed{role: "assistant", content: m.content, toolCalls: m.toolCalls})
		}
	}
	// 防御：末尾仍挂起的 toolResults 不应出现（上方已保证 last=user），再兜底一次。
	flushPendingToolResults(" ")

	// 严格对齐：每个带 toolResults 的 user，只保留上一轮 assistant.toolUses 里出现过的 id；
	// 且同一 id 只保留最后一次结果。多余/孤儿结果会触发 Bedrock TOOL_USE_RESULT_MISMATCH。
	for i := range items {
		if items[i].role != "user" || len(items[i].toolResults) == 0 {
			continue
		}
		allowed := map[string]struct{}{}
		if i > 0 && items[i-1].role == "assistant" {
			for _, tc := range items[i-1].toolCalls {
				if id := strings.TrimSpace(tc.ID); id != "" {
					allowed[id] = struct{}{}
				}
			}
		}
		if len(allowed) == 0 {
			items[i].toolResults = nil
			continue
		}
		// last-wins dedupe, then keep only allowed ids (preserve first-seen order of allowed set via scan)
		byID := map[string]kiroToolResult{}
		order := make([]string, 0, len(items[i].toolResults))
		for _, r := range items[i].toolResults {
			id := strings.TrimSpace(r.ToolUseID)
			if id == "" {
				continue
			}
			if _, ok := allowed[id]; !ok {
				continue
			}
			if _, seen := byID[id]; !seen {
				order = append(order, id)
			}
			byID[id] = r
		}
		filtered := make([]kiroToolResult, 0, len(order))
		for _, id := range order {
			filtered = append(filtered, byID[id])
		}
		items[i].toolResults = filtered
	}

	// last must be user
	if len(items) == 0 || items[len(items)-1].role != "user" {
		return nil, fmt.Errorf("kiro: last message must be from user")
	}
	current := items[len(items)-1]
	historyMsgs := items[:len(items)-1]

	// === 防御：CodeWhisperer 严格校验，空 content 直接 400 "Improperly formed request" ===
	// 凡是要发上游的字符串字段，最少给 1 个空格占位
	const blankPlaceholder = " "
	if current.content == "" {
		current.content = blankPlaceholder
	}
	for i := range historyMsgs {
		if historyMsgs[i].content == "" {
			historyMsgs[i].content = blankPlaceholder
		}
	}

	currentCtx := buildKiroUserCtx(toolWrappers, current.toolResults)

	var history []kiroHistoryItem
	for _, m := range historyMsgs {
		switch m.role {
		case "user":
			history = append(history, kiroHistoryItem{
				UserInputMessage: &kiroUserMessage{
					Content:                 m.content,
					ModelID:                 modelID,
					Origin:                  "AI_EDITOR",
					UserInputMessageContext: buildKiroUserCtx(nil, m.toolResults),
				},
			})
		case "assistant":
			history = append(history, kiroHistoryItem{
				AssistantResponseMessage: &kiroAssistantMessage{
					Content:  m.content,
					ToolUses: convertKiroToolCalls(m.toolCalls),
				},
			})
		}
	}

	return &kiroPayload{
		ConversationState: kiroConversationState{
			ChatTriggerType:     "MANUAL",
			ConversationID:      convoID,
			AgentContinuationID: convoID,
			AgentTaskType:       kiroAgentMode,
			CurrentMessage: kiroCurrentMessage{
				UserInputMessage: kiroUserMessage{
					Content:                 current.content,
					ModelID:                 modelID,
					Origin:                  "AI_EDITOR",
					UserInputMessageContext: currentCtx,
				},
			},
			History: history,
		},
		ProfileArn: profileArn,
	}, nil
}

// convertKiroTools 把 OpenAI tools 转成 Kiro toolSpecification 列表。
func convertKiroTools(tools []kiroOpenAITool) []kiroToolWrapper {
	if len(tools) == 0 {
		return nil
	}
	out := make([]kiroToolWrapper, 0, len(tools))
	for _, t := range tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		params := t.Function.Parameters
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		desc := t.Function.Description
		if desc == "" {
			// Kiro 协议要求 description 非空，否则上游 400。
			desc = t.Function.Name
		}
		out = append(out, kiroToolWrapper{
			ToolSpecification: kiroToolSpec{
				Name:        t.Function.Name,
				Description: desc,
				InputSchema: kiroToolInputSchema{JSON: params},
			},
		})
	}
	return out
}

// convertKiroToolCalls 把 OpenAI assistant.tool_calls 转 Kiro toolUses（解析 arguments JSON）。
func convertKiroToolCalls(calls []kiroOpenAIToolCall) []kiroToolUse {
	if len(calls) == 0 {
		return nil
	}
	out := make([]kiroToolUse, 0, len(calls))
	for _, c := range calls {
		id := strings.TrimSpace(c.ID)
		if id == "" {
			continue
		}
		var input map[string]any
		if c.Function.Arguments != "" {
			_ = json.Unmarshal([]byte(c.Function.Arguments), &input)
		}
		if input == nil {
			input = map[string]any{}
		}
		out = append(out, kiroToolUse{
			ToolUseID: id,
			Name:      c.Function.Name,
			Input:     input,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// buildKiroUserCtx 仅在有内容时返回非 nil。
func buildKiroUserCtx(tools []kiroToolWrapper, results []kiroToolResult) *kiroUserInputMessageContext {
	if len(tools) == 0 && len(results) == 0 {
		return nil
	}
	return &kiroUserInputMessageContext{Tools: tools, ToolResults: results}
}

// ============== 上游调用 ==============

// doKiroGenerate POSTs a marshaled Kiro payload to generateAssistantResponse.
// On success the caller owns resp.Body. Non-2xx / transport failures return
// *UpstreamFailoverError so gateway and account-test share one auth-retry path.
func (s *KiroChatService) doKiroGenerate(
	ctx context.Context,
	account *Account,
	payloadBody []byte,
) (*http.Response, time.Time, *Account, error) {
	if account == nil {
		return nil, time.Time{}, nil, fmt.Errorf("kiro: account is nil")
	}

	region := account.KiroRegion()
	if region == "" {
		region = KiroDefaultRegion
	}
	endpoint := fmt.Sprintf(kiroGenerateEndpointTmpl, region)

	machineID := account.KiroMachineID()
	if machineID == "" {
		machineID = "sub2api"
	}
	ua := fmt.Sprintf(KiroIDEUserAgentTmpl, machineID)

	client, err := httpclient.GetClient(httpclient.Options{
		ProxyURL:           account.KiroProxyURL(),
		Timeout:            kiroChatHTTPTimeout,
		ValidateResolvedIP: true,
	})
	if err != nil {
		return nil, time.Time{}, account, fmt.Errorf("kiro: build http client: %w", err)
	}

	doRequest := func(accessToken string) (*http.Response, error) {
		httpReq, herr := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payloadBody))
		if herr != nil {
			return nil, fmt.Errorf("kiro: build request: %w", herr)
		}
		httpReq.Header.Set("Authorization", "Bearer "+accessToken)
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "application/vnd.amazon.eventstream")
		httpReq.Header.Set("X-Amz-User-Agent", ua)
		httpReq.Header.Set("User-Agent", ua)
		httpReq.Header.Set("amz-sdk-invocation-id", uuid.NewString())
		httpReq.Header.Set("amz-sdk-request", "attempt=1; max=3")
		httpReq.Header.Set("x-amzn-kiro-agent-mode", kiroAgentMode)
		if account.KiroProfileArn() != "" {
			httpReq.Header.Set("x-amzn-kiro-profile-arn", account.KiroProfileArn())
		}
		if strings.EqualFold(account.KiroProvider(), "Internal") {
			httpReq.Header.Set("redirect-for-internal", "true")
		}
		return client.Do(httpReq)
	}

	accessToken, account, _ := s.resolveAccessToken(ctx, account)
	startedAt := time.Now()
	resp, err := doRequest(accessToken)
	if err != nil {
		return nil, startedAt, account, &UpstreamFailoverError{
			StatusCode:             http.StatusBadGateway,
			ResponseBody:           []byte(err.Error()),
			RetryableOnSameAccount: false,
		}
	}

	// 401/403 invalid_token 兜底：ForceRefresh 后同账号重试一次。
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		peekBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if s.tokenProvider != nil && isKiroAuthError(resp.StatusCode, peekBody) {
			logger.L().Info("kiro_chat.auth_error_force_refresh",
				zap.Int64("account_id", account.ID),
				zap.Int("status", resp.StatusCode),
			)
			newToken, refreshedAccount, ferr := s.tokenProvider.ForceRefresh(ctx, account)
			if ferr == nil && newToken != "" {
				account = refreshedAccount
				resp, err = doRequest(newToken)
				if err != nil {
					return nil, startedAt, account, &UpstreamFailoverError{
						StatusCode:             http.StatusBadGateway,
						ResponseBody:           []byte(err.Error()),
						RetryableOnSameAccount: false,
					}
				}
			} else {
				return nil, startedAt, account, &UpstreamFailoverError{
					StatusCode:      resp.StatusCode,
					ResponseBody:    peekBody,
					ResponseHeaders: resp.Header.Clone(),
				}
			}
		} else {
			return nil, startedAt, account, &UpstreamFailoverError{
				StatusCode:      resp.StatusCode,
				ResponseBody:    peekBody,
				ResponseHeaders: resp.Header.Clone(),
			}
		}
	}

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		logger.L().Warn("kiro_chat.upstream_error",
			zap.Int64("account_id", account.ID),
			zap.Int("status", resp.StatusCode),
			zap.String("body", truncateBody(respBody)),
		)
		return nil, startedAt, account, &UpstreamFailoverError{
			StatusCode:      resp.StatusCode,
			ResponseBody:    respBody,
			ResponseHeaders: resp.Header.Clone(),
		}
	}

	return resp, startedAt, account, nil
}

// ChatCompletions 是 handler 调用入口：转发请求到 Kiro，输出到 c.Writer
// （流式 SSE）或返回完整 JSON（非流）。
//
// 失败语义：返回 *UpstreamFailoverError 触发上层切换账号；返回普通 error
// 表示已经向客户端写入响应、上层应停止链路。
func (s *KiroChatService) ChatCompletions(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	conversationID string,
) (*KiroChatResult, error) {
	if account == nil || !account.IsKiro() {
		return nil, fmt.Errorf("kiro: account is not a Kiro platform account")
	}

	var req kiroOpenAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeKiroOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "request body is not valid JSON")
		return nil, fmt.Errorf("kiro: parse body: %w", err)
	}
	internalModel := resolveKiroInternalModel(account, req.Model)
	payload, err := buildKiroPayload(&req, internalModel, account.KiroProfileArn(), conversationID)
	if err != nil {
		writeKiroOpenAIError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, err
	}

	body2, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("kiro: marshal payload: %w", err)
	}

	resp, startedAt, _, err := s.doKiroGenerate(ctx, account, body2)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	result := &KiroChatResult{
		UpstreamModel:   req.Model,
		InternalModel:   internalModel,
		Stream:          req.Stream,
		UpstreamStatus:  resp.StatusCode,
		UpstreamHeaders: resp.Header.Clone(),
		// 入参 token 估算（Kiro 上游不提供 token usage，只给 credit）
		InputTokens: estimateKiroTokens(req),
	}
	defer func() {
		result.Duration = time.Since(startedAt)
	}()

	// Estimate cache_read before writing the client body so both the OpenAI
	// usage payload and RecordUsage see the same split. Upstream frames may
	// still overwrite with real cache tokens via applyKiroFrameUsage (>0 only).
	applyKiroEstimatedCacheUsage(result, &req, conversationID)

	if req.Stream {
		if err := s.streamToOpenAISSE(c, resp.Body, req.Model, startedAt, result); err != nil {
			return result, err
		}
	} else {
		if err := s.aggregateToOpenAIJSON(c, resp.Body, req.Model, startedAt, result); err != nil {
			return result, err
		}
	}

	// 输出 token 估算（流式路径可能已从帧里写入）
	if result.OutputTokens == 0 {
		result.OutputTokens = approxTokensFromText(result.AssembledContent)
	}
	// Re-apply if upstream left cache at 0 (stream frames never set it).
	applyKiroEstimatedCacheUsage(result, &req, conversationID)

	return result, nil
}

// ProbeTextStream 是账号连通性测试用的轻量探针：上游 EventStream 每出一段文本
// 就回调 onDelta（admin SSE 可逐段 Flush）。不写 OpenAI 协议，避免测试路径
// 再走一次 stream=false 聚合。
func (s *KiroChatService) ProbeTextStream(
	ctx context.Context,
	account *Account,
	modelID string,
	prompt string,
	onDelta func(text string) error,
) (*KiroChatResult, error) {
	if account == nil || !account.IsKiro() {
		return nil, fmt.Errorf("kiro: account is not a Kiro platform account")
	}
	if onDelta == nil {
		onDelta = func(string) error { return nil }
	}

	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		modelID = "claude-sonnet-4.5"
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		prompt = "hi"
	}

	contentRaw, err := json.Marshal(prompt)
	if err != nil {
		return nil, fmt.Errorf("kiro: marshal probe prompt: %w", err)
	}
	req := kiroOpenAIRequest{
		Model:     modelID,
		Stream:    true,
		MaxTokens: 64,
		Messages: []kiroOpenAIMessage{{
			Role:    "user",
			Content: contentRaw,
		}},
	}
	internalModel := resolveKiroInternalModel(account, req.Model)
	payload, err := buildKiroPayload(&req, internalModel, account.KiroProfileArn(), "")
	if err != nil {
		return nil, err
	}
	body2, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("kiro: marshal payload: %w", err)
	}

	resp, startedAt, _, err := s.doKiroGenerate(ctx, account, body2)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	result := &KiroChatResult{
		UpstreamModel:   req.Model,
		InternalModel:   internalModel,
		Stream:          true,
		UpstreamStatus:  resp.StatusCode,
		UpstreamHeaders: resp.Header.Clone(),
		InputTokens:     estimateKiroTokens(req),
	}
	defer func() {
		result.Duration = time.Since(startedAt)
	}()

	var assembled strings.Builder
	err = readKiroFrames(resp.Body, func(frame []byte) error {
		ev := extractKiroDelta(frame)
		applyKiroFrameUsage(result, ev)
		if ev.Text == "" {
			return nil
		}
		if result.FirstTokenMs == nil {
			ms := int(time.Since(startedAt).Milliseconds())
			result.FirstTokenMs = &ms
		}
		_, _ = assembled.WriteString(ev.Text)
		return onDelta(ev.Text)
	})
	result.AssembledContent = assembled.String()
	if result.OutputTokens == 0 {
		result.OutputTokens = approxTokensFromText(result.AssembledContent)
	}
	if err != nil {
		return result, err
	}
	return result, nil
}

// applyKiroEstimatedCacheUsage fills cache_read when upstream omitted it but we
// have multi-turn history under a stable conversation id (prompt affinity).
// OpenAI-style InputTokens stay as the full prompt estimate; billing subtracts
// cache_read via actualInputTokens = input - cache_read - cache_creation.
func applyKiroEstimatedCacheUsage(result *KiroChatResult, req *kiroOpenAIRequest, conversationID string) {
	if result == nil || req == nil {
		return
	}
	if strings.TrimSpace(conversationID) == "" {
		return
	}
	if result.CacheReadInputTokens > 0 || result.CacheCreationInputTokens > 0 {
		return
	}
	if !kiroRequestHasPriorTurns(req) {
		return
	}
	total := result.InputTokens
	if total <= 0 {
		total = estimateKiroTokens(*req)
		result.InputTokens = total
	}
	fresh := estimateKiroFreshInputTokens(req)
	if fresh < 0 {
		fresh = 0
	}
	if fresh >= total {
		return
	}
	result.CacheReadInputTokens = total - fresh
}

// kiroRequestHasPriorTurns is true when the client already carries assistant/tool
// history (multi-turn), so conversation cache can apply to the prefix.
func kiroRequestHasPriorTurns(req *kiroOpenAIRequest) bool {
	if req == nil || len(req.Messages) < 2 {
		return false
	}
	for _, m := range req.Messages {
		switch strings.ToLower(strings.TrimSpace(m.Role)) {
		case "assistant", "tool":
			return true
		}
	}
	// Two+ user turns without assistant (rare) still share a long prefix.
	users := 0
	for _, m := range req.Messages {
		if strings.EqualFold(strings.TrimSpace(m.Role), "user") {
			users++
			if users >= 2 {
				return true
			}
		}
	}
	return false
}

// estimateKiroFreshInputTokens estimates tokens that are new this turn
// (last user message + trailing tool results), i.e. not reusable prefix.
func estimateKiroFreshInputTokens(req *kiroOpenAIRequest) int64 {
	if req == nil || len(req.Messages) == 0 {
		return 0
	}
	// Walk from the end: accumulate trailing tools, then stop at the final user.
	var fresh int64
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		role := strings.ToLower(strings.TrimSpace(m.Role))
		switch role {
		case "tool":
			fresh += estimateKiroMessageTokens(m)
		case "user":
			fresh += estimateKiroMessageTokens(m)
			return fresh
		default:
			// trailing assistant/system without a following user: nothing fresh
			return fresh
		}
	}
	return fresh
}

func estimateKiroMessageTokens(m kiroOpenAIMessage) int64 {
	var total int64
	if len(m.Content) > 0 {
		var s string
		if err := json.Unmarshal(m.Content, &s); err == nil {
			total += approxTokensFromText(s)
		} else {
			total += approxTokensFromText(string(m.Content))
		}
	}
	total += 4
	return total
}

// estimateKiroTokens 估算入参 messages 的总 token 数（粗略）。
func estimateKiroTokens(req kiroOpenAIRequest) int64 {
	var total int64
	for _, m := range req.Messages {
		if len(m.Content) > 0 {
			// content 是 raw json，先尝试当字符串展开，否则按原长度算
			var s string
			if err := json.Unmarshal(m.Content, &s); err == nil {
				total += approxTokensFromText(s)
			} else {
				total += approxTokensFromText(string(m.Content))
			}
		}
		total += 4 // role/分隔符开销
	}
	return total
}

// approxTokensFromText 粗略估 token：chars/2，至少 1（仅在文本非空时）。
func approxTokensFromText(s string) int64 {
	if s == "" {
		return 0
	}
	n := int64(len([]rune(s))) / 2
	if n < 1 {
		n = 1
	}
	return n
}

// ============== EventStream 处理 ==============

// readKiroFrames 持续从 reader 拉一帧一帧，回调每个 payload JSON 字符串。
func readKiroFrames(r io.Reader, onJSON func(jsonBytes []byte) error) error {
	buf := make([]byte, 0, 8192)
	chunk := make([]byte, 4096)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			for {
				msg, consumed, derr := kiroeventstream.Decode(buf)
				if derr != nil {
					return fmt.Errorf("kiro: eventstream decode: %w", derr)
				}
				if msg == nil {
					break
				}
				buf = buf[consumed:]
				if msg.Headers[":message-type"] == "exception" || msg.Headers[":message-type"] == "error" {
					return fmt.Errorf("kiro upstream exception: %s: %s", msg.Headers[":exception-type"], string(msg.Payload))
				}
				if len(msg.Payload) > 0 {
					if cberr := onJSON(msg.Payload); cberr != nil {
						return cberr
					}
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("kiro: read upstream: %w", err)
		}
	}
}

// kiroFrameEvent 描述一帧解出的结构化事件。
type kiroFrameEvent struct {
	Text                     string
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64

	// Tool use 帧（KAM 的协议有三种形态：start (有 name) / input delta / stop）
	ToolUseID      string
	ToolName       string
	ToolInputDelta string // 增量 JSON 文本片段
	ToolStop       bool
}

// extractKiroDelta 从一个 EventStream 子 JSON 里抽出文本/usage。
// Kiro CodeWhisperer 的 frame payload 直接是 {"content":"...","modelId":"..."}
// （事件类型由 EventStream 头 :event-type=assistantResponseEvent 标识，而非 JSON 里再包一层）。
func extractKiroDelta(payload []byte) kiroFrameEvent {
	var ev kiroFrameEvent
	var v map[string]any
	if err := json.Unmarshal(payload, &v); err != nil {
		return ev
	}
	// 顶层 content（assistantResponseEvent 帧）
	if c, ok := v["content"].(string); ok {
		ev.Text = c
	}
	// 兼容旧格式（如果未来 upstream 再嵌一层）
	if ev.Text == "" {
		if are, ok := v["assistantResponseEvent"].(map[string]any); ok {
			if c, ok := are["content"].(string); ok {
				ev.Text = c
			}
		}
	}

	// Tool use 帧
	if tid, ok := v["toolUseId"].(string); ok && tid != "" {
		ev.ToolUseID = tid
		if name, ok := v["name"].(string); ok {
			ev.ToolName = name
		}
		if stop, ok := v["stop"].(bool); ok && stop {
			ev.ToolStop = true
		}
		if input, ok := v["input"]; ok {
			switch t := input.(type) {
			case string:
				ev.ToolInputDelta = t
			case map[string]any, []any:
				if b, err := json.Marshal(t); err == nil {
					ev.ToolInputDelta = string(b)
				}
			}
		}
	}

	if u, ok := v["usage"].(map[string]any); ok {
		ev.InputTokens = firstKiroInt64(u, "inputTokens", "input_tokens", "promptTokens", "prompt_tokens")
		ev.OutputTokens = firstKiroInt64(u, "outputTokens", "output_tokens", "completionTokens", "completion_tokens")
		ev.CacheCreationInputTokens = firstKiroInt64(u, "cacheCreationInputTokens", "cache_creation_input_tokens", "cacheCreationTokens", "cache_creation_tokens")
		ev.CacheReadInputTokens = firstKiroInt64(u, "cacheReadInputTokens", "cache_read_input_tokens", "cachedTokens", "cached_tokens")
		if ev.CacheReadInputTokens == 0 {
			ev.CacheReadInputTokens = cachedTokensFromKiroUsageDetails(u, "inputTokensDetails", "input_tokens_details", "promptTokensDetails", "prompt_tokens_details")
		}
	}
	if ev.InputTokens == 0 {
		ev.InputTokens = firstKiroInt64(v, "inputTokens", "input_tokens", "promptTokens", "prompt_tokens")
	}
	if ev.OutputTokens == 0 {
		ev.OutputTokens = firstKiroInt64(v, "outputTokens", "output_tokens", "completionTokens", "completion_tokens")
	}
	if ev.CacheCreationInputTokens == 0 {
		ev.CacheCreationInputTokens = firstKiroInt64(v, "cacheCreationInputTokens", "cache_creation_input_tokens", "cacheCreationTokens", "cache_creation_tokens")
	}
	if ev.CacheReadInputTokens == 0 {
		ev.CacheReadInputTokens = firstKiroInt64(v, "cacheReadInputTokens", "cache_read_input_tokens", "cachedTokens", "cached_tokens")
	}
	return ev
}

func firstKiroInt64(values map[string]any, keys ...string) int64 {
	for _, key := range keys {
		if number := kiroInt64Value(values[key]); number > 0 {
			return number
		}
	}
	return 0
}

func cachedTokensFromKiroUsageDetails(values map[string]any, keys ...string) int64 {
	for _, key := range keys {
		details, ok := values[key].(map[string]any)
		if !ok || details == nil {
			continue
		}
		if number := firstKiroInt64(details, "cachedTokens", "cached_tokens"); number > 0 {
			return number
		}
	}
	return 0
}

func kiroInt64Value(value any) int64 {
	switch typedValue := value.(type) {
	case json.Number:
		if number, err := typedValue.Int64(); err == nil {
			return number
		}
		if floatNumber, err := typedValue.Float64(); err == nil {
			return int64(floatNumber)
		}
	case float64:
		return int64(typedValue)
	case int64:
		return typedValue
	case int:
		return int64(typedValue)
	case string:
		if number, err := strconv.ParseInt(strings.TrimSpace(typedValue), 10, 64); err == nil {
			return number
		}
	}
	return 0
}

func applyKiroFrameUsage(result *KiroChatResult, event kiroFrameEvent) {
	if result == nil {
		return
	}
	if event.InputTokens > 0 {
		result.InputTokens = event.InputTokens
	}
	if event.OutputTokens > 0 {
		result.OutputTokens = event.OutputTokens
	}
	if event.CacheCreationInputTokens > 0 {
		result.CacheCreationInputTokens = event.CacheCreationInputTokens
	}
	if event.CacheReadInputTokens > 0 {
		result.CacheReadInputTokens = event.CacheReadInputTokens
	}
}

// streamToOpenAISSE 实时把 Kiro 事件转成 OpenAI ChatCompletion chunk SSE。
// 帧解析与 tool_call 聚合共用 emitKiroChatChunks，保证 chat / responses 行为一致。
func (s *KiroChatService) streamToOpenAISSE(
	c *gin.Context,
	body io.Reader,
	model string,
	startedAt time.Time,
	result *KiroChatResult,
) error {
	c.Writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.WriteHeader(http.StatusOK)
	flusher, _ := c.Writer.(http.Flusher)

	err := emitKiroChatChunks(body, model, startedAt, result, func(chunk *apicompat.ChatCompletionsChunk) error {
		payload, mErr := json.Marshal(chunk)
		if mErr != nil {
			return mErr
		}
		writeKiroSSEData(c.Writer, payload)
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	})
	if err != nil {
		writeKiroSSEData(c.Writer, []byte(`{"error":{"type":"upstream_error","message":"`+jsonEscape(err.Error())+`"}}`))
		_, _ = c.Writer.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		return err
	}

	_, _ = c.Writer.Write([]byte("data: [DONE]\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}

// aggregateToOpenAIJSON 非流式：聚合 EventStream 后一次性返回 OpenAI ChatCompletion JSON。
func (s *KiroChatService) aggregateToOpenAIJSON(
	c *gin.Context,
	body io.Reader,
	model string,
	startedAt time.Time,
	result *KiroChatResult,
) error {
	ccResp, err := buildKiroChatCompletionsResponse(body, model, startedAt, result)
	if err != nil {
		writeKiroOpenAIError(c, http.StatusBadGateway, "upstream_error", err.Error())
		return err
	}
	c.JSON(http.StatusOK, ccResp)
	return nil
}

// ============== Helpers ==============

func writeKiroSSEData(w io.Writer, data []byte) {
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(data)
	_, _ = w.Write([]byte("\n\n"))
}

func writeKiroOpenAIError(c *gin.Context, status int, code, message string) {
	c.JSON(status, map[string]any{
		"error": map[string]any{
			"type":    code,
			"code":    code,
			"message": message,
		},
	})
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	if len(b) >= 2 {
		return string(b[1 : len(b)-1])
	}
	return s
}
