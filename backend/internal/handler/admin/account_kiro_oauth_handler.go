package admin

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// 单例 Kiro OAuth Device 服务（无外部依赖，全进程共享）
var (
	kiroOAuthOnce sync.Once
	kiroOAuthSvc  *service.KiroOAuthDeviceService
)

func getKiroOAuthService() *service.KiroOAuthDeviceService {
	kiroOAuthOnce.Do(func() {
		kiroOAuthSvc = service.NewKiroOAuthDeviceService(service.NewKiroTokenService())
	})
	return kiroOAuthSvc
}

// ============== IdC Device Code OAuth ==============

// KiroOAuthStartRequest body of POST /admin/api/accounts/kiro/oauth/start
type KiroOAuthStartRequest struct {
	StartURL              string  `json:"startUrl,omitempty"`
	Region                string  `json:"region,omitempty"`
	GroupIDs              []int64 `json:"group_ids,omitempty"`
	Concurrency           int     `json:"concurrency,omitempty"`
	SkipMixedChannelCheck bool    `json:"skip_mixed_channel_check,omitempty"`
	Label                 string  `json:"label,omitempty"`
}

// StartKiroOAuth POST /admin/api/accounts/kiro/oauth/start
// 入参：可选 startUrl/region（默认 BuilderID）+ 可选 group_ids/concurrency
// 返回：{sessionId, verificationUriComplete, userCode, expiresIn, interval}
func (h *AccountHandler) StartKiroOAuth(c *gin.Context) {
	var req KiroOAuthStartRequest
	_ = c.ShouldBindJSON(&req) // 全字段可选

	groupIDs := append([]int64(nil), req.GroupIDs...)
	concurrency := req.Concurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	skipMixed := req.SkipMixedChannelCheck
	label := strings.TrimSpace(req.Label)

	svc := getKiroOAuthService()
	result, err := svc.StartDeviceAuth(c.Request.Context(),
		service.StartDeviceAuthInput{
			StartUrl: req.StartURL,
			Region:   req.Region,
			ProxyURL: "",
		},
		// 成功回调：把 token + IdC 元数据落库为新 Account
		func(ctx context.Context, info *service.KiroTokenInfo, meta service.OAuthSuccessContext) (int64, string, error) {
			machineID := uuid.NewString()
			accName := label
			if accName == "" {
				accName = fmt.Sprintf("kiro-idc-%s", time.Now().Format("20060102-150405"))
			}
			creds := map[string]any{
				"auth_method":   service.KiroAuthMethodIdC,
				"provider":      kiroProviderForStartUrl(meta.StartUrl),
				"machine_id":    machineID,
				"access_token":  info.AccessToken,
				"refresh_token": info.RefreshToken,
				"id_token":      info.IDToken,
				"client_id":     meta.ClientID,
				"client_secret": meta.ClientSecret,
				"region":        meta.Region,
				"start_url":     meta.StartUrl,
			}
			if info.ExpiresAt > 0 {
				creds["expires_at"] = info.ExpiresAt
			}
			extra := map[string]any{
				"kiro_imported_at":     nowUnix(),
				"kiro_imported_via":    "oauth_device_code",
				"kiro_oauth_start_url": meta.StartUrl,
				"kiro_oauth_region":    meta.Region,
			}
			input := &service.CreateAccountInput{
				Name:                  accName,
				Platform:              service.PlatformKiro,
				Type:                  service.AccountTypeOAuth,
				Credentials:           creds,
				Extra:                 extra,
				Concurrency:           concurrency,
				GroupIDs:              groupIDs,
				SkipMixedChannelCheck: skipMixed,
			}
			acc, err := h.adminService.CreateAccount(ctx, input)
			if err != nil {
				return 0, "", err
			}
			return acc.ID, acc.KiroEmail(), nil
		},
	)
	if err != nil {
		response.BadRequest(c, "kiro oauth start: "+err.Error())
		return
	}
	response.Success(c, result)
}

// PollKiroOAuth GET /admin/api/accounts/kiro/oauth/poll?sessionId=...
func (h *AccountHandler) PollKiroOAuth(c *gin.Context) {
	sessionID := strings.TrimSpace(c.Query("sessionId"))
	if sessionID == "" {
		response.BadRequest(c, "sessionId is required")
		return
	}
	svc := getKiroOAuthService()
	r, ok := svc.Poll(sessionID)
	if !ok {
		response.NotFound(c, "session not found or expired")
		return
	}
	response.Success(c, r)
}

// ============== Social RefreshToken 批量导入 ==============

// KiroSocialImportRequest body of POST /admin/api/accounts/kiro/oauth/social-import
type KiroSocialImportRequest struct {
	Provider              string   `json:"provider" binding:"required"` // Google | Github
	Tokens                []string `json:"tokens" binding:"required"`   // 一行一个 refresh_token
	GroupIDs              []int64  `json:"group_ids,omitempty"`
	Concurrency           int      `json:"concurrency,omitempty"`
	SkipMixedChannelCheck bool     `json:"skip_mixed_channel_check,omitempty"`
}

