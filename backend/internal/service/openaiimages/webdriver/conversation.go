package webdriver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/imroc/req/v3"
	"github.com/tidwall/gjson"
)

// conversation.go：prepare → /f/conversation SSE → 兜底 polling。

var pointerRe = regexp.MustCompile(`(?:file-service|sediment)://[^"\s\]]+`)

// modelSlug 把 OpenAI 模型名映射到 ChatGPT Web 内部 slug。
func modelSlug(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if strings.Contains(m, "gpt-image-2") || strings.Contains(m, "gpt-5-3") {
		return "gpt-5-3"
	}
	return "auto"
}

// buildPrompt 在生成 / 编辑场景统一加上「直接出图」的前缀指令，
// 与 chatgpt2api `_build_image_prompt` 保持一致：不要让模型回答文字，
// 也不再叠加任何 "不要 echo 源图" 的负面指令（实测会导致模型反向 echo）。
func buildPrompt(userPrompt string, hasUploads bool) string {
	prompt := strings.TrimSpace(userPrompt)
	if prompt == "" && hasUploads {
		prompt = "Edit the attached image."
	}
	instruction := "Generate the image directly from the request. Do not answer with plain text, do not ask for more details, and make reasonable visual choices if details are missing."
	if prompt == "" {
		return instruction
	}
	return instruction + "\n\n" + prompt
}

func prepareConversation(
	ctx context.Context,
	client *req.Client,
	headers http.Header,
	baseURL string,
	prompt, parentMessageID, requirementsToken, proofToken, model string,
) (string, error) {
	h := cloneHTTPHeader(headers)
	h.Set("openai-sentinel-chat-requirements-token", requirementsToken)
	if proofToken != "" {
		h.Set("openai-sentinel-proof-token", proofToken)
	}
	payload := map[string]any{
		"action":                "next",
		"fork_from_shared_post": false,
		"parent_message_id":     uuid.NewString(),
		"model":                 modelSlug(model),
		"client_prepare_state":  "success",
		"timezone_offset_min":   tzOffsetMinutes(),
		"timezone":              tzName(),
		"conversation_mode":     map[string]any{"kind": "primary_assistant"},
		"system_hints":          []string{"picture_v2"},
		"partial_query": map[string]any{
			"id":     parentMessageID,
			"author": map[string]any{"role": "user"},
			"content": map[string]any{
				"content_type": "text",
				"parts":        []string{prompt},
			},
		},
		"supports_buffering":  true,
		"supported_encodings": []string{"v1"},
		"client_contextual_info": map[string]any{
			"app_name": "chatgpt.com",
		},
	}
	var result struct {
		ConduitToken string `json:"conduit_token"`
	}
	resp, err := client.R().
		SetContext(ctx).
		SetHeaders(headerToMap(h)).
		SetBodyJsonMarshal(payload).
		SetSuccessResult(&result).
		Post(baseURL)
	if err != nil {
		return "", &TransportError{Wrapped: fmt.Errorf("prepare conversation: %w", err)}
	}
	if !resp.IsSuccessState() {
		return "", classifyHTTPError(resp, "prepare conversation failed")
	}
	return strings.TrimSpace(result.ConduitToken), nil
}

