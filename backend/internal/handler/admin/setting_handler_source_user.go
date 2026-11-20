package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *SettingHandler) GetSourceUserIntegrations(c *gin.Context) {
	sources, err := h.settingService.GetSourceUserIntegrations(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, sources)
}

func (h *SettingHandler) UpdateSourceUserIntegrations(c *gin.Context) {
	var sources []service.SourceUserIntegration
	if err := c.ShouldBindJSON(&sources); err != nil {
		response.BadRequest(c, "Invalid source integrations")
		return
	}
	if err := service.ValidateSourceUserIntegrations(sources); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if err := h.settingService.SetSourceUserIntegrations(c.Request.Context(), sources); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	h.GetSourceUserIntegrations(c)
}
