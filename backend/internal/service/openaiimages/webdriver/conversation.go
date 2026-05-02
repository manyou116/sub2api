package webdriver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/imroc/req/v3"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	pkglogger "github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// mappingDiag 汇总一次 conversation REST 拉取里所有 message 的 role / recipient / content_type 计数 +
// 最近的 assistant 文本 / 最近的 tool message recipient & 摘要。仅用于诊断「模型不生图」问题。
type mappingDiag struct {
	Roles                map[string]int
	Recipients           map[string]int
	ContentTypes         map[string]int
	ToolMessages         int
	AssistantTextSnippet string
	LastToolRecipient    string
	LastToolContentType  string
	LastToolBodySnippet  string
	HasMapping           bool

	// 用于 "silent refusal" 早退判定：
	LastAssistantStatus      string // 比如 "in_progress" / "finished_successfully" / "finished_partial_completion"
	LastAssistantEndTurn     string // end_turn 字段：true / false / null
	LastAssistantContentRaw  string // 原始 content snippet（即使 parts 为空也能看到结构）
	LastAssistantPartsLen    int    // content.parts 数组长度
	LastAssistantRecipient   string
	LastUserContentTypeFinal string // 用户消息最终的 content_type（例如 multimodal_text 验证 attach 是否被认）
}

func summarizeMapping(body []byte) mappingDiag {
	d := mappingDiag{
		Roles:        map[string]int{},
		Recipients:   map[string]int{},
		ContentTypes: map[string]int{},
	}
	mapping := gjson.GetBytes(body, "mapping")
	if !mapping.Exists() || !mapping.IsObject() {
		return d
	}
	d.HasMapping = true
	var lastToolTime, lastAssistantTime, lastUserTime float64
	mapping.ForEach(func(_, node gjson.Result) bool {
		msg := node.Get("message")
		if !msg.Exists() {
			return true
		}
		role := msg.Get("author.role").String()
		recipient := msg.Get("recipient").String()
		ct := msg.Get("content.content_type").String()
		ts := msg.Get("create_time").Float()
		if role != "" {
			d.Roles[role]++
		}
		if recipient != "" {
			d.Recipients[recipient]++
		}
		if ct != "" {
			d.ContentTypes[ct]++
		}
		switch role {
		case "tool":
			d.ToolMessages++
			if ts >= lastToolTime {
				lastToolTime = ts
				d.LastToolRecipient = recipient
				d.LastToolContentType = ct
				raw := msg.Get("content").Raw
				d.LastToolBodySnippet = truncate(strings.ReplaceAll(raw, "\n", " "), 320)
			}
		case "assistant":
			// 跳过 system 注入的结构化 context (非真实回复)
			if ct == "model_editable_context" || ct == "system_message" || ct == "user_editable_context" {
				return true
			}
			if ts >= lastAssistantTime {
				lastAssistantTime = ts
				d.LastAssistantStatus = msg.Get("status").String()
				d.LastAssistantEndTurn = msg.Get("end_turn").Raw
				d.LastAssistantRecipient = recipient
				d.LastAssistantPartsLen = int(msg.Get("content.parts.#").Int())
				d.LastAssistantContentRaw = truncate(strings.ReplaceAll(msg.Get("content").Raw, "\n", " "), 320)
			}
		case "user":
			if ts >= lastUserTime {
				lastUserTime = ts
				d.LastUserContentTypeFinal = ct
			}
		}
		return true
	})
	d.AssistantTextSnippet = truncate(strings.TrimSpace(extractLastAssistantTextFromMapping(body)), 320)
	return d
}

func (d mappingDiag) zapFields(prefix string) []zap.Field {
	return []zap.Field{
		zap.Bool(prefix+"_has_mapping", d.HasMapping),
		zap.String(prefix+"_roles", mapToSortedString(d.Roles)),
		zap.String(prefix+"_recipients", mapToSortedString(d.Recipients)),
		zap.String(prefix+"_content_types", mapToSortedString(d.ContentTypes)),
		zap.Int(prefix+"_tool_messages", d.ToolMessages),
		zap.String(prefix+"_last_tool_recipient", d.LastToolRecipient),
		zap.String(prefix+"_last_tool_content_type", d.LastToolContentType),
		zap.String(prefix+"_last_tool_body_snippet", d.LastToolBodySnippet),
		zap.String(prefix+"_assistant_text_snippet", d.AssistantTextSnippet),
		zap.String(prefix+"_assistant_status", d.LastAssistantStatus),
		zap.String(prefix+"_assistant_end_turn", d.LastAssistantEndTurn),
		zap.String(prefix+"_assistant_recipient", d.LastAssistantRecipient),
		zap.Int(prefix+"_assistant_parts_len", d.LastAssistantPartsLen),
		zap.String(prefix+"_assistant_content_raw", d.LastAssistantContentRaw),
		zap.String(prefix+"_user_content_type", d.LastUserContentTypeFinal),
	}
}

