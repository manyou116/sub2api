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
// 不在此处提取 assistant 文本：JSON Patch 增量流难以可靠地按 message role 切分；
// 文本分类（policy refusal / protocol error）统一在 driver.go 通过 polling fallback
// 拿到完整 message tree 后做。
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
	// JSON Patch 增量格式：顶层为 [{"p":"/message/content/parts/0","o":"append","v":"..."}]
	// 拼接所有指向 message.content 的 append/replace 字符串值。
	if root := gjson.ParseBytes(body); root.IsArray() {
		var sb strings.Builder
		root.ForEach(func(_, item gjson.Result) bool {
			p := item.Get("p").String()
			if p == "/message/content/parts/0" || strings.HasPrefix(p, "/message/content/parts/0") {
				if v := item.Get("v").String(); v != "" {
					sb.WriteString(v)
				}
			}
			return true
		})
		if s := sb.String(); s != "" {
			return s
		}
	}
	// 单 patch 对象形式：{"p":"/message/content/parts/0", "o":"append", "v":"text"}
	if vr := gjson.GetBytes(body, "v"); vr.Type == gjson.String {
		if p := gjson.GetBytes(body, "p").String(); strings.HasPrefix(p, "/message/content/parts/0") {
			return vr.String()
		}
	}
	return ""
}

// extractLastAssistantTextFromMapping 在 REST poll 返回的 conversation tree 中，
// 找出最新一条 author.role=="assistant" 且 content_type=="text" 的消息文本。
func extractLastAssistantTextFromMapping(body []byte) string {
	mapping := gjson.GetBytes(body, "mapping")
	if !mapping.Exists() {
		return ""
	}
	var (
		latestText string
		latestTime float64
	)
	mapping.ForEach(func(_, node gjson.Result) bool {
		msg := node.Get("message")
		if !msg.Exists() {
			return true
		}
		if msg.Get("author.role").String() != "assistant" {
			return true
		}
		if msg.Get("content.content_type").String() != "text" {
			return true
		}
		ts := msg.Get("create_time").Float()
		var sb strings.Builder
		msg.Get("content.parts").ForEach(func(_, p gjson.Result) bool {
			if p.Type == gjson.String {
				sb.WriteString(p.String())
			}
			return true
		})
		text := sb.String()
		if strings.TrimSpace(text) == "" {
			return true
		}
		if ts >= latestTime {
			latestTime = ts
			latestText = text
		}
		return true
	})
	return latestText
}

func looksLikeTextResponse(t string) bool {
	t = strings.TrimSpace(t)
	if t == "" {
		return false
	}
	low := strings.ToLower(t)
	for _, marker := range []string{
		"i can't", "i cannot", "i'm unable", "i am unable",
		"i won't", "i will not",
		"sorry,", "i'm sorry",
		"unable to", "image generation is",
	} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

// looksLikeContentPolicyRefusal 在 looksLikeTextResponse 命中后，
// 进一步判断是否为内容安全策略拒绝（vs 模型协议跑偏 / 临时不可用）。
// 命中关键词通常意味着同样 prompt 在任何账号上都会被拒，应该直接返回给客户端
// 而不是换号重试。
func looksLikeContentPolicyRefusal(t string) bool {
	low := strings.ToLower(t)
	for _, marker := range []string{
		"can't create",
		"can't provide",
		"can't generate",
		"can't help",
		"can't make",
		"can't assist",
		"cannot create",
		"cannot provide",
		"cannot generate",
		"cannot help",
		"cannot assist",
		"won't be able to",
		"won't help",
		"won't create",
		"won't generate",
		"violates",
		"violation",
		"content policy",
		"usage policies",
		"safety guidelines",
		"safety policy",
		"not allowed",
		"against our policies",
		"goes against",
		"policy prohibits",
		"explicit content",
		"adult content",
		"sexual content",
		"我无法",
		"无法生成",
		"无法创建",
		"无法提供",
		"违反",
		"政策",
		"准则",
	} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

// looksLikeQuotaExhaustedRefusal 检测上游配额耗尽类文本（free plan limit 等）。
// 命中后应返回 RateLimitError 让上层换号 + 给当前账号打 cooldown。
// resetAfter 为从文本中提取的恢复时间（hours/minutes），失败返回 0。
func looksLikeQuotaExhaustedRefusal(t string) (bool, time.Duration) {
	low := strings.ToLower(t)
	markers := []string{
		"hit the free plan limit",
		"reached the free plan limit",
		"plan limit for image",
		"image generation limit",
		"limit for image generation",
		"limit resets in",
		"rate limit",
		"too many requests",
		"please try again later",
	}
	hit := false
	for _, m := range markers {
		if strings.Contains(low, m) {
			hit = true
			break
		}
	}
	if !hit {
		return false, 0
	}
	return true, parseResetAfterFromText(low)
}

// parseResetAfterFromText 从 "limit resets in 21 hours and 11 minutes" 类文本中提取恢复时长。
func parseResetAfterFromText(low string) time.Duration {
	idx := strings.Index(low, "resets in")
	if idx < 0 {
		idx = strings.Index(low, "try again in")
	}
	if idx < 0 {
		return 0
	}
	tail := low[idx:]
	var dur time.Duration
	var n int
	var unit string
	for _, pat := range []string{"%d hours and %d minutes", "%d hour and %d minutes", "%d hours and %d minute", "%d hour and %d minute"} {
		var h, m int
		if c, _ := fmt.Sscanf(tail, "resets in "+pat, &h, &m); c == 2 {
			return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute
		}
	}
	if c, _ := fmt.Sscanf(tail, "resets in %d %s", &n, &unit); c >= 2 {
		switch {
		case strings.HasPrefix(unit, "hour"):
			dur = time.Duration(n) * time.Hour
		case strings.HasPrefix(unit, "minute"):
			dur = time.Duration(n) * time.Minute
		case strings.HasPrefix(unit, "second"):
			dur = time.Duration(n) * time.Second
		}
	}
	return dur
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
		if len(ptrs) > 0 {
			last = ptrs
			if countDownloadablePointers(ptrs) > 0 {
				return ptrs, nil
			}
		}
		if t := extractLastAssistantTextFromMapping(body); t != "" && countDownloadablePointers(last) == 0 {
			if hit, reset := looksLikeQuotaExhaustedRefusal(t); hit {
				return last, &RateLimitError{
					StatusCode: http.StatusTooManyRequests,
					Message:    truncate(strings.TrimSpace(t), 480),
					ResetAfter: reset,
				}
			}
			if looksLikeContentPolicyRefusal(t) {
				return last, &ContentPolicyError{
					UpstreamMessage: truncate(strings.TrimSpace(t), 480),
					ConversationID:  conversationID,
				}
			}
			return last, &ProtocolError{Reason: "text response in poll: " + truncate(t, 240), ConversationID: conversationID}
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
