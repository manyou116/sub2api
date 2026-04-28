package openaiimages

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// ResponseSink 是 writer 与 gin.Context 之间的最小抽象，便于单测注入。
//
// 实现要求：
//   - SetHeader / WriteStatus 必须在第一次 Write 之前调用；
//   - SSE 实现每次 Write 之后必须 Flush。
type ResponseSink interface {
	io.Writer
	SetHeader(key, value string)
	WriteStatus(code int)
	Flush()
}

// WriteOptions 由 dispatch 在调用 writer 前组装。
type WriteOptions struct {
	// ResponseID 用于回填 chat / responses 入口的 id 字段；为空则自动生成。
	ResponseID string
	// ClientModel 是客户端发送的 model（chat/responses 透传需要原值，而不是 driver 上游真实 model）。
	ClientModel string
}

// WriteStandard 把 ImageResult 写成 OpenAI /v1/images/* 标准 JSON 响应。
//
// 输出 schema:
//
//	{ "created": <unix>, "data": [ {"b64_json": "..."} | {"url": "..."} | { both, "revised_prompt": "..." } ] }
func WriteStandard(sink ResponseSink, req *ImagesRequest, res *ImageResult, _ WriteOptions) error {
	created := normalizedCreated(res)
	data := make([]map[string]any, 0, len(res.Items))
	for _, it := range res.Items {
		entry := map[string]any{}
		if it.B64JSON != "" {
			entry["b64_json"] = it.B64JSON
		}
		if it.URL != "" {
			entry["url"] = it.URL
		}
		if it.RevisedPrompt != "" {
			entry["revised_prompt"] = it.RevisedPrompt
		}
		data = append(data, entry)
	}
	body := map[string]any{
		"created": created,
		"data":    data,
	}
	if req != nil && req.Background != "" {
		body["background"] = req.Background
	}
	return writeJSON(sink, 200, body)
}

// WriteChatSync 把 ImageResult 写成 /v1/chat/completions 非流式响应。
func WriteChatSync(sink ResponseSink, req *ImagesRequest, res *ImageResult, opts WriteOptions) error {
	id := nonEmpty(opts.ResponseID, "chatcmpl-"+randomID())
	model := nonEmpty(opts.ClientModel, res.Model)
	created := normalizedCreated(res)
	content := RenderMarkdown(promptOf(req), res.Items)

	body := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": content,
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     res.Usage.InputTokens,
			"completion_tokens": res.Usage.OutputTokens,
			"total_tokens":      res.Usage.TotalTokens,
		},
	}
	return writeJSON(sink, 200, body)
}

// WriteChatSSE 流式输出 chat.completion.chunk 序列。
//
// 帧序列：role delta → content delta → finish_reason=stop → usage chunk → [DONE]
func WriteChatSSE(sink ResponseSink, req *ImagesRequest, res *ImageResult, opts WriteOptions) error {
	id := nonEmpty(opts.ResponseID, "chatcmpl-"+randomID())
	model := nonEmpty(opts.ClientModel, res.Model)
	created := normalizedCreated(res)
	content := RenderMarkdown(promptOf(req), res.Items)

	prepareSSEHeaders(sink)

	frame := func(delta map[string]any, finishReason any) map[string]any {
		ch := map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason}
		return map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]any{ch},
		}
	}

	if err := writeSSEData(sink, frame(map[string]any{"role": "assistant"}, nil)); err != nil {
		return err
	}
	if err := writeSSEData(sink, frame(map[string]any{"content": content}, nil)); err != nil {
		return err
	}
	if err := writeSSEData(sink, frame(map[string]any{}, "stop")); err != nil {
		return err
	}
	usageFrame := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]any{},
		"usage": map[string]any{
			"prompt_tokens":     res.Usage.InputTokens,
			"completion_tokens": res.Usage.OutputTokens,
			"total_tokens":      res.Usage.TotalTokens,
		},
	}
	if err := writeSSEData(sink, usageFrame); err != nil {
		return err
	}
	return writeSSERaw(sink, "data: [DONE]\n\n")
}

// WriteResponsesSync 写 /v1/responses 非流式响应。
func WriteResponsesSync(sink ResponseSink, req *ImagesRequest, res *ImageResult, opts WriteOptions) error {
	id := nonEmpty(opts.ResponseID, "resp_"+randomID())
	model := nonEmpty(opts.ClientModel, res.Model)
	created := normalizedCreated(res)
	content := RenderMarkdown(promptOf(req), res.Items)

	body := map[string]any{
		"id":         id,
		"object":     "response",
		"created_at": created,
		"model":      model,
		"status":     "completed",
		"output": []map[string]any{{
			"id":   "msg_" + randomID(),
			"type": "message",
			"role": "assistant",
			"content": []map[string]any{{
				"type": "output_text",
				"text": content,
			}},
		}},
		"usage": map[string]any{
			"input_tokens":  res.Usage.InputTokens,
			"output_tokens": res.Usage.OutputTokens,
			"total_tokens":  res.Usage.TotalTokens,
		},
	}
	return writeJSON(sink, 200, body)
}

