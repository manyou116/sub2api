package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

// Responses serves OpenAI /v1/responses clients by bridging to Kiro's chat
// generateAssistantResponse path (same completeness model as OpenAI
// force-chat-completions fallback: tools, stream, usage).
func (s *KiroChatService) Responses(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	conversationID string,
) (*KiroChatResult, error) {
	if account == nil || !account.IsKiro() {
		return nil, fmt.Errorf("kiro: account is not a Kiro platform account")
	}
	if prev := strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()); prev != "" {
		writeKiroResponsesError(c, http.StatusBadRequest, "invalid_request_error",
			"previous_response_id is not supported for Kiro groups on HTTP; send full input history instead")
		return nil, fmt.Errorf("kiro responses: previous_response_id not supported")
	}

	var responsesReq apicompat.ResponsesRequest
	if err := json.Unmarshal(body, &responsesReq); err != nil {
		writeKiroResponsesError(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return nil, fmt.Errorf("kiro responses: parse body: %w", err)
	}
	originalModel := strings.TrimSpace(responsesReq.Model)
	if originalModel == "" {
		writeKiroResponsesError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return nil, fmt.Errorf("kiro responses: missing model")
	}

	effectiveTools, err := apicompat.EffectiveResponsesTools(&responsesReq)
	if err != nil {
		writeKiroResponsesError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, fmt.Errorf("kiro responses: resolve tools: %w", err)
	}
	customTools := apicompat.CustomToolNames(effectiveTools)
	toolSearch := apicompat.HasToolSearchTool(effectiveTools)
	namespaceTools := apicompat.NamespaceToolNames(effectiveTools)

	chatReq, err := apicompat.ResponsesToChatCompletionsRequest(&responsesReq)
	if err != nil {
		writeKiroResponsesError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, fmt.Errorf("kiro responses: convert to chat: %w", err)
	}
	clientStream := chatReq.Stream
	if clientStream {
		chatReq.StreamOptions = &apicompat.ChatStreamOptions{IncludeUsage: true}
	}

	chatBody, err := marshalKiroChatBodyFromCompat(chatReq)
	if err != nil {
		return nil, err
	}

	var openaiReq kiroOpenAIRequest
	if err := json.Unmarshal(chatBody, &openaiReq); err != nil {
		writeKiroResponsesError(c, http.StatusBadRequest, "invalid_request_error", "invalid converted chat request")
		return nil, fmt.Errorf("kiro responses: parse converted chat: %w", err)
	}
	openaiReq.Stream = clientStream
	openaiReq.Model = originalModel

	internalModel := resolveKiroInternalModel(account, originalModel)
	payload, err := buildKiroPayload(&openaiReq, internalModel, account.KiroProfileArn(), conversationID)
	if err != nil {
		writeKiroResponsesError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, err
	}
	body2, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("kiro responses: marshal payload: %w", err)
	}

	resp, startedAt, _, err := s.doKiroGenerate(ctx, account, body2)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	result := &KiroChatResult{
		UpstreamModel:   originalModel,
		InternalModel:   internalModel,
		Stream:          clientStream,
		UpstreamStatus:  resp.StatusCode,
		UpstreamHeaders: resp.Header.Clone(),
		InputTokens:     estimateKiroTokens(openaiReq),
	}
	defer func() { result.Duration = time.Since(startedAt) }()

	if clientStream {
		if err := s.streamKiroAsResponses(c, resp.Body, originalModel, startedAt, result, customTools, toolSearch, namespaceTools); err != nil {
			return result, err
		}
	} else {
		if err := s.aggregateKiroAsResponses(c, resp.Body, originalModel, startedAt, result, customTools, toolSearch, namespaceTools); err != nil {
			return result, err
		}
	}
	if result.OutputTokens == 0 {
		result.OutputTokens = approxTokensFromText(result.AssembledContent)
	}
	return result, nil
}

func marshalKiroChatBodyFromCompat(chatReq *apicompat.ChatCompletionsRequest) ([]byte, error) {
	if chatReq == nil {
		return nil, fmt.Errorf("kiro responses: chat request is nil")
	}
	// Prefer max_completion_tokens for Responses→Chat conversion.
	if chatReq.MaxCompletionTokens != nil && chatReq.MaxTokens == nil {
		v := *chatReq.MaxCompletionTokens
		chatReq.MaxTokens = &v
	}
	chatBody, err := json.Marshal(chatReq)
	if err != nil {
		return nil, fmt.Errorf("kiro responses: marshal chat request: %w", err)
	}
	// kiroOpenAIRequest only understands max_tokens.
	if gjson.GetBytes(chatBody, "max_completion_tokens").Exists() && !gjson.GetBytes(chatBody, "max_tokens").Exists() {
		chatBody, err = sjson.SetBytes(chatBody, "max_tokens", gjson.GetBytes(chatBody, "max_completion_tokens").Int())
		if err != nil {
			return nil, err
		}
	}
	return chatBody, nil
}

