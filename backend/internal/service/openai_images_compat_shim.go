// Package service — openai_images_compat_shim.go
//
// 旧 openai_images*.go (~3000 行) 已被新的 service/openaiimages 子包 + handler/openai_images_v2.go
// 完整替代。本文件保留少量公开符号给仍在引用的旧调用点：
//
//   - OpenAIImagesCapability (+ Basic/Native 常量) — 被 openai_account_scheduler.go / account.go 用于
//     候选账号过滤。新流水线只用 webdriver/responses-tool/api-key 三种 driver；这里把 capability 当
//     成 scheduler 的"是否支持图片"标签保留。
//   - isOpenAIImageGenerationModel — 被 openai_codex_transform.go / pricing_service.go 用于识别图片
//     模型，转调 openaiimages.IsImageModel。
//
// 不再保留：OpenAIImagesRequest / Upload / 各种 helper（responses tool 测试路径已改用 openaiimages
// 直接派发，见 account_test_service.go）。
package service

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service/openaiimages"
)

// OpenAIImagesCapability 是 scheduler 阶段的图片能力标签。
type OpenAIImagesCapability string

const (
	// OpenAIImagesCapabilityBasic 表示账号支持基础图片端点（gpt-image-2 单图 b64）。
	OpenAIImagesCapabilityBasic OpenAIImagesCapability = "images-basic"
	// OpenAIImagesCapabilityNative 表示账号支持原生 Responses 图片工具（流式 / n>1 / mask 等）。
	OpenAIImagesCapabilityNative OpenAIImagesCapability = "images-native"
)

// openAIImagesResponsesMainModel 是把 image_generation tool 重写到 Responses API 时使用的主模型名。
// 原本定义在已删除的 openai_images.go 里，被 openai_codex_transform.go 引用。
const openAIImagesResponsesMainModel = "gpt-5.4-mini"

// firstNonEmptyString 返回第一个非空（去除前后空白）的字符串；都为空时返回 ""。
// 接受任意 any 参数（非 string 视作空），原本定义在已被删除的 openai_images.go 里，
// 被 openai_codex_transform.go 多处使用（许多入参来自 map[string]any 拆出来的字段）。
func firstNonEmptyString(values ...any) string {
	for _, v := range values {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if trimmed := strings.TrimSpace(s); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// isOpenAIImageGenerationModel 保留语义：任意 "gpt-image-*" / "dall-e-*" / "imagen-*" 前缀
// 都视为图片生成模型。这与 openaiimages.IsImageModel（只命中已注册的具体型号）不同——
// 这里更宽松，给 codex_transform / pricing fallback 用，避免未来新型号无人识别。
func isOpenAIImageGenerationModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case m == "":
		return false
	case strings.HasPrefix(m, "gpt-image-"):
		return true
	case strings.HasPrefix(m, "dall-e-"):
		return true
	case strings.HasPrefix(m, "imagen-"):
		return true
	}
	return openaiimages.IsImageModel(m)
}
