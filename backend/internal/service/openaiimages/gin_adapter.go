package openaiimages

import (
	"github.com/gin-gonic/gin"
)

// GinSink 是把 gin.Context 适配为 ResponseSink 的 thin adapter。
//
// 用法：
//
//	sink := openaiimages.NewGinSink(c)
//	openaiimages.WriteChatSSE(sink, req, result, opts)
//
// 实现说明：
//   - SetHeader 在第一次 Write/WriteStatus 之前生效；调用顺序与 gin.ResponseWriter 一致；
//   - WriteStatus 内部调用 c.Writer.WriteHeader（gin.ResponseWriter 自动忽略二次调用）；
//   - Flush 调用 c.Writer.Flush，gin 已实现 http.Flusher。
type GinSink struct {
	c *gin.Context
}

// NewGinSink 包装 gin.Context。
func NewGinSink(c *gin.Context) *GinSink { return &GinSink{c: c} }

func (g *GinSink) Write(p []byte) (int, error) { return g.c.Writer.Write(p) }
func (g *GinSink) SetHeader(key, value string) { g.c.Writer.Header().Set(key, value) }
func (g *GinSink) WriteStatus(code int)        { g.c.Writer.WriteHeader(code) }
func (g *GinSink) Flush()                      { g.c.Writer.Flush() }

// WriteResult 根据请求入口与 stream 标志选择合适的 writer。
//
// 路由表：
//
//	images_generations / images_edits           → WriteStandard
//	chat_completions   stream=false             → WriteChatSync
//	chat_completions   stream=true              → WriteChatSSE
//	responses          stream=false             → WriteResponsesSync
//	responses          stream=true              → WriteResponsesSSE
//
// 调用方负责 sink 的关闭/flush（writer 内部会处理 stream flush）。
func WriteResult(sink ResponseSink, req *ImagesRequest, res *ImageResult, opts WriteOptions) error {
	switch req.Entry {
	case EntryImagesGenerations, EntryImagesEdits:
		return WriteStandard(sink, req, res, opts)
	case EntryChatCompletions:
		if req.Stream {
			return WriteChatSSE(sink, req, res, opts)
		}
		return WriteChatSync(sink, req, res, opts)
	case EntryResponses:
		if req.Stream {
			return WriteResponsesSSE(sink, req, res, opts)
		}
		return WriteResponsesSync(sink, req, res, opts)
	default:
		// fallback: 标准格式
		return WriteStandard(sink, req, res, opts)
	}
}
