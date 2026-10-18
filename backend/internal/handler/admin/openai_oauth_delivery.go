package admin

import (
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// OpenAIOAuthCredentials 专用管理员接口显式声明能力，旧服务器不会静默忽略版本参数。
func (h *AccountHandler) OpenAIOAuthCredentials(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	ctx := c.Request.Context()
	if c.Request.Method != http.MethodGet {
		var req struct {
			Expected    string         `json:"expected_credential_revision" binding:"required,len=64,hexadecimal"`
			Credentials map[string]any `json:"credentials"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			response.BadRequest(c, "A valid expected_credential_revision is required")
			return
		}
		var executor service.OAuthRefreshExecutor
		if c.Request.Method == http.MethodPost {
			executor = service.NewOpenAITokenRefresher(h.openaiOAuthService, nil)
		}
		if _, err := h.oauthRefreshAPI.MutateOpenAIOAuthCredentials(ctx, id, req.Expected, req.Credentials, executor); err != nil {
			response.ErrorFrom(c, err)
			return
		}
	}
	snapshot, err := h.oauthRefreshAPI.ReadOpenAIOAuthCredentials(ctx, id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, snapshot)
}

// RefreshOpenAIOAuth 保留旧平台路由响应，但进入与后台相同的刷新协调入口。
func (h *AccountHandler) RefreshOpenAIOAuth(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	account, err := h.adminService.GetAccount(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if !account.IsOpenAI() {
		response.ErrorFrom(c, infraerrors.BadRequest("NOT_OPENAI_OAUTH", "OpenAI account required"))
		return
	}
	updated, _, err := h.refreshSingleAccount(c.Request.Context(), account)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, dto.AccountFromService(updated))
}

// OpenAIOAuthDeliveryCapabilities 允许新建交付在轮换 RT 前验证服务器支持条件写契约。
func (h *AccountHandler) OpenAIOAuthDeliveryCapabilities(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !h.oauthRefreshAPI.OpenAIOAuthDeliveryAvailable() {
		response.ErrorFrom(c, infraerrors.ServiceUnavailable("OAUTH_COORDINATION_UNAVAILABLE", "OAuth credential coordination unavailable"))
		return
	}
	response.Success(c, gin.H{"capability": service.OpenAIOAuthDeliveryCapability})
}
