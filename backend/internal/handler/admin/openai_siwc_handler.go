package admin

import (
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

func siwcAccountID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return 0, false
	}
	return id, true
}

func (h *OpenAIOAuthHandler) SIWCStatus(c *gin.Context) {
	if id, ok := siwcAccountID(c); ok {
		status, err := h.openaiOAuthService.GetOpenAIConnectionStatus(c.Request.Context(), id, openAIConnectionKind(c))
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		response.Success(c, status)
	}
}

func (h *OpenAIOAuthHandler) BeginSIWC(c *gin.Context) {
	if id, ok := siwcAccountID(c); ok {
		info, err := h.openaiOAuthService.BeginOpenAIConnectionAuthorization(c.Request.Context(), id, openAIConnectionKind(c))
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		response.Success(c, info)
	}
}

func (h *OpenAIOAuthHandler) CompleteSIWC(c *gin.Context) {
	id, ok := siwcAccountID(c)
	if !ok {
		return
	}
	var input struct {
		SessionID string `json:"session_id" binding:"required"`
		Callback  string `json:"callback_url" binding:"required"`
	}
	if c.ShouldBindJSON(&input) != nil {
		response.BadRequest(c, "SIWC session and callback URL are required")
		return
	}
	if err := h.openaiOAuthService.CompleteOpenAIConnectionAuthorization(c.Request.Context(), id, openAIConnectionKind(c), input.SessionID, input.Callback); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	h.SIWCStatus(c)
}

func (h *OpenAIOAuthHandler) SelectSIWC(c *gin.Context) {
	id, ok := siwcAccountID(c)
	if !ok {
		return
	}
	var input struct {
		Enabled       *bool     `json:"enabled"`
		SearchEnabled *bool     `json:"search_enabled"`
		Models        *[]string `json:"models"`
	}
	kind := openAIConnectionKind(c)
	if c.ShouldBindJSON(&input) != nil {
		response.BadRequest(c, "Invalid authorization connection settings")
		return
	}
	ctx := c.Request.Context()
	var err error
	switch {
	case input.Enabled != nil && input.SearchEnabled == nil && input.Models == nil:
		err = h.openaiOAuthService.SetOpenAIConnectionEnabled(ctx, id, kind, *input.Enabled)
	case kind == "excel" && input.SearchEnabled != nil && input.Enabled == nil && input.Models == nil:
		err = h.openaiOAuthService.SetExcelBPSSearchEnabled(ctx, id, *input.SearchEnabled)
	case kind == "excel" && input.Models != nil && input.Enabled == nil && input.SearchEnabled == nil:
		err = h.openaiOAuthService.SetExcelBPSModels(ctx, id, *input.Models)
	default:
		response.BadRequest(c, "Specify one valid authorization connection setting")
		return
	}
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	h.SIWCStatus(c)
}

func openAIConnectionKind(c *gin.Context) string {
	if strings.Contains(c.FullPath(), "/:id/excel") {
		return "excel"
	}
	return "siwc"
}

func (h *OpenAIOAuthHandler) RemoveSIWC(c *gin.Context) {
	if id, ok := siwcAccountID(c); ok {
		if err := h.openaiOAuthService.RemoveOpenAIConnection(c.Request.Context(), id, openAIConnectionKind(c)); err != nil {
			response.ErrorFrom(c, err)
			return
		}
		h.SIWCStatus(c)
	}
}