func buildConversationPayload(model, prompt, parentMessageID string, uploads []uploadedFile) map[string]any {
	parts := []any{}
	attachments := []map[string]any{}
	for _, up := range uploads {
		parts = append(parts, map[string]any{
			"content_type":  "image_asset_pointer",
			"asset_pointer": "file-service://" + up.FileID,
			"size_bytes":    up.FileSize,
			"width":         up.Width,
			"height":        up.Height,
		})
		attachments = append(attachments, map[string]any{
			"id": up.FileID, "mimeType": up.ContentType, "name": up.FileName,
			"size": up.FileSize, "width": up.Width, "height": up.Height,
		})
	}
	parts = append(parts, prompt)

	contentType := "text"
	if len(uploads) > 0 {
		contentType = "multimodal_text"
	}
	metadata := map[string]any{
		"developer_mode_connector_ids": []any{},
		"selected_github_repos":        []any{},
		"selected_all_github_repos":    false,
		"system_hints":                 []string{"picture_v2"},
		"serialization_metadata":       map[string]any{"custom_symbol_offsets": []any{}},
	}
	if len(uploads) > 0 {
		metadata["attachments"] = attachments
	}
	messages := []map[string]any{{
		"id":          uuid.NewString(),
		"author":      map[string]any{"role": "user"},
		"create_time": float64(time.Now().UnixNano()) / 1e9,
		"content":     map[string]any{"content_type": contentType, "parts": parts},
		"metadata":    metadata,
	}}
	return map[string]any{
		"action":                               "next",
		"messages":                             messages,
		"parent_message_id":                    parentMessageID,
		"model":                                modelSlug(model),
		"client_prepare_state":                 "sent",
		"timezone_offset_min":                  tzOffsetMinutes(),
		"timezone":                             tzName(),
		"conversation_mode":                    map[string]any{"kind": "primary_assistant"},
		"enable_message_followups":             true,
		"system_hints":                         []string{"picture_v2"},
		"supports_buffering":                   true,
		"supported_encodings":                  []string{"v1"},
		"paragen_cot_summary_display_override": "allow",
		"force_parallel_switch":                "auto",
		"client_contextual_info": map[string]any{
			"is_dark_mode":      false,
			"time_since_loaded": 1200,
			"page_height":       1072,
			"page_width":        1724,
			"pixel_ratio":       1.2,
			"screen_height":     1440,
			"screen_width":      2560,
			"app_name":          "chatgpt.com",
		},
	}
}

// readSSE 解析 SSE 流，提取 conversation_id 与图片 pointer。
// allowEarlyExit=true 时，凑齐 expectedImages 张可下载 pointer 即返回。
func readSSE(
	resp *req.Response,
	startTime time.Time,
	expectedImages int,
	excludedPointers map[string]struct{},
	allowEarlyExit bool,
) (conversationID string, pointers []pointerInfo, firstTokenMs *int, earlyExit bool, err error) {
	if expectedImages < 1 {
		expectedImages = 1
	}
	reader := bufio.NewReader(resp.Body)
	for {
		line, rerr := reader.ReadBytes('\n')
		if len(line) > 0 {
			if firstTokenMs == nil {
				ms := int(time.Since(startTime).Milliseconds())
				firstTokenMs = &ms
			}
			text := strings.TrimRight(string(line), "\r\n")
			if data, ok := extractSSEDataLine(text); ok && data != "" && data != "[DONE]" {
				dataBytes := []byte(data)
				if id := gjson.GetBytes(dataBytes, "conversation_id").String(); id != "" {
					conversationID = id
				}
				pointers = append(pointers, collectPointers(dataBytes, excludedPointers)...)
				if t := extractAssistantText(dataBytes); looksLikeTextResponse(t) {
					return conversationID, pointers, firstTokenMs, false, &ProtocolError{
						Reason: "text response instead of image: " + truncate(t, 240), ConversationID: conversationID,
					}
				}
				if allowEarlyExit && conversationID != "" && countDownloadablePointers(pointers) >= expectedImages {
					go drainStream(resp.Body)
					return conversationID, pointers, firstTokenMs, true, nil
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return conversationID, pointers, firstTokenMs, false, &TransportError{Wrapped: rerr}
		}
	}
	return conversationID, pointers, firstTokenMs, false, nil
}

func drainStream(body io.ReadCloser) {
	if body == nil {
		return
	}
	defer body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 8<<20))
}

func collectPointers(body []byte, excluded map[string]struct{}) []pointerInfo {
	out := []pointerInfo{}
	for _, m := range pointerRe.FindAll(body, -1) {
		p := string(m)
		if _, ok := excluded[p]; ok {
			continue
		}
		out = append(out, pointerInfo{Pointer: p})
	}
	return out
}

func collectToolPointers(body []byte, excluded map[string]struct{}) []pointerInfo {
	// 优先按 author.role == "tool" 严格抽取（与 chatgpt2api `_extract_image_tool_records` 一致）。
	// 如果 body 不是完整的 conversation tree（比如 SSE 单帧），则回退到全文扫描。
	if ptrs, ok := collectToolPointersFromMapping(body, excluded); ok {
		return ptrs
	}
	return collectPointers(body, excluded)
}