// KiroSocialImportItem 单条结果
type KiroSocialImportItem struct {
	Index        int    `json:"index"`
	TokenPreview string `json:"token_preview"`
	AccountID    int64  `json:"accountId,omitempty"`
	Email        string `json:"email,omitempty"`
	Error        string `json:"error,omitempty"`
}

// ImportKiroSocialTokens 批量导入 Google/GitHub refresh_token：
//   - 对每条 token 调一次 social refresh 验证
//   - 失败的不落库，结果列表里返回错误
//   - 成功的创建 Account 并写回最新 access/refresh
func (h *AccountHandler) ImportKiroSocialTokens(c *gin.Context) {
	var req KiroSocialImportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	provider := normalizeKiroSocialProvider(req.Provider)
	if provider == "" {
		response.BadRequest(c, "provider must be Google or Github")
		return
	}
	cleaned := make([]string, 0, len(req.Tokens))
	for _, t := range req.Tokens {
		if v := strings.TrimSpace(t); v != "" {
			cleaned = append(cleaned, v)
		}
	}
	if len(cleaned) == 0 {
		response.BadRequest(c, "tokens must not be empty")
		return
	}

	concurrency := req.Concurrency
	if concurrency <= 0 {
		concurrency = 1
	}

	tokenSvc := service.NewKiroTokenService()
	results := make([]KiroSocialImportItem, 0, len(cleaned))
	imported := 0

	for i, rt := range cleaned {
		item := KiroSocialImportItem{Index: i, TokenPreview: previewToken(rt)}

		machineID := uuid.NewString()
		// 临时 Account 用于走 RefreshAccountToken
		tempAccount := &service.Account{
			Platform: service.PlatformKiro,
			Type:     service.AccountTypeOAuth,
			Credentials: map[string]any{
				"auth_method":   service.KiroAuthMethodSocial,
				"provider":      provider,
				"refresh_token": rt,
				"machine_id":    machineID,
			},
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
		info, err := tokenSvc.RefreshAccountToken(ctx, tempAccount, "")
		cancel()
		if err != nil {
			item.Error = err.Error()
			results = append(results, item)
			continue
		}

		// 落库
		creds := map[string]any{
			"auth_method":   service.KiroAuthMethodSocial,
			"provider":      provider,
			"machine_id":    machineID,
			"access_token":  info.AccessToken,
			"refresh_token": info.RefreshToken,
			"id_token":      info.IDToken,
		}
		if info.ProfileArn != "" {
			creds["profile_arn"] = info.ProfileArn
		}
		if info.ExpiresAt > 0 {
			creds["expires_at"] = info.ExpiresAt
		}
		extra := map[string]any{
			"kiro_imported_at":  nowUnix(),
			"kiro_imported_via": "social_refresh_bulk",
		}
		name := fmt.Sprintf("kiro-%s-%s", strings.ToLower(provider), time.Now().Format("20060102-150405.000"))
		input := &service.CreateAccountInput{
			Name:                  name,
			Platform:              service.PlatformKiro,
			Type:                  service.AccountTypeOAuth,
			Credentials:           creds,
			Extra:                 extra,
			Concurrency:           concurrency,
			GroupIDs:              append([]int64(nil), req.GroupIDs...),
			SkipMixedChannelCheck: req.SkipMixedChannelCheck,
		}
		acc, createErr := h.adminService.CreateAccount(c.Request.Context(), input)
		if createErr != nil {
			item.Error = "create account: " + createErr.Error()
			results = append(results, item)
			continue
		}
		item.AccountID = acc.ID
		item.Email = acc.KiroEmail()
		imported++
		results = append(results, item)
	}

	response.Success(c, gin.H{
		"results": results,
		"summary": gin.H{
			"total":    len(cleaned),
			"imported": imported,
			"failed":   len(cleaned) - imported,
		},
	})
}

// ============== utils ==============

func normalizeKiroSocialProvider(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "google":
		return "Google"
	case "github":
		return "Github"
	default:
		return ""
	}
}

func previewToken(t string) string {
	t = strings.TrimSpace(t)
	if len(t) <= 12 {
		return t
	}
	return t[:6] + "..." + t[len(t)-4:]
}

// kiroProviderForStartUrl 推断 IdC 登录的 provider 标签：
//   - 默认 BuilderID URL → "BuilderId"
//   - 自定义企业 URL    → "Enterprise"
func kiroProviderForStartUrl(startUrl string) string {
	startUrl = strings.TrimSpace(startUrl)
	if startUrl == "" || strings.EqualFold(startUrl, service.KiroOAuthDefaultStartUrl) {
		return "BuilderId"
	}
	return "Enterprise"
}
