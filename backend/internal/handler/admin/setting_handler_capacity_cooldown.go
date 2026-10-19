package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *SettingHandler) GetOpenAICapacityCooldownSettings(c *gin.Context) {
	v, err := h.settingService.GetOpenAICapacityCooldownSettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, v)
}

func (h *SettingHandler) UpdateOpenAICapacityCooldownSettings(c *gin.Context) {
	v := service.DefaultOpenAICapacityCooldownSettings()
	if err := c.ShouldBindJSON(&v); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if err := v.Validate(); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if err := h.settingService.SetOpenAICapacityCooldownSettings(c.Request.Context(), v); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, v)
}