// collectToolPointersFromMapping 从 conversation mapping 里只挑 author.role == "tool"
// 的消息中的 file-service:// / sediment:// 指针。返回 (pointers, mappingFound)。
func collectToolPointersFromMapping(body []byte, excluded map[string]struct{}) ([]pointerInfo, bool) {
	mapping := gjson.GetBytes(body, "mapping")
	if !mapping.Exists() || !mapping.IsObject() {
		return nil, false
	}
	seen := map[string]struct{}{}
	var out []pointerInfo
	mapping.ForEach(func(_, node gjson.Result) bool {
		role := node.Get("message.author.role").String()
		if role != "tool" {
			return true
		}
		// 收集这个 tool message 内所有 pointer
		raw := node.Get("message.content").Raw
		if raw == "" {
			return true
		}
		for _, m := range pointerRe.FindAllString(raw, -1) {
			if _, skip := excluded[m]; skip {
				continue
			}
			if _, dup := seen[m]; dup {
				continue
			}
			seen[m] = struct{}{}
			out = append(out, pointerInfo{Pointer: m})
		}
		return true
	})
	return out, true
}

func extractAssistantText(body []byte) string {
	candidates := []string{
		"message.content.parts.0",
		"v.message.content.parts.0",
		"v.0.message.content.parts.0",
		"current_node.message.content.parts.0",
	}
	for _, c := range candidates {
		if v := gjson.GetBytes(body, c).String(); v != "" {
			return v
		}
	}
	if v := gjson.GetBytes(body, "v").String(); v != "" {
		return v
	}
	return ""
}

func looksLikeTextResponse(t string) bool {
	t = strings.TrimSpace(t)
	if t == "" {
		return false
	}
	low := strings.ToLower(t)
	for _, marker := range []string{
		"i can't generate", "i cannot generate", "i'm unable to generate",
		"sorry,", "i can't help with", "unable to create", "image generation is",
	} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

func countDownloadablePointers(items []pointerInfo) int {
	n := 0
	for _, it := range items {
		if strings.HasPrefix(it.Pointer, "file-service://") || strings.HasPrefix(it.Pointer, "sediment://") {
			n++
		}
	}
	return n
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// pollConversation 兜底轮询 conversation 接口直到拿到可下载 pointer 或超时。
func pollConversation(
	ctx context.Context,
	client *req.Client,
	headers http.Header,
	baseConvURL string,
	conversationID string,
	excludedPointers map[string]struct{},
	allowEarlyReturn bool,
) ([]pointerInfo, error) {
	pollURL := fmt.Sprintf("%s/%s", baseConvURL, conversationID)
	deadline := time.Now().Add(pollDeadline)
	var last []pointerInfo
	iter := 0
	for time.Now().Before(deadline) {
		iter++
		var body json.RawMessage
		resp, err := client.R().
			SetContext(ctx).
			SetHeaders(headerToMap(headers)).
			SetSuccessResult(&body).
			Get(pollURL)
		if err != nil {
			return last, &TransportError{Wrapped: err}
		}
		if !resp.IsSuccessState() {
			return last, classifyHTTPError(resp, "poll conversation failed")
		}
		ptrs := collectToolPointers(body, excludedPointers)
		if t := extractAssistantText(body); looksLikeTextResponse(t) {
			return last, &ProtocolError{Reason: "text response in poll: " + truncate(t, 240), ConversationID: conversationID}
		}
		if len(ptrs) > 0 {
			last = ptrs
			if countDownloadablePointers(ptrs) > 0 {
				return ptrs, nil
			}
		}
		var backoff time.Duration
		switch {
		case iter <= 6:
			backoff = time.Second
		case iter <= 15:
			backoff = 2 * time.Second
		default:
			backoff = 4 * time.Second
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return last, ctx.Err()
		case <-t.C:
		}
	}
	return last, nil
}

// hashSet 用于在 edit 场景排除回显的源图（按 sha256 比较）。
// （历史函数：实际不再使用，pointer-level 去重已足够；保留空 stub 以减小 diff，可在后续清理彻底删除）

func buildUploadPointerSet(uploads []uploadedFile) map[string]struct{} {
	out := make(map[string]struct{}, len(uploads)*2)
	for _, u := range uploads {
		out["file-service://"+u.FileID] = struct{}{}
		// ChatGPT 现在常以 sediment://{file_id} 在 conversation 中引用上传图片
		out["sediment://"+u.FileID] = struct{}{}
	}
	return out
}