// IsSilentRefusal 判定 "模型悄无声息地结束对话"：
// assistant 已经存在且状态 finished_*，但 tool_messages==0 且没有可见文本/parts 为空。
// 这通常是 Cloudflare 路由到非 image_gen backend 或被安全策略静默拦截。
// 早退避免傻等 4 分钟 polling deadline。
func (d mappingDiag) IsSilentRefusal() bool {
	if !d.HasMapping {
		return false
	}
	if d.ToolMessages > 0 {
		return false
	}
	if d.LastAssistantStatus == "" {
		return false
	}
	// 还在 in_progress 就别早退。
	if strings.HasPrefix(d.LastAssistantStatus, "in_progress") {
		return false
	}
	// finished 但没有 tool 也没有可见文本 → silent refusal
	if strings.HasPrefix(d.LastAssistantStatus, "finished") && d.AssistantTextSnippet == "" {
		return true
	}
	return false
}

func mapToSortedString(m map[string]int) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			_ = sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "%s=%d", k, m[k])
	}
	return sb.String()
}

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

// improviseHint 在用户 prompt 末尾追加的"软指令"，用于关掉模型的澄清反射
// （asking-for-clarification reflex）——实测：模糊 prompt 失败率 40% → 0%；
// 长 prompt 不掉画质。位置在 prompt 之后、aspect-ratio hint 之前；中文括号
// 包裹以暗示 meta 指令而非描述。
const improviseHint = "（描述若不够精确，请按你的审美自由发挥，直接出图，不要反问。）"

// buildPrompt 对齐 chatgpt2api `_build_image_prompt`：原样下发用户 prompt，
// 仅在已知尺寸时追加中文构图提示。不叠加任何英文前缀指令——实测前缀
// 会污染上下文、降低生图精细度（短 prompt 尤甚）。
//
// 在非空 prompt 之后追加 improviseHint，关闭模型的澄清反射，避免上游回
// "提示词建议清单"或反问导致 ModelNoImageError。
func buildPrompt(userPrompt string, hasUploads bool, size string) string {
	prompt := strings.TrimSpace(userPrompt)
	if prompt == "" && hasUploads {
		prompt = "请编辑附带的图片。"
	}
	hint := aspectRatioHint(size)
	parts := make([]string, 0, 3)
	if prompt != "" {
		parts = append(parts, prompt, improviseHint)
	}
	if hint != "" {
		parts = append(parts, hint)
	}
	return strings.Join(parts, "\n\n")
}

// aspectRatioHint 把 OpenAI 的 size（如 "1024x1024" / "1792x1024" / "1:1"）
// 翻译成中文构图提示，与 chatgpt2api 一致。未知 size 返回空串（不附加）。
func aspectRatioHint(size string) string {
	s := strings.ToLower(strings.TrimSpace(size))
	if s == "" || s == "auto" {
		return ""
	}
	ratio := s
	if strings.Contains(s, "x") {
		parts := strings.SplitN(s, "x", 2)
		if len(parts) == 2 {
			w, errW := strconv.Atoi(strings.TrimSpace(parts[0]))
			h, errH := strconv.Atoi(strings.TrimSpace(parts[1]))
			if errW == nil && errH == nil && w > 0 && h > 0 {
				g := gcd(w, h)
				rw, rh := w/g, h/g
				ratio = fmt.Sprintf("%d:%d", rw, rh)
			}
		}
	}
	switch ratio {
	case "1:1":
		return "输出为 1:1 正方形构图，主体居中，适合正方形画幅。"
	case "16:9":
		return "输出为 16:9 横屏构图，适合宽画幅展示。"
	case "9:16":
		return "输出为 9:16 竖屏构图，适合竖版画幅展示。"
	case "4:3":
		return "输出为 4:3 比例，兼顾宽度与高度，适合展示画面细节。"
	case "3:4":
		return "输出为 3:4 比例，纵向构图，适合人物肖像或竖向场景。"
	case "7:4":
		// OpenAI gpt-image-1 横幅档（1792x1024），按 16:9 引导。
		return "输出为 16:9 横屏构图，适合宽画幅展示。"
	case "4:7":
		return "输出为 9:16 竖屏构图，适合竖版画幅展示。"
	case "3:2":
		return "输出为 3:2 横向构图，适合风景或宽画面展示。"
	case "2:3":
		return "输出为 2:3 纵向构图，适合人物或竖向场景展示。"
	}
	return fmt.Sprintf("输出图片，宽高比为 %s。", ratio)
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	if a < 0 {
		return -a
	}
	return a
}

