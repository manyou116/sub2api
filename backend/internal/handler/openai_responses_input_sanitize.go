package handler

import (
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// sanitizeOpenAIResponsesInput 兜底修复客户端发来的 input[] 中已知的不规范字段。
//
// 当前修复一项：function_call.id 必须以 "fc_" 开头（OpenAI Responses API 规范要求）。
// 不规范客户端示例：OpenClaw 把 chat-completions 风格的 "call_xxx" 误填到
// Responses 风格的 function_call.id 字段（参见 OpenClaw issue #52827）。
//
// 行为契约：
//   - 仅在 input[i].type == "function_call" 且 id 不以 "fc_" 开头时改写
//   - 合规客户端 / 不含 input 数组的请求一律原样返回（零影响）
//   - 不修改 call_id（避免破坏调用配对）
//   - 解析失败时返回原 body，不阻断请求
func sanitizeOpenAIResponsesInput(body []byte) []byte {
	inputArr := gjson.GetBytes(body, "input")
	if !inputArr.IsArray() {
		return body
	}

	out := body
	idx := 0
	inputArr.ForEach(func(_, item gjson.Result) bool {
		current := idx
		idx++
		if item.Get("type").String() != "function_call" {
			return true
		}
		id := item.Get("id").String()
		if id == "" || strings.HasPrefix(id, "fc_") {
			return true
		}
		newID := "fc_" + strings.TrimPrefix(id, "call_")
		path := "input." + strconv.Itoa(current) + ".id"
		if updated, err := sjson.SetBytes(out, path, newID); err == nil {
			out = updated
		}
		return true
	})
	return out
}
