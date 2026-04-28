// Package openaiimages 实现 OpenAI 图片场景网关的核心业务逻辑。
//
// 本包负责把 4 个对外 endpoint（/v1/images/generations、/v1/images/edits、
// /v1/chat/completions、/v1/responses）归一化为统一的 ImagesRequest，再交给
// 一个 Driver 完成上游交互，最后由 handler 层根据原入口选择 ResponseWriter
// 序列化回客户端。
//
// 三种 Driver：
//   - webdriver:   ChatGPT Web 反代（PoW + sentinel + f/conversation）
//   - apikeydriver: api.openai.com /v1/images/* 直连
//   - responsesdriver: api.openai.com /v1/responses image_generation tool
//
// 调度池：ImagePool 维护一组"图片可用账号"的 in-memory state，与 codex 文本
// 通道完全隔离（数据源是 account.extra 中独立的 image_* 字段）。
package openaiimages

import (
	"context"
	"time"
)

// EntryKind 表示客户端调用的对外入口类型。
type EntryKind string

const (
	EntryImagesGenerations EntryKind = "images_generations"
	EntryImagesEdits       EntryKind = "images_edits"
	EntryChatCompletions   EntryKind = "chat_completions"
	EntryResponses         EntryKind = "responses"
)

// ResponseFormat 控制 ImageItem 的载体形式。
type ResponseFormat string

const (
	ResponseFormatB64JSON  ResponseFormat = "b64_json"
	ResponseFormatURL      ResponseFormat = "url"
	ResponseFormatMarkdown ResponseFormat = "markdown" // chat/responses 入口默认
)

// SourceImage 表示一张待编辑/参考的输入图（来自 multipart 或 chat 消息中的 image_url）。
type SourceImage struct {
	Filename    string
	ContentType string
	Data        []byte
}

// ImagesRequest 是三入口归一化后的内部请求模型。
type ImagesRequest struct {
	Entry          EntryKind
	Model          string // 客户端送入的 model 名称（可能是别名）
	Prompt         string
	N              int
	Size           string
	Quality        string
	Style          string
	Background     string
	ResponseFormat ResponseFormat
	// ResponseFormatExplicit 标记 ResponseFormat 是否由客户端显式指定。
	// 为 false 时，dispatcher 可用全局默认值覆盖（见 SettingKeyDefaultImageResponseFormat）。
	ResponseFormatExplicit bool
	Stream                 bool
	User           string

	// edits / chat 图片输入：第一张作为 base，余下作为 mask / 多图参考。
	Images []SourceImage

	// 透传给 driver 的可选参数（不同 driver 需要的字段不同）。
	Extras map[string]any

	// 计费/会话上下文。
	RequestID string
	StartedAt time.Time
}

// ImageItem 是单张生成结果。
type ImageItem struct {
	B64JSON       string // ResponseFormatB64JSON
	URL           string // ResponseFormatURL（cache.go 落盘后签发）
	RevisedPrompt string
	MimeType      string // 如 image/png
	Bytes         []byte // 原始字节，markdown 渲染时用；序列化前会被清空
}

// Usage 记录上游消耗（用于计费）。
type Usage struct {
	InputTokens  int
	OutputTokens int
	TotalTokens  int
	ImagesCount  int
}

// ImageResult 是 driver 返回给 dispatch 的统一结果。
type ImageResult struct {
	Items   []ImageItem
	Model   string // 上游真实使用的 model（可能与请求别名不同）
	Usage   Usage
	Created int64 // unix seconds
	// QuotaSnapshot 若非空，说明 driver 顺便从上游响应中拿到了配额信息，
	// 可由 ImagePool 直接写入 account.extra.image_*。
	QuotaSnapshot *AccountQuotaSnapshot
}

// AccountQuotaSnapshot 描述一次 probe 或一次成功调用顺便观测到的账号配额。
type AccountQuotaSnapshot struct {
	Plan           string    // free / plus / pro
	QuotaRemaining int       // 剩余次数（未知则 -1）
	QuotaTotal     int
	CooldownUntil  time.Time // 零值 = 未限流
	ObservedAt     time.Time
}

// Capability 表示某个图片别名所需的 driver 与子能力。
type Capability struct {
	// DriverName 路由到哪个 driver 处理。可能是 "web" / "apikey" / "responses"。
	// 实际选择仍受 group/account toggle 影响（见 capability.go SelectDriver）。
	DriverName string
	// Plan 表示模型属于哪个计费/配额池（"basic" / "codex" / "native"）。
	Plan string
	// SupportsEdits 是否支持图生图。
	SupportsEdits bool
}

// Driver 是图片生成的统一抽象。
type Driver interface {
	Name() string
	// Forward 同步发起一次完整的生成/编辑流程并返回结果。
	// 错误分类由 driver 内部完成：返回的 error 可被 IsFailover/IsRateLimit/IsFatal 判定。
	Forward(ctx context.Context, account AccountView, req *ImagesRequest) (*ImageResult, error)
}

// AccountView 是 driver 看到的账号视图，避免 openaiimages 直接依赖 service.Account。
// 由 service 包内的适配器填充。
type AccountView interface {
	ID() int64
	AccessToken() string
	ChatGPTAccountID() string
	UserAgent() string
	DeviceID() string
	SessionID() string
	ProxyURL() string
	IsAPIKey() bool
	APIKey() string
	// LegacyImagesEnabled 是 group + account toggle 解析后的最终开关
	// （account.extra.openai_oauth_legacy_images 覆盖 group.openai_legacy_images_default）。
	LegacyImagesEnabled() bool
	// QuotaSnapshot 当前内存中已知的图片配额状态，供 ImagePool 选号时读取。
	QuotaSnapshot() *AccountQuotaSnapshot
}