func prepareConversation(
	ctx context.Context,
	client *req.Client,
	headers http.Header,
	baseURL string,
	prompt, parentMessageID, requirementsToken, proofToken, model string,
) (string, error) {
	h := withTargetPath(headers, targetPathOf(baseURL))
	h.Set("openai-sentinel-chat-requirements-token", requirementsToken)
	if proofToken != "" {
		h.Set("openai-sentinel-proof-token", proofToken)
	}
	// 与 chatgpt2api `_prepare_image_conversation` 对齐：generate / edit 走完全
	// 一致的 prepare payload。曾尝试在 edit 分支使用空 system_hints +
	// attachment_mime_types + 移除 partial_query，结果上游不进入 image_gen
	// pipeline（不调用 image_generation tool），edit 永远拿不到结果。
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
	recipients := map[string]int{}
	roles := map[string]int{}
	contentTypes := map[string]int{}
	frames := 0
	dataTotalBytes := 0
	var firstFrameMs int
	// 信号位：用于诊断"上游有没有真正干活"。任何一个为 true 意味着上游
	// 已经认领了请求并开始推内容；全为 false 通常对应"静默挂起"。
	var sawAuthorAssistant, sawRecipientImage, sawTextDelta, sawPatch, sawTool, sawAnyMessageObj bool

	// 调试用：当环境变量 SUB2API_SSE_DUMP_DIR 非空时，将每帧 raw JSON 写入
	// <dir>/sse_<unixnano>_<seq>.jsonl，便于离线分析当前 ChatGPT 协议结构。
	// 为避免 conversation_id 还未到时找不到文件名，采用 seq 命名 + 内容里写 conv_id 行。
	var dumpFile *os.File
	if dir := strings.TrimSpace(os.Getenv("SUB2API_SSE_DUMP_DIR")); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
		seq := atomic.AddInt64(&sseDumpSeq, 1)
		fname := filepath.Join(dir, fmt.Sprintf("sse_%d_%d.jsonl", time.Now().UnixNano(), seq))
		if f, ferr := os.Create(fname); ferr == nil {
			dumpFile = f
			defer func() {
				_, _ = fmt.Fprintf(dumpFile, "{\"__meta__\":true,\"conversation_id\":%q,\"frames\":%d,\"downloadable\":%d}\n",
					conversationID, frames, countDownloadablePointers(pointers))
				_ = dumpFile.Close()
			}()
		}
	}
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
				frames++
				dataTotalBytes += len(dataBytes)
				if firstFrameMs == 0 {
					firstFrameMs = int(time.Since(startTime).Milliseconds())
				}
				if dumpFile != nil {
					_, _ = dumpFile.Write(dataBytes)
					_, _ = dumpFile.WriteString("\n")
				}
				// 多协议探针：旧 message snapshot 协议 + 新 JSON Patch 协议
				if gjson.GetBytes(dataBytes, "message").Exists() {
					sawAnyMessageObj = true
				}
				if gjson.GetBytes(dataBytes, "v").Exists() || gjson.GetBytes(dataBytes, "p").Exists() || gjson.GetBytes(dataBytes, "o").Exists() {
					sawPatch = true
				}
				lower := strings.ToLower(data)
				// 鲁棒匹配："role":"assistant" 或 "role": "assistant"（带空格）
				if strings.Contains(lower, "\"role\":\"assistant\"") || strings.Contains(lower, "\"role\": \"assistant\"") {
					sawAuthorAssistant = true
				}
				if strings.Contains(lower, "image_gen") || strings.Contains(lower, "dalle") || strings.Contains(lower, "imagegen") {
					sawRecipientImage = true
				}
				if strings.Contains(lower, "tool") {
					sawTool = true
				}
				if strings.Contains(lower, "\"delta\"") || strings.Contains(lower, "/parts/") {
					sawTextDelta = true
				}
				if id := gjson.GetBytes(dataBytes, "conversation_id").String(); id != "" {
					conversationID = id
				}
				if r := gjson.GetBytes(dataBytes, "message.author.role").String(); r != "" {
					roles[r]++
				}
				if r := gjson.GetBytes(dataBytes, "message.recipient").String(); r != "" {
					recipients[r]++
				}
				if ct := gjson.GetBytes(dataBytes, "message.content.content_type").String(); ct != "" {
					contentTypes[ct]++
				}
				pointers = append(pointers, collectPointers(dataBytes, excludedPointers)...)
				if allowEarlyExit && conversationID != "" && countDownloadablePointers(pointers) >= expectedImages {
					go drainStream(resp.Body)
					pkglogger.L().Info("openaiimages.sse_summary",
						zap.String("conversation_id", conversationID),
						zap.String("exit", "early"),
						zap.Int("frames", frames),
						zap.Int("downloadable", countDownloadablePointers(pointers)),
						zap.String("roles", mapToSortedString(roles)),
						zap.String("recipients", mapToSortedString(recipients)),
						zap.String("content_types", mapToSortedString(contentTypes)),
						zap.Int("first_frame_ms", firstFrameMs),
						zap.Int("data_bytes", dataTotalBytes),
						zap.Bool("saw_msg_obj", sawAnyMessageObj),
						zap.Bool("saw_patch", sawPatch),
						zap.Bool("saw_assistant", sawAuthorAssistant),
						zap.Bool("saw_image_gen", sawRecipientImage),
						zap.Bool("saw_tool", sawTool),
						zap.Bool("saw_delta", sawTextDelta),
					)
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
	pkglogger.L().Info("openaiimages.sse_summary",
		zap.String("conversation_id", conversationID),
		zap.String("exit", "eof"),
		zap.Int("frames", frames),
		zap.Int("downloadable", countDownloadablePointers(pointers)),
		zap.Int("excluded_count", len(excludedPointers)),
		zap.String("roles", mapToSortedString(roles)),
		zap.String("recipients", mapToSortedString(recipients)),
		zap.String("content_types", mapToSortedString(contentTypes)),
		zap.Int("first_frame_ms", firstFrameMs),
		zap.Int("data_bytes", dataTotalBytes),
		zap.Bool("saw_msg_obj", sawAnyMessageObj),
		zap.Bool("saw_patch", sawPatch),
		zap.Bool("saw_assistant", sawAuthorAssistant),
		zap.Bool("saw_image_gen", sawRecipientImage),
		zap.Bool("saw_tool", sawTool),
		zap.Bool("saw_delta", sawTextDelta),
	)
	return conversationID, pointers, firstTokenMs, false, nil
}

