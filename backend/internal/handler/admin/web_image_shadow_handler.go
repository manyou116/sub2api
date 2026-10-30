package admin

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// CreateWebImageShadow enables Web images through an ordinary account resource.
func (h *OpenAIOAuthHandler) CreateWebImageShadow(c *gin.Context) {
	parentID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || parentID <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	var input service.WebImageShadowOptions
	if err := c.ShouldBindJSON(&input); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	shadow, err := h.adminService.CreateWebImageShadow(c.Request.Context(), parentID, input)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, dto.AccountFromServiceShallow(shadow))
}

// CreateWebImageShadows reports each parent independently; repeated batches do
// not overwrite or resume existing children.
func (h *OpenAIOAuthHandler) CreateWebImageShadows(c *gin.Context) {
	var input service.WebImageShadowBatchInput
	if err := c.ShouldBindJSON(&input); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	result, err := service.CreateWebImageShadows(c.Request.Context(), h.adminService, input)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

// ConvertLegacyWebImageShadow previews by default; apply must be explicitly true.
func (h *OpenAIOAuthHandler) ConvertLegacyWebImageShadow(c *gin.Context) {
	parentID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || parentID <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	var input service.WebImageShadowConversionOptions
	if err := c.ShouldBindJSON(&input); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	result, err := h.adminService.ConvertLegacyWebImageShadow(c.Request.Context(), parentID, input)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}
