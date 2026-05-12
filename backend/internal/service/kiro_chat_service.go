// Package service - Kiro Chat Completions adapter.
//
// 把 OpenAI /v1/chat/completions 请求转换为 Kiro CodeWhisperer
// `generateAssistantResponse` 接口调用，并把上游 AWS EventStream 响应
// 拼装回 OpenAI 兼容的流式 / 非流式 JSON。
//
// 该 service 是 MVP 实现，覆盖：
//   - 文本 messages（system / user / assistant）
//   - 模型名映射（gpt-* / claude-* / 其他 → Kiro internal id）
//   - 流式 SSE chunk 输出（OpenAI delta 格式）
//   - 非流式聚合输出
//   - usage 事件提取（input/output tokens）
//   - 失败 → UpstreamFailoverError，触发上层切换账号
//
// 暂未实现（后续迭代）：
//   - tool_calls / function_calls
//   - 图片 / 多模态 input
//   - prompt caching、reasoning content、citations
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

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
	UpstreamModel    string
	InternalModel    string
	Stream           bool
	InputTokens      int64
	OutputTokens     int64
	FirstTokenMs     *int
	UpstreamStatus   int
	UpstreamHeaders  http.Header
	AssembledContent string
}

// KiroChatService 转发 OpenAI Chat Completions 到 Kiro CodeWhisperer。
type KiroChatService struct {
	tokenSvc      *KiroTokenService
	tokenProvider *KiroTokenProvider // 可选：提供 access_token 自动刷新；nil 时回退到 account.KiroAccessToken()
}

// NewKiroChatService 构造服务。
func NewKiroChatService() *KiroChatService {
	return &KiroChatService{tokenSvc: NewKiroTokenService()}
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

// isKiroAuthError 判断上游响应是否为可通过 refresh 解决的 token 失效错误
func isKiroAuthError(status int, body []byte) bool {
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		return false
	}
	low := strings.ToLower(string(body))
	// Kiro 上游典型 401/403 token 失效响应特征：
	//   "bearer token included in the request is invalid"
	//   "ExpiredTokenException"
	//   "The security token included in the request is expired"
	//   "InvalidSignatureException" (token 被服务端拒签)
	return strings.Contains(low, "invalid bearer token") ||
		strings.Contains(low, "bearer token") ||
		strings.Contains(low, "expiredtoken") ||
		strings.Contains(low, "expired token") ||
		strings.Contains(low, "invalidsignature") ||
		strings.Contains(low, "token expired") ||
		strings.Contains(low, "unauthorized")
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

// KiroAvailableModel 描述一个 UI 可见的 Kiro 测试模型。
type KiroAvailableModel struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
}

