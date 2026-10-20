package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
	"net/http"
	"strconv"
)

func (h *OpenAIOAuthHandler) WebImageStatus(c *gin.Context) { h.webImageStatus(c, false) }
func (h *OpenAIOAuthHandler) ProbeWebImage(c *gin.Context)  { h.webImageStatus(c, true) }
func (h *OpenAIOAuthHandler) BatchWebImageStatus(c *gin.Context) {
	var input struct {
		AccountIDs []int64 `json:"account_ids" binding:"required,min=1,max=200,dive,gt=0"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		response.BadRequest(c, "account_ids must contain 1 to 200 positive IDs")
		return
	}
	if h.webImageService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Web image service unavailable")
		return
	}
	status, err := h.webImageService.BatchStatus(c.Request.Context(), input.AccountIDs)
	if err != nil {
		response.Error(c, http.StatusServiceUnavailable, "Web image state temporarily unavailable")
		return
	}
	response.Success(c, status)
}
func (h *OpenAIOAuthHandler) webImageStatus(c *gin.Context, probe bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.Error(c, http.StatusBadRequest, "invalid account id")
		return
	}
	if h.webImageService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Web image service unavailable")
		return
	}
	if probe {
		status, err := h.webImageService.Probe(c.Request.Context(), id)
		if err != nil {
			response.Error(c, http.StatusServiceUnavailable, "Web image request failed: "+err.Error())
			return
		}
		response.Success(c, status)
		return
	}
	status, err := h.webImageService.Status(c.Request.Context(), id)
	if err != nil {
		response.Error(c, http.StatusServiceUnavailable, "Web image request failed: "+err.Error())
		return
	}
	response.Success(c, status)
}