func (s *KiroChatService) streamKiroAsResponses(
	c *gin.Context,
	body io.Reader,
	model string,
	startedAt time.Time,
	result *KiroChatResult,
	customTools map[string]bool,
	toolSearch bool,
	namespaceTools map[string]apicompat.NamespacedToolName,
) error {
	c.Writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.WriteHeader(http.StatusOK)
	flusher, _ := c.Writer.(http.Flusher)

	state := apicompat.NewChatCompletionsToResponsesStreamState(model)
	state.CustomTools = customTools
	state.ToolSearchDeclared = toolSearch
	state.NamespaceTools = namespaceTools

	clientDisconnected := false
	writeEvents := func(events []apicompat.ResponsesStreamEvent) {
		if clientDisconnected || len(events) == 0 {
			return
		}
		for _, event := range events {
			sse, err := apicompat.ResponsesEventToSSE(event)
			if err != nil {
				logger.L().Warn("kiro responses: marshal stream event failed", zap.Error(err))
				continue
			}
			if _, err := fmt.Fprint(c.Writer, sse); err != nil {
				clientDisconnected = true
				return
			}
		}
		if flusher != nil {
			flusher.Flush()
		}
	}

	err := emitKiroChatChunks(body, model, startedAt, result, func(chunk *apicompat.ChatCompletionsChunk) error {
		writeEvents(apicompat.ChatCompletionsChunkToResponsesEvents(chunk, state))
		return nil
	})
	if err != nil {
		// Codex/SDK clients require a Responses terminal event (response.failed),
		// not a bare event:error frame.
		if !clientDisconnected {
			failEvt := apicompat.ResponsesStreamEvent{
				Type: "response.failed",
				Response: &apicompat.ResponsesResponse{
					ID:     state.ResponseID,
					Object: "response",
					Model:  model,
					Status: "failed",
					Output: []apicompat.ResponsesOutput{},
					Error: &apicompat.ResponsesError{
						Code:    "upstream_error",
						Message: err.Error(),
					},
				},
			}
			if failEvt.Response.ID == "" {
				failEvt.Response.ID = "resp_" + uuid.NewString()
			}
			if sse, mErr := apicompat.ResponsesEventToSSE(failEvt); mErr == nil {
				_, _ = fmt.Fprint(c.Writer, sse)
			}
			_, _ = fmt.Fprint(c.Writer, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		return err
	}

	writeEvents(apicompat.FinalizeChatCompletionsResponsesStream(state))
	if !clientDisconnected {
		_, _ = fmt.Fprint(c.Writer, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
	return nil
}

func (s *KiroChatService) aggregateKiroAsResponses(
	c *gin.Context,
	body io.Reader,
	model string,
	startedAt time.Time,
	result *KiroChatResult,
	customTools map[string]bool,
	toolSearch bool,
	namespaceTools map[string]apicompat.NamespacedToolName,
) error {
	ccResp, err := buildKiroChatCompletionsResponse(body, model, startedAt, result)
	if err != nil {
		writeKiroResponsesError(c, http.StatusBadGateway, "upstream_error", err.Error())
		return err
	}
	responsesResp := apicompat.ChatCompletionsResponseToResponses(ccResp, model, customTools, toolSearch, namespaceTools)
	c.JSON(http.StatusOK, responsesResp)
	return nil
}

// emitKiroChatChunks turns Kiro EventStream frames into OpenAI Chat Completions
// chunks (role / content / tool_calls / finish / usage).
func emitKiroChatChunks(
	body io.Reader,
	model string,
	startedAt time.Time,
	result *KiroChatResult,
	onChunk func(*apicompat.ChatCompletionsChunk) error,
) error {
	if onChunk == nil {
		onChunk = func(*apicompat.ChatCompletionsChunk) error { return nil }
	}
	chunkID := "chatcmpl-" + uuid.NewString()
	created := time.Now().Unix()
	var assembled strings.Builder

	markFirst := func() {
		if result == nil || result.FirstTokenMs != nil {
			return
		}
		ms := int(time.Since(startedAt).Milliseconds())
		result.FirstTokenMs = &ms
	}

	type toolAcc struct {
		index int
		name  string
		args  strings.Builder
	}
	toolByID := map[string]*toolAcc{}
	var toolOrder []string
	nextIdx := 0

	// role chunk
	role := "assistant"
	if err := onChunk(&apicompat.ChatCompletionsChunk{
		ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []apicompat.ChatChunkChoice{{
			Index: 0,
			Delta: apicompat.ChatDelta{Role: role},
		}},
	}); err != nil {
		return err
	}

	err := readKiroFrames(body, func(payload []byte) error {
		ev := extractKiroDelta(payload)
		applyKiroFrameUsage(result, ev)

		if ev.ToolUseID != "" {
			acc, ok := toolByID[ev.ToolUseID]
			if !ok {
				acc = &toolAcc{index: nextIdx}
				nextIdx++
				toolByID[ev.ToolUseID] = acc
				toolOrder = append(toolOrder, ev.ToolUseID)
			}
			if ev.ToolName != "" && acc.name == "" {
				acc.name = ev.ToolName
				markFirst()
				idx := acc.index
				chunk := &apicompat.ChatCompletionsChunk{
					ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: model,
					Choices: []apicompat.ChatChunkChoice{{
						Index: 0,
						Delta: apicompat.ChatDelta{
							ToolCalls: []apicompat.ChatToolCall{{
								Index: &idx,
								ID:    ev.ToolUseID,
								Type:  "function",
								Function: apicompat.ChatFunctionCall{
									Name:      ev.ToolName,
									Arguments: "",
								},
							}},
						},
					}},
				}
				return onChunk(chunk)
			}
			if ev.ToolInputDelta != "" {
				markFirst()
				_, _ = acc.args.WriteString(ev.ToolInputDelta)
				idx := acc.index
				chunk := &apicompat.ChatCompletionsChunk{
					ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: model,
					Choices: []apicompat.ChatChunkChoice{{
						Index: 0,
						Delta: apicompat.ChatDelta{
							ToolCalls: []apicompat.ChatToolCall{{
								Index: &idx,
								Function: apicompat.ChatFunctionCall{
									Arguments: ev.ToolInputDelta,
								},
							}},
						},
					}},
				}
				return onChunk(chunk)
			}
			return nil
		}

		if ev.Text == "" {
			return nil
		}
		markFirst()
		_, _ = assembled.WriteString(ev.Text)
		text := ev.Text
		return onChunk(&apicompat.ChatCompletionsChunk{
			ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []apicompat.ChatChunkChoice{{
				Index: 0,
				Delta: apicompat.ChatDelta{Content: &text},
			}},
		})
	})
	if err != nil {
		return err
	}

	if result != nil {
		result.AssembledContent = assembled.String()
		if result.OutputTokens == 0 {
			result.OutputTokens = approxTokensFromText(result.AssembledContent)
		}
	}

	finish := "stop"
	if len(toolOrder) > 0 {
		finish = "tool_calls"
	}
	if err := onChunk(&apicompat.ChatCompletionsChunk{
		ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []apicompat.ChatChunkChoice{{
			Index:        0,
			Delta:        apicompat.ChatDelta{},
			FinishReason: &finish,
		}},
	}); err != nil {
		return err
	}

	// usage-only trailing chunk
	usage := kiroChatUsage(result)
	return onChunk(&apicompat.ChatCompletionsChunk{
		ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []apicompat.ChatChunkChoice{},
		Usage:   usage,
	})
}

func buildKiroChatCompletionsResponse(
	body io.Reader,
	model string,
	startedAt time.Time,
	result *KiroChatResult,
) (*apicompat.ChatCompletionsResponse, error) {
	var assembled strings.Builder
	type toolAcc struct {
		name string
		args strings.Builder
	}
	toolByID := map[string]*toolAcc{}
	var toolOrder []string

	err := readKiroFrames(body, func(payload []byte) error {
		ev := extractKiroDelta(payload)
		applyKiroFrameUsage(result, ev)
		if result != nil && result.FirstTokenMs == nil && (ev.Text != "" || ev.ToolUseID != "") {
			ms := int(time.Since(startedAt).Milliseconds())
			result.FirstTokenMs = &ms
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
		return nil, err
	}

	if result != nil {
		result.AssembledContent = assembled.String()
		if result.OutputTokens == 0 {
			result.OutputTokens = approxTokensFromText(result.AssembledContent)
		}
	}

	content := assembled.String()
	contentRaw, _ := json.Marshal(content)
	msg := apicompat.ChatMessage{
		Role:    "assistant",
		Content: contentRaw,
	}
	finish := "stop"
	if len(toolOrder) > 0 {
		finish = "tool_calls"
		calls := make([]apicompat.ChatToolCall, 0, len(toolOrder))
		for _, id := range toolOrder {
			acc := toolByID[id]
			args := acc.args.String()
			if args == "" {
				args = "{}"
			}
			calls = append(calls, apicompat.ChatToolCall{
				ID:   id,
				Type: "function",
				Function: apicompat.ChatFunctionCall{
					Name:      acc.name,
					Arguments: args,
				},
			})
		}
		msg.ToolCalls = calls
		if content == "" {
			msg.Content = json.RawMessage("null")
		}
	}

	return &apicompat.ChatCompletionsResponse{
		ID:      "chatcmpl-" + uuid.NewString(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []apicompat.ChatChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: finish,
		}},
		Usage: kiroChatUsage(result),
	}, nil
}

func kiroChatUsage(result *KiroChatResult) *apicompat.ChatUsage {
	if result == nil {
		return nil
	}
	usage := &apicompat.ChatUsage{
		PromptTokens:     int(result.InputTokens),
		CompletionTokens: int(result.OutputTokens),
		TotalTokens:      int(result.InputTokens + result.OutputTokens),
	}
	if result.CacheReadInputTokens > 0 || result.CacheCreationInputTokens > 0 {
		usage.PromptTokensDetails = &apicompat.ChatTokenDetails{
			CachedTokens:        int(result.CacheReadInputTokens),
			CacheCreationTokens: int(result.CacheCreationInputTokens),
		}
	}
	return usage
}

func writeKiroResponsesError(c *gin.Context, status int, errType, message string) {
	if c == nil {
		return
	}
	c.JSON(status, gin.H{
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}
