package admin

import (
	"context"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *AccountHandler) SetOpenAIWebImagesService(svc *service.OpenAIWebImagesService) {
	if h == nil {
		return
	}
	h.webImages = svc
}

type openAIWebImagesPatchRequest struct {
	Enabled        *bool   `json:"enabled"`
	EnabledMode    *string `json:"enabled_mode"`
	MaxInflight    *int    `json:"max_inflight"`
	Priority       *int    `json:"priority"`
	ModelMode      *string `json:"model_mode"`
	Model          *string `json:"model"`
	ThinkingEffort *string `json:"thinking_effort"`
}

type openAIWebImagesBulkRequest struct {
	AccountIDs []int64                     `json:"account_ids"`
	Patch      openAIWebImagesPatchRequest `json:"patch"`
	Actions    struct {
		Probe bool `json:"probe"`
	} `json:"actions"`
}

func (h *AccountHandler) PatchOpenAIWebImages(c *gin.Context) {
	if h.webImages == nil {
		response.BadRequest(c, "openai web images service not enabled")
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "invalid account id")
		return
	}
	var req openAIWebImagesPatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request: "+err.Error())
		return
	}
	if err := h.webImages.PatchAccount(c.Request.Context(), id, service.OpenAIWebImagesBulkPatch{
		Enabled: req.Enabled, EnabledMode: req.EnabledMode, MaxInflight: req.MaxInflight, Priority: req.Priority,
		ModelMode: req.ModelMode, Model: req.Model, ThinkingEffort: req.ThinkingEffort,
	}); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	acc, err := h.adminService.GetAccount(c.Request.Context(), id)
	if err != nil {
		st, e2 := h.webImages.ProbeAccount(c.Request.Context(), id, false)
		if e2 != nil {
			response.Success(c, gin.H{"account_id": id, "updated": true})
			return
		}
		response.Success(c, st)
		return
	}
	st, _ := h.webImages.GetStatus(c.Request.Context(), acc)
	response.Success(c, st)
}

func (h *AccountHandler) GetOpenAIWebImagesStatus(c *gin.Context) {
	if h.webImages == nil {
		response.BadRequest(c, "openai web images service not enabled")
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "invalid account id")
		return
	}
	acc, err := h.adminService.GetAccount(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	st, err := h.webImages.GetStatus(c.Request.Context(), acc)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, st)
}

func (h *AccountHandler) ProbeOpenAIWebImages(c *gin.Context) {
	if h.webImages == nil {
		response.BadRequest(c, "openai web images service not enabled")
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "invalid account id")
		return
	}
	st, err := h.webImages.ProbeAccount(c.Request.Context(), id, true)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, st)
}

func (h *AccountHandler) BulkOpenAIWebImages(c *gin.Context) {
	if h.webImages == nil {
		response.BadRequest(c, "openai web images service not enabled")
		return
	}
	var req openAIWebImagesBulkRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request: "+err.Error())
		return
	}
	if len(req.AccountIDs) == 0 {
		response.BadRequest(c, "account_ids required")
		return
	}
	result, err := h.webImages.BulkPatch(c.Request.Context(), req.AccountIDs, service.OpenAIWebImagesBulkPatch{
		Enabled: req.Patch.Enabled, EnabledMode: req.Patch.EnabledMode, MaxInflight: req.Patch.MaxInflight, Priority: req.Patch.Priority,
		ModelMode: req.Patch.ModelMode, Model: req.Patch.Model, ThinkingEffort: req.Patch.ThinkingEffort,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if req.Actions.Probe {
		job, err := h.webImages.StartBulkProbe(c.Request.Context(), req.AccountIDs)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		result.JobID = job.ID
	}
	response.Success(c, result)
}

func (h *AccountHandler) BulkProbeOpenAIWebImages(c *gin.Context) {
	if h.webImages == nil {
		response.BadRequest(c, "openai web images service not enabled")
		return
	}
	var req struct {
		AccountIDs []int64 `json:"account_ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request: "+err.Error())
		return
	}
	job, err := h.webImages.StartBulkProbe(c.Request.Context(), req.AccountIDs)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, job)
}

func (h *AccountHandler) GetOpenAIWebImagesJob(c *gin.Context) {
	if h.webImages == nil {
		response.BadRequest(c, "openai web images service not enabled")
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	job, ok := h.webImages.GetBulkJob(id)
	if !ok {
		response.NotFound(c, "job not found")
		return
	}
	response.Success(c, job)
}

func (h *AccountHandler) OverviewOpenAIWebImages(c *gin.Context) {
	if h.webImages == nil {
		response.BadRequest(c, "openai web images service not enabled")
		return
	}
	raw := strings.TrimSpace(c.Query("ids"))
	if raw == "" {
		response.BadRequest(c, "ids required")
		return
	}
	parts := strings.Split(raw, ",")
	ids := make([]int64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		id, err := strconv.ParseInt(p, 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		ids = append(ids, id)
	}
	items, err := h.webImages.Overview(c.Request.Context(), ids)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"items": items})
}

func (h *AccountHandler) ClearOpenAIWebImagesCooldown(c *gin.Context) {
	if h.webImages == nil {
		response.BadRequest(c, "openai web images service not enabled")
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "invalid account id")
		return
	}
	if err := h.webImages.ClearCooldown(c.Request.Context(), id); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	acc, err := h.adminService.GetAccount(c.Request.Context(), id)
	if err != nil {
		response.Success(c, gin.H{"account_id": id, "cleared": true})
		return
	}
	st, _ := h.webImages.GetStatus(c.Request.Context(), acc)
	response.Success(c, st)
}

func (h *AccountHandler) clearWebImagesCooldownBestEffort(ctx context.Context, accountID int64) {
	if h == nil || h.webImages == nil || accountID <= 0 {
		return
	}
	// Always use background context: batch handlers may cancel gctx before redis DEL finishes.
	if err := h.webImages.ClearCooldown(context.Background(), accountID); err != nil {
		// best-effort
		_ = err
	}
}

func (h *AccountHandler) BulkClearOpenAIWebImagesCooldown(c *gin.Context) {
	if h.webImages == nil {
		response.BadRequest(c, "openai web images service not enabled")
		return
	}
	var req struct {
		AccountIDs []int64 `json:"account_ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request: "+err.Error())
		return
	}
	if len(req.AccountIDs) == 0 {
		response.BadRequest(c, "account_ids required")
		return
	}
	cleared, err := h.webImages.BulkClearCooldown(c.Request.Context(), req.AccountIDs)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"matched": len(req.AccountIDs), "cleared": cleared})
}
