// Package admin: image_quota_handler.go
//
// Manual probe of an OpenAI OAuth account's ChatGPT image-generation quota.
// Calls chatgpt.com /backend-api/me + /backend-api/conversation/init through
// openaiimages.AccountProbe, then writes image_* fields to account.extra.
package admin

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/service/openaiimages"

	"github.com/gin-gonic/gin"
)

// ImageQuotaHandler 处理图片配额相关的管理操作。
type ImageQuotaHandler struct {
	accountRepo service.AccountRepository
	probe       *openaiimages.AccountProbe
}

// NewImageQuotaHandler 构造一个 ImageQuotaHandler。
func NewImageQuotaHandler(accountRepo service.AccountRepository) *ImageQuotaHandler {
	return &ImageQuotaHandler{
		accountRepo: accountRepo,
		probe:       openaiimages.NewAccountProbe(accountRepo),
	}
}

// RefreshImageQuota POST /api/v1/admin/accounts/:id/refresh-image-quota
func (h *ImageQuotaHandler) RefreshImageQuota(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	ctx := c.Request.Context()
	account, err := h.accountRepo.GetByID(ctx, accountID)
	if err != nil || account == nil {
		response.NotFound(c, "Account not found")
		return
	}
	if !account.IsOpenAI() || !account.IsOAuth() {
		response.BadRequest(c, "image quota probe only supports OpenAI OAuth accounts")
		return
	}
	accessToken := account.GetOpenAIAccessToken()
	if accessToken == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    400,
			"message": "account has no access_token; refresh credentials first",
		})
		return
	}

	res, err := h.probe.RefreshAccount(ctx, openaiimages.ProbeAccount{
		ID:          account.ID,
		AccessToken: accessToken,
		ProxyURL:    accountProxyURL(account),
	})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"code":    502,
			"message": "failed to probe upstream: " + err.Error(),
		})
		return
	}

	response.Success(c, buildProbePayload(account.ID, account.Name, res))
}

// BulkRefreshImageQuota POST /api/v1/admin/accounts/bulk-refresh-image-quota
//
// 并发探测全部 OpenAI OAuth 账号（或入参 account_ids 指定的子集）。每个账号独立失败，
// 返回 results / failures 两段。并发上限 4，单次 probe 30s 超时。
func (h *ImageQuotaHandler) BulkRefreshImageQuota(c *gin.Context) {
	var req struct {
		AccountIDs []int64 `json:"account_ids"`
	}
	_ = c.ShouldBindJSON(&req)

	ctx := c.Request.Context()

	var targets []*service.Account
	if len(req.AccountIDs) > 0 {
		fetched, err := h.accountRepo.GetByIDs(ctx, req.AccountIDs)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": err.Error()})
			return
		}
		targets = fetched
	} else {
		all, err := h.accountRepo.ListByPlatform(ctx, "openai")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": err.Error()})
			return
		}
		for i := range all {
			targets = append(targets, &all[i])
		}
	}

	type itemResult struct {
		Payload gin.H
		Err     string
		ID      int64
		Email   string
	}

	const concurrency = 4
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	results := make([]itemResult, len(targets))

	for i, acc := range targets {
		if acc == nil {
			results[i] = itemResult{Err: "account not found"}
			continue
		}
		if !acc.IsOpenAI() || !acc.IsOAuth() {
			results[i] = itemResult{ID: acc.ID, Email: acc.Name, Err: "skipped: not openai oauth"}
			continue
		}
		token := acc.GetOpenAIAccessToken()
		if token == "" {
			results[i] = itemResult{ID: acc.ID, Email: acc.Name, Err: "no access token"}
			continue
		}

		wg.Add(1)
		sem <- struct{}{}
		proxyURL := accountProxyURL(acc)
		go func(i int, acc *service.Account, token, proxyURL string) {
			defer wg.Done()
			defer func() { <-sem }()
			pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			res, err := h.probe.RefreshAccount(pctx, openaiimages.ProbeAccount{
				ID:          acc.ID,
				AccessToken: token,
				ProxyURL:    proxyURL,
			})
			if err != nil {
				results[i] = itemResult{ID: acc.ID, Email: acc.Name, Err: err.Error()}
				return
			}
			results[i] = itemResult{ID: acc.ID, Email: acc.Name, Payload: buildProbePayload(acc.ID, acc.Name, res)}
		}(i, acc, token, proxyURL)
	}
	wg.Wait()

	successes := make([]gin.H, 0, len(results))
	failures := make([]gin.H, 0)
	for _, r := range results {
		if r.Err != "" {
			failures = append(failures, gin.H{"account_id": r.ID, "email": r.Email, "error": r.Err})
			continue
		}
		successes = append(successes, r.Payload)
	}

	response.Success(c, gin.H{
		"total":     len(targets),
		"succeeded": len(successes),
		"failed":    len(failures),
		"results":   successes,
		"failures":  failures,
	})
}

// accountProxyURL 返回账号生效的代理 URL（账号自带优先；否则 fallback 到分组代理，
// 此 fallback 已在 accountsToService 中提前 hydrate 到 account.Proxy）。
func accountProxyURL(a *service.Account) string {
	if a == nil || a.Proxy == nil {
		return ""
	}
	return a.Proxy.URL()
}

func buildProbePayload(accountID int64, email string, res *openaiimages.ProbeResult) gin.H {
	payload := gin.H{
		"account_id":            accountID,
		"email":                 email,
		"plan":                  res.AccountPlan,
		"image_quota_remaining": res.QuotaRemaining,
		"image_quota_total":     res.QuotaTotal,
		"probed_at":             res.ProbedAt.Unix(),
	}
	if res.Email != "" {
		payload["email"] = res.Email
	}
	if !res.CooldownUntil.IsZero() {
		payload["image_cooldown_until"] = res.CooldownUntil.Unix()
	}
	return payload
}
