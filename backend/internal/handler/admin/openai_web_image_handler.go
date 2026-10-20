package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
	"net/http"
	"strconv"
)

func (h *OpenAIOAuthHandler) WebImageStatus(c *gin.Context) { h.webImageStatus(c, false) }
func (h *OpenAIOAuthHandler) ProbeWebImage(c *gin.Context)  { h.webImageStatus(c, true) }
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
			response.Error(c, http.StatusBadRequest, err.Error())
			return
		}
		response.Success(c, status)
		return
	}
	status, err := h.webImageService.Status(c.Request.Context(), id)
	if err != nil {
		response.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	response.Success(c, status)
}
