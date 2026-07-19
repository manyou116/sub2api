package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/handler"

	"github.com/gin-gonic/gin"
)

// registerKiroAdminRoutes registers Kiro platform admin endpoints (P5).
func registerKiroAdminRoutes(accounts *gin.RouterGroup, h *handler.Handlers) {
	if accounts == nil || h == nil || h.Admin == nil || h.Admin.Account == nil {
		return
	}
	// JSON bulk import (kiro-account-manager export format)
	accounts.POST("/kiro/import", h.Admin.Account.ImportKiro)
}
