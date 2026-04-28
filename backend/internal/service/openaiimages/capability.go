package openaiimages

import "strings"

// modelTable 维护"客户端别名 → Capability"。任何不在表中的 model 在 chat/responses
// 入口走文本分支，在 images/* 入口直接 400。
//
// 别名集合参照 chatgpt2api services/openai_backend_api.py 中的 IMAGE_MODELS：
//
//	gpt-image-2 / codex-gpt-image-2 / auto / gpt-5* 系列
//
// DriverName 字段只是"建议"，最终选择仍由 SelectDriver 结合 group/account toggle 决定。
var modelTable = map[string]Capability{
	"gpt-image-2":       {DriverName: DriverWeb, Plan: "basic", SupportsEdits: true},
	"codex-gpt-image-2": {DriverName: DriverWeb, Plan: "codex", SupportsEdits: true},
	"auto":              {DriverName: DriverWeb, Plan: "basic", SupportsEdits: true},
	"gpt-5":             {DriverName: DriverWeb, Plan: "native", SupportsEdits: true},
	"gpt-5-1":           {DriverName: DriverWeb, Plan: "native", SupportsEdits: true},
	"gpt-5-2":           {DriverName: DriverWeb, Plan: "native", SupportsEdits: true},
	"gpt-5-3":           {DriverName: DriverWeb, Plan: "native", SupportsEdits: true},
	"gpt-5-3-mini":      {DriverName: DriverWeb, Plan: "native", SupportsEdits: true},
	"gpt-5-mini":        {DriverName: DriverWeb, Plan: "native", SupportsEdits: true},

	// 标准 OpenAI 图片模型（API-Key 直连场景）。
	"dall-e-3":           {DriverName: DriverAPIKey, Plan: "basic", SupportsEdits: false},
	"dall-e-2":           {DriverName: DriverAPIKey, Plan: "basic", SupportsEdits: true},
	"gpt-image-1":        {DriverName: DriverAPIKey, Plan: "basic", SupportsEdits: true},
	"gpt-image-1-medium": {DriverName: DriverAPIKey, Plan: "basic", SupportsEdits: true},
	"gpt-image-1-high":   {DriverName: DriverAPIKey, Plan: "basic", SupportsEdits: true},
}

// Driver 名称常量。Driver 实例在选号阶段按这个名字注册。
const (
	DriverWeb       = "web"
	DriverAPIKey    = "apikey"
	DriverResponses = "responses"
)

// LookupCapability 把客户端送入的 model 名（含可能的前缀）解析成 Capability。
// 返回 (capability, true) 表示命中，(zero, false) 表示该 model 不归本网关处理。
func LookupCapability(model string) (Capability, bool) {
	key := strings.ToLower(strings.TrimSpace(model))
	if key == "" {
		return Capability{}, false
	}
	cap, ok := modelTable[key]
	return cap, ok
}

// IsImageModel 是 IsOpenAIImageGenerationModel 的语义替代：判断 model 是否
// 路由到本图片网关。chat/responses 入口用它决定是否分流到图片处理链路。
func IsImageModel(model string) bool {
	_, ok := LookupCapability(model)
	return ok
}

// ListPublicModelIDs 返回 GET /v1/models 暴露给客户端的图片模型清单。
func ListPublicModelIDs() []string {
	out := make([]string, 0, len(modelTable))
	for id := range modelTable {
		out = append(out, id)
	}
	return out
}

// ResolveDriverName 结合 capability + 账号能力解析最终 driver 名称。
//
// 决策矩阵：
//
//	account.IsAPIKey() == true                                  → DriverAPIKey
//	cap.DriverName == DriverWeb && account.LegacyImagesEnabled() → DriverWeb
//	cap.DriverName == DriverWeb && !account.LegacyImagesEnabled() → DriverResponses（OAuth 但未开 web）
//	cap.DriverName == DriverAPIKey                              → DriverAPIKey（dall-e 等只能 sk- 走）
//	cap.DriverName == DriverResponses                           → DriverResponses
//
// 调用方应保证已经过 SelectAccountForCapability 过滤，即 account 的 type / mode
// 与 cap 兼容；若不兼容此函数也会返回最相近的 driver，但 driver.Forward 会自检。
func ResolveDriverName(cap Capability, account AccountView) string {
	if account != nil && account.IsAPIKey() {
		return DriverAPIKey
	}
	switch cap.DriverName {
	case DriverAPIKey:
		return DriverAPIKey
	case DriverResponses:
		return DriverResponses
	case DriverWeb:
		if account != nil && account.LegacyImagesEnabled() {
			return DriverWeb
		}
		return DriverResponses
	}
	return DriverWeb
}
