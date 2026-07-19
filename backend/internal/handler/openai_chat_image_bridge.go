package handler

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// chatImageCaptureWriter buffers the images pipeline response so we can re-encode
// it as chat.completion without forking the images scheduling/forward path.
type chatImageCaptureWriter struct {
	gin.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *chatImageCaptureWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}

func (w *chatImageCaptureWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(b)
}

func (w *chatImageCaptureWriter) WriteString(s string) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.WriteString(s)
}

func (w *chatImageCaptureWriter) Written() bool {
	return w.status != 0 || w.body.Len() > 0
}

func (w *chatImageCaptureWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (w *chatImageCaptureWriter) Size() int { return w.body.Len() }

func (h *OpenAIGatewayHandler) chatImageBridgeEnabled() bool {
	if h == nil || h.cfg == nil {
		return false
	}
	return h.cfg.Gateway.OpenAIChatImageBridge.Enabled
}

func (h *OpenAIGatewayHandler) chatImageBridgeStyle() string {
	if h == nil || h.cfg == nil {
		return service.ChatImageBridgeStyleMarkdownDataURL
	}
	return service.NormalizeChatImageBridgeStyle(h.cfg.Gateway.OpenAIChatImageBridge.ResponseStyle)
}

// tryChatCompletionsImageBridge returns true when the request was fully handled
// (success, validation error, or disabled-with-image-model error).
func (h *OpenAIGatewayHandler) tryChatCompletionsImageBridge(
	c *gin.Context,
	reqLog *zap.Logger,
	body []byte,
	reqModel string,
	mappedModel string,
) bool {
	model := strings.TrimSpace(mappedModel)
	if model == "" {
		model = strings.TrimSpace(reqModel)
	}
	if !service.ShouldBridgeChatCompletionsToImages(model) && !service.ShouldBridgeChatCompletionsToImages(reqModel) {
		return false
	}
	// Prefer mapped business model when it is still an image model; else original.
	bridgeModel := model
	if !service.ShouldBridgeChatCompletionsToImages(bridgeModel) {
		bridgeModel = reqModel
	}

	if !h.chatImageBridgeEnabled() {
		reqLog.Info("openai_chat_completions.image_bridge_disabled",
			zap.String("model", bridgeModel),
		)
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error",
			"model '"+bridgeModel+"' is an image model; use POST /v1/images/generations, or set GATEWAY_OPENAI_CHAT_IMAGE_BRIDGE_ENABLED=true to bridge chat.completions")
		return true
	}

	imagesBody, err := service.BuildOpenAIImagesBodyFromChatCompletions(body, bridgeModel)
	if err != nil {
		reqLog.Info("openai_chat_completions.image_bridge_build_failed", zap.Error(err))
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return true
	}

	reqLog.Info("openai_chat_completions.image_bridge_start",
		zap.String("model", bridgeModel),
		zap.String("style", h.chatImageBridgeStyle()),
		zap.Int("images_body_len", len(imagesBody)),
	)

	// Swap request body to images JSON and capture the images handler response.
	origWriter := c.Writer
	capture := &chatImageCaptureWriter{ResponseWriter: origWriter}
	c.Writer = capture

	origBody := c.Request.Body
	origCL := c.Request.ContentLength
	origCT := c.Request.Header.Get("Content-Type")
	c.Request.Body = io.NopCloser(bytes.NewReader(imagesBody))
	c.Request.ContentLength = int64(len(imagesBody))
	c.Request.Header.Set("Content-Type", "application/json")

	// Run the full images pipeline (schedule / web / codex images tool).
	h.Images(c)

	// Restore request (best-effort; request is done).
	c.Request.Body = origBody
	c.Request.ContentLength = origCL
	if origCT != "" {
		c.Request.Header.Set("Content-Type", origCT)
	} else {
		c.Request.Header.Del("Content-Type")
	}
	c.Writer = origWriter

	status := capture.Status()
	raw := capture.body.Bytes()
	if status <= 0 {
		status = http.StatusOK
	}

	// Non-success: pass through (already OpenAI error shape from images path).
	if status >= 400 {
		if len(raw) == 0 {
			h.errorResponse(c, status, "upstream_error", "image bridge upstream failed")
			return true
		}
		c.Data(status, "application/json", raw)
		return true
	}

	chatJSON, err := service.WrapImagesJSONAsChatCompletion(raw, bridgeModel, h.chatImageBridgeStyle())
	if err != nil {
		reqLog.Warn("openai_chat_completions.image_bridge_wrap_failed", zap.Error(err))
		// Fall back to raw images payload rather than dropping a successful generation.
		if len(raw) > 0 {
			c.Data(status, "application/json", raw)
			return true
		}
		h.errorResponse(c, http.StatusBadGateway, "api_error", "failed to wrap image response as chat.completion")
		return true
	}
	c.Data(http.StatusOK, "application/json", chatJSON)
	return true
}
