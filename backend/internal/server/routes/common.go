package routes

import (
	"net/http"
	"sync/atomic"

	"github.com/gin-gonic/gin"
)

// shuttingDown is set when the process receives a termination signal so
// readiness probes can stop routing new traffic before the listener closes.
var shuttingDown atomic.Bool

// MarkShuttingDown marks the process as draining for health checks.
func MarkShuttingDown() {
	shuttingDown.Store(true)
}

// IsShuttingDown reports whether graceful shutdown has started.
func IsShuttingDown() bool {
	return shuttingDown.Load()
}

// RegisterCommonRoutes 注册通用路由（健康检查、状态等）
func RegisterCommonRoutes(r *gin.Engine) {
	// 健康检查 / readiness: returns 503 while draining so rolling updates stop routing.
	r.GET("/health", func(c *gin.Context) {
		if IsShuttingDown() {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "draining"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// Claude Code 遥测日志（忽略，直接返回200）
	r.POST("/api/event_logging/batch", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	// Setup status endpoint (always returns needs_setup: false in normal mode)
	// This is used by the frontend to detect when the service has restarted after setup
	r.GET("/setup/status", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"code": 0,
			"data": gin.H{
				"needs_setup": false,
				"step":        "completed",
			},
		})
	})
}