// KiroDefaultModels 提供 admin 测试 / /v1/models 列表使用的 Kiro 模型集合。
// 仅暴露 Kiro 内部 ID（claude-sonnet-4 系列等），避免 UI 选了 Anthropic 公开 ID 触发 INVALID_MODEL_ID。
var KiroDefaultModels = []KiroAvailableModel{
	{ID: "claude-sonnet-4", Type: "model", DisplayName: "Claude Sonnet 4 (Kiro)"},
	{ID: "claude-sonnet-4.5", Type: "model", DisplayName: "Claude Sonnet 4.5 (Kiro)"},
	{ID: "claude-sonnet-4.6", Type: "model", DisplayName: "Claude Sonnet 4.6 (Kiro)"},
	{ID: "claude-haiku-4.5", Type: "model", DisplayName: "Claude Haiku 4.5 (Kiro)"},
	{ID: "claude-opus-4.5", Type: "model", DisplayName: "Claude Opus 4.5 (Kiro)"},
	{ID: "claude-opus-4.6", Type: "model", DisplayName: "Claude Opus 4.6 (Kiro)"},
	{ID: "claude-opus-4.7", Type: "model", DisplayName: "Claude Opus 4.7 (Kiro)"},
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
func buildKiroPayload(req *kiroOpenAIRequest, modelID, profileArn string) (*kiroPayload, error) {
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("kiro: messages is empty")
	}

	convoID := uuid.NewString()

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

	// 把 normalized msgs 折叠：连续的 tool → 合并到下一个 user 的 toolResults
	type collapsed struct {
		role        string
		content     string
		toolCalls   []kiroOpenAIToolCall
		toolResults []kiroToolResult
	}
	var items []collapsed
	var pendingResults []kiroToolResult
	for _, m := range msgs {
		switch m.role {
		case "tool":
			pendingResults = append(pendingResults, kiroToolResult{
				ToolUseID: m.toolUseID,
				Status:    "success",
				Content:   []kiroToolResultContent{{Text: m.content}},
			})
		case "user":
			items = append(items, collapsed{role: "user", content: m.content, toolResults: pendingResults})
			pendingResults = nil
		case "assistant":
			items = append(items, collapsed{role: "assistant", content: m.content, toolCalls: m.toolCalls})
		}
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
		var input map[string]any
		if c.Function.Arguments != "" {
			_ = json.Unmarshal([]byte(c.Function.Arguments), &input)
		}
		if input == nil {
			input = map[string]any{}
		}
		out = append(out, kiroToolUse{
			ToolUseID: c.ID,
			Name:      c.Function.Name,
			Input:     input,
		})
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
) (*KiroChatResult, error) {
	if account == nil || !account.IsKiro() {
		return nil, fmt.Errorf("kiro: account is not a Kiro platform account")
	}

	var req kiroOpenAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeKiroOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "request body is not valid JSON")
		return nil, fmt.Errorf("kiro: parse body: %w", err)
	}
	internalModel := MapKiroModel(req.Model)
	payload, err := buildKiroPayload(&req, internalModel, account.KiroProfileArn())
	if err != nil {
		writeKiroOpenAIError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, err
	}

	body2, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("kiro: marshal payload: %w", err)
	}

	// 把发给上游的真实 payload 写入 ops context，便于 admin 后台 debug
	// "Improperly formed request" 等上游错误（kiro_chat_service.go:606+）
	setOpsUpstreamRequestBody(c, body2)

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
		Timeout:            kiroChatHTTPTimeout,
		ValidateResolvedIP: true,
	})
	if err != nil {
		return nil, fmt.Errorf("kiro: build http client: %w", err)
	}

	// doRequest 用给定的 access_token 发起一次上游调用。
	// 返回的 *http.Response 调用方负责关闭 body。
	doRequest := func(accessToken string) (*http.Response, error) {
		httpReq, herr := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body2))
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
		return nil, &UpstreamFailoverError{
			StatusCode:             http.StatusBadGateway,
			ResponseBody:           []byte(err.Error()),
			RetryableOnSameAccount: false,
		}
	}

	// 401/403 invalid_token 兜底：主动 ForceRefresh 后用同账号重试一次。
	// 只在注入了 provider 且首次失败时尝试。
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
					return nil, &UpstreamFailoverError{
						StatusCode:             http.StatusBadGateway,
						ResponseBody:           []byte(err.Error()),
						RetryableOnSameAccount: false,
					}
				}
			} else {
				// refresh 失败：返回原始 401/403，让上层 quarantine + 切账号
				return nil, &UpstreamFailoverError{
					StatusCode:      resp.StatusCode,
					ResponseBody:    peekBody,
					ResponseHeaders: resp.Header.Clone(),
				}
			}
		} else {
			// 非 token 失效类的 401/403（账号被封等），直接走切号路径
			return nil, &UpstreamFailoverError{
				StatusCode:      resp.StatusCode,
				ResponseBody:    peekBody,
				ResponseHeaders: resp.Header.Clone(),
			}
		}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		logger.L().Warn("kiro_chat.upstream_error",
			zap.Int64("account_id", account.ID),
			zap.Int("status", resp.StatusCode),
			zap.String("body", truncateBody(respBody)),
		)
		return nil, &UpstreamFailoverError{
			StatusCode:      resp.StatusCode,
			ResponseBody:    respBody,
			ResponseHeaders: resp.Header.Clone(),
		}
	}

	result := &KiroChatResult{
		UpstreamModel:   req.Model,
		InternalModel:   internalModel,
		Stream:          req.Stream,
		UpstreamStatus:  resp.StatusCode,
		UpstreamHeaders: resp.Header.Clone(),
		// 入参 token 估算（Kiro 上游不提供 token usage，只给 credit）
		InputTokens: estimateKiroTokens(req),
	}

	if req.Stream {
		if err := s.streamToOpenAISSE(c, resp.Body, req.Model, startedAt, result); err != nil {
			return result, err
		}
	} else {
		if err := s.aggregateToOpenAIJSON(c, resp.Body, req.Model, result); err != nil {
			return result, err
		}
	}

	// 输出 token 估算
	if result.OutputTokens == 0 {
		result.OutputTokens = approxTokensFromText(result.AssembledContent)
	}

	return result, nil
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
	Text         string
	InputTokens  int64
	OutputTokens int64

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
		if iv, ok := u["inputTokens"].(float64); ok {
			ev.InputTokens = int64(iv)
		}
		if ov, ok := u["outputTokens"].(float64); ok {
			ev.OutputTokens = int64(ov)
		}
		if iv, ok := u["input_tokens"].(float64); ok && ev.InputTokens == 0 {
			ev.InputTokens = int64(iv)
		}
		if ov, ok := u["output_tokens"].(float64); ok && ev.OutputTokens == 0 {
			ev.OutputTokens = int64(ov)
		}
	}
	return ev
}

