package openaiimages

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
)

// RenderMarkdown 把一组 ImageItem 渲染为 chat/responses 入口的 markdown 内容。
// 渲染规则：
//   - ItemBytes 非空：内联为 ![](data:{mime};base64,...)
//   - ItemBytes 空但 URL 非空：![](url)
//   - 多张图片用空行分隔
//   - 顶部可附加 RevisedPrompt 摘要（若 driver 提供且非空，且与原 prompt 不同）
//
// 该输出形态参照 chatgpt2api services/chatgpt_service._format_image_result。
func RenderMarkdown(originalPrompt string, items []ImageItem) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	for i, it := range items {
		if it.RevisedPrompt != "" && !strings.EqualFold(strings.TrimSpace(it.RevisedPrompt), strings.TrimSpace(originalPrompt)) {
			fmt.Fprintf(&b, "*Revised prompt:* %s\n\n", strings.TrimSpace(it.RevisedPrompt))
		}
		switch {
		case len(it.Bytes) > 0:
			mime := it.MimeType
			if mime == "" {
				mime = http.DetectContentType(it.Bytes)
			}
			fmt.Fprintf(&b, "![image-%d](data:%s;base64,%s)", i+1, mime, base64.StdEncoding.EncodeToString(it.Bytes))
		case it.B64JSON != "":
			mime := it.MimeType
			if mime == "" {
				mime = "image/png"
			}
			fmt.Fprintf(&b, "![image-%d](data:%s;base64,%s)", i+1, mime, it.B64JSON)
		case it.URL != "":
			fmt.Fprintf(&b, "![image-%d](%s)", i+1, it.URL)
		default:
			continue
		}
		if i < len(items)-1 {
			b.WriteString("\n\n")
		}
	}
	return b.String()
}

// ExtractDataURLs 提取 markdown 内容中所有 data: 形式的图片 URL（用于 stream chunk 拆分）。
func ExtractDataURLs(content string) []string {
	if content == "" {
		return nil
	}
	var out []string
	const needle = "(data:"
	cursor := 0
	for {
		idx := strings.Index(content[cursor:], needle)
		if idx < 0 {
			break
		}
		start := cursor + idx + 1 // 跳过 (
		end := strings.IndexByte(content[start:], ')')
		if end < 0 {
			break
		}
		out = append(out, content[start:start+end])
		cursor = start + end + 1
	}
	return out
}