// WriteResponsesSSE 流式输出 Responses API 事件序列。
//
// 事件序列：response.created → response.output_item.added → response.output_text.delta
// → response.output_text.done → response.output_item.done → response.completed → [DONE]
func WriteResponsesSSE(sink ResponseSink, req *ImagesRequest, res *ImageResult, opts WriteOptions) error {
	id := nonEmpty(opts.ResponseID, "resp_"+randomID())
	model := nonEmpty(opts.ClientModel, res.Model)
	itemID := "msg_" + randomID()
	created := normalizedCreated(res)
	content := RenderMarkdown(promptOf(req), res.Items)

	prepareSSEHeaders(sink)

	respObj := func(status string, output []map[string]any) map[string]any {
		body := map[string]any{
			"id":         id,
			"object":     "response",
			"created_at": created,
			"model":      model,
			"status":     status,
			"output":     output,
		}
		if status == "completed" {
			body["usage"] = map[string]any{
				"input_tokens":  res.Usage.InputTokens,
				"output_tokens": res.Usage.OutputTokens,
				"total_tokens":  res.Usage.TotalTokens,
			}
		}
		return body
	}

	emit := func(event string, payload map[string]any) error {
		payload["type"] = event
		return writeSSEEvent(sink, event, payload)
	}

	if err := emit("response.created", map[string]any{"response": respObj("in_progress", []map[string]any{})}); err != nil {
		return err
	}
	if err := emit("response.output_item.added", map[string]any{
		"output_index": 0,
		"item": map[string]any{
			"id": itemID, "type": "message", "role": "assistant",
			"content": []map[string]any{},
		},
	}); err != nil {
		return err
	}
	if err := emit("response.output_text.delta", map[string]any{
		"output_index": 0, "content_index": 0, "item_id": itemID, "delta": content,
	}); err != nil {
		return err
	}
	if err := emit("response.output_text.done", map[string]any{
		"output_index": 0, "content_index": 0, "item_id": itemID, "text": content,
	}); err != nil {
		return err
	}
	if err := emit("response.output_item.done", map[string]any{
		"output_index": 0,
		"item": map[string]any{
			"id": itemID, "type": "message", "role": "assistant",
			"content": []map[string]any{{"type": "output_text", "text": content}},
		},
	}); err != nil {
		return err
	}
	finalOutput := []map[string]any{{
		"id": itemID, "type": "message", "role": "assistant",
		"content": []map[string]any{{"type": "output_text", "text": content}},
	}}
	if err := emit("response.completed", map[string]any{"response": respObj("completed", finalOutput)}); err != nil {
		return err
	}
	return writeSSERaw(sink, "data: [DONE]\n\n")
}

// --- Helpers ---

func writeJSON(sink ResponseSink, status int, body any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	sink.SetHeader("Content-Type", "application/json; charset=utf-8")
	sink.WriteStatus(status)
	if _, err := sink.Write(buf); err != nil {
		return err
	}
	return nil
}

func prepareSSEHeaders(sink ResponseSink) {
	sink.SetHeader("Content-Type", "text/event-stream")
	sink.SetHeader("Cache-Control", "no-cache")
	sink.SetHeader("Connection", "keep-alive")
	sink.SetHeader("X-Accel-Buffering", "no")
	sink.WriteStatus(200)
}

func writeSSEData(sink ResponseSink, payload any) error {
	buf, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(sink, "data: %s\n\n", buf); err != nil {
		return err
	}
	sink.Flush()
	return nil
}

func writeSSEEvent(sink ResponseSink, event string, payload any) error {
	buf, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(sink, "event: %s\ndata: %s\n\n", event, buf); err != nil {
		return err
	}
	sink.Flush()
	return nil
}

func writeSSERaw(sink ResponseSink, line string) error {
	if _, err := io.WriteString(sink, line); err != nil {
		return err
	}
	sink.Flush()
	return nil
}

func normalizedCreated(res *ImageResult) int64 {
	if res != nil && res.Created > 0 {
		return res.Created
	}
	return time.Now().Unix()
}

func promptOf(req *ImagesRequest) string {
	if req == nil {
		return ""
	}
	return req.Prompt
}

func nonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func randomID() string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}