// streamToOpenAISSE 实时把 Kiro 事件转成 OpenAI ChatCompletion chunk SSE。
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

	chunkID := "chatcmpl-" + uuid.NewString()
	created := time.Now().Unix()
	var firstTokenLatency *int
	var assembled strings.Builder

	// tool_call 聚合（KAM 协议：start 拿 name → 多帧 input delta → stop）。
	type toolAcc struct {
		index int
		name  string
		args  strings.Builder
	}
	toolByID := map[string]*toolAcc{}
	var toolOrder []string
	nextIdx := 0

	// 首个 chunk 带 role
	first := openaiChunk(chunkID, model, created, map[string]any{"role": "assistant"}, nil)
	writeKiroSSEData(c.Writer, first)
	if flusher != nil {
		flusher.Flush()
	}

	err := readKiroFrames(body, func(payload []byte) error {
		ev := extractKiroDelta(payload)
		if ev.InputTokens > 0 {
			result.InputTokens = ev.InputTokens
		}
		if ev.OutputTokens > 0 {
			result.OutputTokens = ev.OutputTokens
		}

		// 工具事件
		if ev.ToolUseID != "" {
			acc, ok := toolByID[ev.ToolUseID]
			if !ok {
				acc = &toolAcc{index: nextIdx}
				nextIdx++
				toolByID[ev.ToolUseID] = acc
				toolOrder = append(toolOrder, ev.ToolUseID)
			}
			// start 帧（带 name）→ 发起 tool_call delta
			if ev.ToolName != "" && acc.name == "" {
				acc.name = ev.ToolName
				delta := map[string]any{
					"tool_calls": []map[string]any{{
						"index":    acc.index,
						"id":       ev.ToolUseID,
						"type":     "function",
						"function": map[string]any{"name": ev.ToolName, "arguments": ""},
					}},
				}
				writeKiroSSEData(c.Writer, openaiChunk(chunkID, model, created, delta, nil))
				if flusher != nil {
					flusher.Flush()
				}
			}
			if ev.ToolInputDelta != "" {
				_, _ = acc.args.WriteString(ev.ToolInputDelta)
				delta := map[string]any{
					"tool_calls": []map[string]any{{
						"index":    acc.index,
						"function": map[string]any{"arguments": ev.ToolInputDelta},
					}},
				}
				writeKiroSSEData(c.Writer, openaiChunk(chunkID, model, created, delta, nil))
				if flusher != nil {
					flusher.Flush()
				}
			}
			return nil
		}

		if ev.Text == "" {
			return nil
		}
		if firstTokenLatency == nil {
			ms := int(time.Since(startedAt).Milliseconds())
			firstTokenLatency = &ms
			result.FirstTokenMs = &ms
		}
		_, _ = assembled.WriteString(ev.Text)
		chunk := openaiChunk(chunkID, model, created, map[string]any{"content": ev.Text}, nil)
		writeKiroSSEData(c.Writer, chunk)
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

	// 收尾 chunk —— 如果有 tool_calls，finish_reason 用 tool_calls
	finishReason := "stop"
	if len(toolOrder) > 0 {
		finishReason = "tool_calls"
	}
	stop := openaiChunk(chunkID, model, created, nil, kiroStrPtr(finishReason))
	writeKiroSSEData(c.Writer, stop)

	// usage chunk（参照 OpenAI stream_options.include_usage 协议）
	result.AssembledContent = assembled.String()
	if result.OutputTokens == 0 {
		result.OutputTokens = approxTokensFromText(result.AssembledContent)
	}
	usageChunk := map[string]any{
		"id":      chunkID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{},
		"usage": map[string]any{
			"prompt_tokens":     result.InputTokens,
			"completion_tokens": result.OutputTokens,
			"total_tokens":      result.InputTokens + result.OutputTokens,
		},
	}
	if usageBytes, err := json.Marshal(usageChunk); err == nil {
		writeKiroSSEData(c.Writer, usageBytes)
	}

	_, _ = c.Writer.Write([]byte("data: [DONE]\n\n"))
	if flusher != nil {
		flusher.Flush()
	}

	return nil
}

// aggregateToOpenAIJSON 非流式：聚合所有 text 后一次性返回 OpenAI ChatCompletion JSON。
func (s *KiroChatService) aggregateToOpenAIJSON(
	c *gin.Context,
	body io.Reader,
	model string,
	result *KiroChatResult,
) error {
	var assembled strings.Builder
	type toolAcc struct {
		name string
		args strings.Builder
	}
	toolByID := map[string]*toolAcc{}
	var toolOrder []string

	err := readKiroFrames(body, func(payload []byte) error {
		ev := extractKiroDelta(payload)
		if ev.InputTokens > 0 {
			result.InputTokens = ev.InputTokens
		}
		if ev.OutputTokens > 0 {
			result.OutputTokens = ev.OutputTokens
		}
		if ev.ToolUseID != "" {
			acc, ok := toolByID[ev.ToolUseID]
			if !ok {
				acc = &toolAcc{}
				toolByID[ev.ToolUseID] = acc
				toolOrder = append(toolOrder, ev.ToolUseID)
			}
			if ev.ToolName != "" {
				acc.name = ev.ToolName
			}
			if ev.ToolInputDelta != "" {
				_, _ = acc.args.WriteString(ev.ToolInputDelta)
			}
			return nil
		}
		if ev.Text != "" {
			_, _ = assembled.WriteString(ev.Text)
		}
		return nil
	})
	if err != nil {
		writeKiroOpenAIError(c, http.StatusBadGateway, "upstream_error", err.Error())
		return err
	}
	result.AssembledContent = assembled.String()
	if result.OutputTokens == 0 {
		result.OutputTokens = approxTokensFromText(result.AssembledContent)
	}

	message := map[string]any{
		"role":    "assistant",
		"content": assembled.String(),
	}
	finishReason := "stop"
	if len(toolOrder) > 0 {
		finishReason = "tool_calls"
		var calls []map[string]any
		for _, id := range toolOrder {
			acc := toolByID[id]
			args := acc.args.String()
			if args == "" {
				args = "{}"
			}
			calls = append(calls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      acc.name,
					"arguments": args,
				},
			})
		}
		message["tool_calls"] = calls
		// OpenAI 规范：当返回 tool_calls 时 content 可以是空串
		if assembled.Len() == 0 {
			message["content"] = nil
		}
	}

	resp := map[string]any{
		"id":      "chatcmpl-" + uuid.NewString(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
		"usage": map[string]any{
			"prompt_tokens":     result.InputTokens,
			"completion_tokens": result.OutputTokens,
			"total_tokens":      result.InputTokens + result.OutputTokens,
		},
	}
	c.JSON(http.StatusOK, resp)
	return nil
}

// ============== Helpers ==============

func openaiChunk(id, model string, created int64, delta map[string]any, finishReason *string) []byte {
	choice := map[string]any{"index": 0, "delta": map[string]any{}}
	if delta != nil {
		choice["delta"] = delta
	}
	if finishReason != nil {
		choice["finish_reason"] = *finishReason
	}
	out := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{choice},
	}
	b, _ := json.Marshal(out)
	return b
}

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

func kiroStrPtr(s string) *string { return &s }

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	if len(b) >= 2 {
		return string(b[1 : len(b)-1])
	}
	return s
}
