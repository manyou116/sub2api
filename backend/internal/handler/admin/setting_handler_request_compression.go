package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *SettingHandler) GetOpenAIRequestCompression(c *gin.Context) {
	enabled, err := h.settingService.GetOpenAIRequestCompression(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	options, guardErr := h.settingService.GetOpenAIRequestCompressionOptions(c.Request.Context())
	if guardErr != nil {
		response.ErrorFrom(c, guardErr)
		return
	}
	response.Success(c, struct {
		Enabled bool `json:"enabled"`
		service.OpenAIRequestCompressionOptions
	}{enabled, options})
}

func (h *SettingHandler) UpdateOpenAIRequestCompression(c *gin.Context) {
	var req struct {
		Enabled                *bool `json:"enabled"`
		ImageGuardEnabled      *bool `json:"image_guard_enabled"`
		ImageGuardMaxBytes     *int  `json:"image_guard_max_bytes"`
		Bare400FallbackEnabled *bool `json:"bare_400_fallback_enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "compression settings must be valid JSON")
		return
	}
	if req.Enabled == nil && req.ImageGuardEnabled == nil && req.ImageGuardMaxBytes == nil && req.Bare400FallbackEnabled == nil {
		response.BadRequest(c, "at least one compression setting is required")
		return
	}
	options, err := h.settingService.GetOpenAIRequestCompressionOptions(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if req.ImageGuardEnabled != nil {
		options.ImageGuardEnabled = *req.ImageGuardEnabled
	}
	if req.ImageGuardMaxBytes != nil {
		options.ImageGuardMaxBytes = *req.ImageGuardMaxBytes
	}
	if req.Bare400FallbackEnabled != nil {
		options.Bare400FallbackEnabled = *req.Bare400FallbackEnabled
	}
	if err := options.Validate(); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if err := h.settingService.SetOpenAIRequestCompressionOptions(c.Request.Context(), req.Enabled, options); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	h.GetOpenAIRequestCompression(c)
}
