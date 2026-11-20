package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// Keep the established constructor available to existing handlers and tests.
func NewGroupHandlerWithCapacityMonitor(adminService service.AdminService, dashboard *service.DashboardService, capacity *service.GroupCapacityService, cfg *config.Config, monitor *service.GroupCapacityMonitorService) *GroupHandler {
	h := NewGroupHandlerWithConfig(adminService, dashboard, capacity, cfg)
	h.capacityMonitor = monitor
	h.capacityMonitorCache = newSnapshotCache(time.Minute)
	return h
}

func (h *GroupHandler) GetCapacityMonitor(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid group ID")
		return
	}
	history, okHistory := service.CapacityMonitorDuration(c.DefaultQuery("history", "7d"))
	horizon, okHorizon := service.CapacityMonitorDuration(c.DefaultQuery("horizon", "15d"))
	if !okHistory || !okHorizon {
		response.BadRequest(c, "Invalid capacity monitor range")
		return
	}
	key := fmt.Sprintf("%s:%d:%d:%d", service.CapacityMonitorModelVersion, id, history, horizon)
	cache := h.capacityMonitorCache
	started := time.Now()
	entry, hit, err := cache.GetOrLoad(key, func() (any, error) { return h.capacityMonitor.Get(c.Request.Context(), id, history, horizon) })
	if err != nil {
		switch {
		case errors.Is(err, service.ErrCapacityMonitorNotFound):
			response.NotFound(c, "Group not found")
		case errors.Is(err, service.ErrCapacityMonitorBudget):
			response.ErrorWithDetails(c, http.StatusUnprocessableEntity, "Group exceeds capacity monitor query budget", "CAPACITY_MONITOR_BUDGET", nil)
		default:
			slog.Error("group_capacity_monitor_snapshot_failed", "group_id", id, "history", history.String(), "horizon", horizon.String(), "elapsed_ms", time.Since(started).Milliseconds(), "error", err)
			if errors.Is(err, context.DeadlineExceeded) {
				response.ErrorWithDetails(c, http.StatusServiceUnavailable, "Capacity monitor query timed out; please retry", "CAPACITY_MONITOR_TIMEOUT", nil)
			} else {
				response.ErrorWithDetails(c, http.StatusServiceUnavailable, "Unable to load capacity monitor snapshot", "CAPACITY_MONITOR_UNAVAILABLE", nil)
			}
		}
		return
	}
	// This feature accepts only nine range combinations and caps its group cache.
	// State/member changes may remain visible for at most the advertised 60s TTL.
	if cache != nil && !hit {
		cache.mu.Lock()
		now := time.Now()
		for k, v := range cache.items {
			if now.After(v.ExpiresAt) {
				delete(cache.items, k)
			}
		}
		for len(cache.items) > 128 {
			oldestKey := ""
			oldest := time.Time{}
			for k, v := range cache.items {
				if oldestKey == "" || v.ExpiresAt.Before(oldest) {
					oldestKey = k
					oldest = v.ExpiresAt
				}
			}
			delete(cache.items, oldestKey)
		}
		cache.mu.Unlock()
	}
	c.Header("ETag", entry.ETag)
	c.Header("Cache-Control", "private, no-cache")
	c.Header("Vary", "If-None-Match")
	c.Header("X-Snapshot-Cache", cacheStatusValue(hit))
	if ifNoneMatchMatched(c.GetHeader("If-None-Match"), entry.ETag) {
		c.Status(http.StatusNotModified)
		return
	}
	response.Success(c, entry.Payload)
}