func drainStream(body io.ReadCloser) {
	if body == nil {
		return
	}
	defer func() { _ = body.Close() }()
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

func extractAssistantText(body []byte) string { //nolint:unused // kept for future SSE patch parsing; helper covered by tests in extractLastAssistantTextFromMapping
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
					_, _ = sb.WriteString(v)
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
				_, _ = sb.WriteString(p.String())
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
		"我不能",
		"无法生成",
		"无法创建",
		"无法提供",
		"无法帮",
		"不能生成",
		"不能创建",
		"不能提供",
		"不能帮",
		"违反",
		"政策",
		"准则",
		"色情",
		"裸体",
		"成人内容",
		"露骨",
		"血腥",
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
	pollHeaders := withTargetPath(headers, targetPathOf(pollURL))
	deadline := time.Now().Add(pollDeadline)
	var last []pointerInfo
	iter := 0
	var lastDiag mappingDiag
	for time.Now().Before(deadline) {
		iter++
		var body json.RawMessage
		resp, err := client.R().
			SetContext(ctx).
			SetHeaders(headerToMap(pollHeaders)).
			SetSuccessResult(&body).
			Get(pollURL)
		if err != nil {
			pkglogger.L().Warn("openaiimages.poll_transport_error",
				zap.String("conversation_id", conversationID),
				zap.Int("iter", iter),
				zap.String("error", err.Error()),
			)
			return last, &TransportError{Wrapped: err}
		}
		if !resp.IsSuccessState() {
			body2, _ := resp.ToBytes()
			pkglogger.L().Warn("openaiimages.poll_http_error",
				zap.String("conversation_id", conversationID),
				zap.Int("iter", iter),
				zap.Int("status", resp.StatusCode),
				zap.String("body_snippet", truncate(string(body2), 200)),
			)
			return last, classifyHTTPError(resp, "poll conversation failed")
		}
		ptrs := collectToolPointers(body, excludedPointers)
		lastDiag = summarizeMapping(body)
		// 每 4 次或第 1 次 / 第 2 次 / 第 3 次输出一次诊断日志，避免刷屏。
		if iter == 1 || iter == 2 || iter == 3 || iter%4 == 0 {
			fields := append([]zap.Field{
				zap.String("conversation_id", conversationID),
				zap.Int("iter", iter),
				zap.Int("pointers_seen", len(ptrs)),
				zap.Int("downloadable", countDownloadablePointers(ptrs)),
				zap.Int("excluded_count", len(excludedPointers)),
			}, lastDiag.zapFields("poll")...)
			pkglogger.L().Info("openaiimages.poll_iter", fields...)
		}
		if len(ptrs) > 0 {
			last = ptrs
			if countDownloadablePointers(ptrs) > 0 {
				return ptrs, nil
			}
		}
		// 早退：模型已结束（finished_*）但既无 tool 消息也无可见文本 →
		// silent refusal（CF 路由到非 image_gen backend / 安全策略静默拦截）。
		// 抛 ProtocolError 让上层换号重试，避免傻等 4 分钟 polling deadline。
		// 要求 iter >= 2 避免在 mapping 还在写入时误判。
		if iter >= 2 && lastDiag.IsSilentRefusal() {
			pkglogger.L().Warn("openaiimages.silent_refusal_detected",
				append([]zap.Field{
					zap.String("conversation_id", conversationID),
					zap.Int("iter", iter),
				}, lastDiag.zapFields("silent")...)...,
			)
			return last, &ProtocolError{
				Reason:         fmt.Sprintf("model finished without producing image (status=%s, parts=%d) — likely silent refusal / cf downgrade", lastDiag.LastAssistantStatus, lastDiag.LastAssistantPartsLen),
				ConversationID: conversationID,
			}
		}
		if t := extractLastAssistantTextFromMapping(body); t != "" && countDownloadablePointers(last) == 0 {
			// 如果 end_turn=false，说明本轮对话尚未结束——通常是 image_generation tool
			// 已被调用但图片仍在异步生成或排队中（tool 回复 "正在处理图片" 等）。
			// 此时不应基于文本内容提前退出，继续轮询直到 pointer 出现或截止时间到。
			if lastDiag.LastAssistantEndTurn == "false" {
				pkglogger.L().Info("openaiimages.poll_image_queued",
					zap.String("conversation_id", conversationID),
					zap.Int("iter", iter),
					zap.String("end_turn", lastDiag.LastAssistantEndTurn),
					zap.String("tool_snippet", truncate(lastDiag.LastToolBodySnippet, 120)),
				)
				// fall through to backoff and continue polling
			} else {
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
				// 兜底：模型在 poll 阶段产出文本而非图片。常见诱因：
				//   (a) prompt 过于模糊 (如 "随便生成一张图")，模型走对话分支输出
				//       追问问题或 tool_call 参数 JSON ({"size":"medium"} 等);
				//   (b) 内容策略拒绝但措辞未命中 looksLikeContentPolicyRefusal 关键词;
				//   (c) 模型被服务端降级，未能调用 image_generation tool。
				// 三种情况换号重试都解决不了，按 ModelNoImageError 把模型原文透传给客户端,
				// 让用户根据回复内容自行判断与调整 prompt——比硬归类为
				// content_policy_violation 误导更小。
				return last, &ModelNoImageError{
					UpstreamMessage: truncate(strings.TrimSpace(t), 480),
					ConversationID:  conversationID,
				}
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
	pkglogger.L().Warn("openaiimages.poll_deadline_exhausted",
		append([]zap.Field{
			zap.String("conversation_id", conversationID),
			zap.Int("iters", iter),
			zap.Int("downloadable_at_exit", countDownloadablePointers(last)),
			zap.Int("excluded_count", len(excludedPointers)),
		}, lastDiag.zapFields("poll_final")...)...,
	)
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

// sseDumpSeq 单调递增序号，用于生成 SSE dump 文件名（避免同一纳秒下名字冲突）。
var sseDumpSeq int64
