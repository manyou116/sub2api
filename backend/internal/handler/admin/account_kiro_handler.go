package admin

import (
	"errors"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// kiroImportItem aligns with kiro-account-manager export JSON (camelCase).
type kiroImportItem struct {
	ID           string `json:"id"`
	Email        string `json:"email"`
	Label        string `json:"label"`
	AuthMethod   string `json:"authMethod"` // "Social" / "IdC"
	Provider     string `json:"provider"`
	UserID       string `json:"userId"`
	MachineID    string `json:"machineId"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	IDToken      string `json:"idToken"`
	ExpiresAt    string `json:"expiresAt"`

	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	Region       string `json:"region"`

	ProfileArn string `json:"profileArn"`
	StartUrl   string `json:"startUrl"`

	UsageData map[string]any `json:"usageData"`
}

// KiroImportRequest is POST /admin/accounts/kiro/import body.
type KiroImportRequest struct {
	Items                 []kiroImportItem `json:"items" binding:"required"`
	GroupIDs              []int64          `json:"group_ids,omitempty"`
	Concurrency           int              `json:"concurrency,omitempty"`
	SkipMixedChannelCheck bool             `json:"skip_mixed_channel_check,omitempty"`
}

// KiroImportResult is one import row.
type KiroImportResult struct {
	Index   int    `json:"index"`
	ID      string `json:"id,omitempty"`
	Email   string `json:"email,omitempty"`
	Created bool   `json:"created"`
	Error   string `json:"error,omitempty"`
}

// ImportKiro bulk-imports Kiro accounts from kiro-account-manager JSON.
// POST /api/v1/admin/accounts/kiro/import
func (h *AccountHandler) ImportKiro(c *gin.Context) {
	var req KiroImportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if len(req.Items) == 0 {
		response.BadRequest(c, "items must not be empty")
		return
	}

	results := make([]KiroImportResult, 0, len(req.Items))
	succeeded := 0
	for i, item := range req.Items {
		res := KiroImportResult{Index: i, ID: item.ID, Email: item.Email}
		acc, err := h.createKiroAccountFromImport(c, &item, &req)
		if err != nil {
			res.Error = err.Error()
		} else {
			res.Created = true
			succeeded++
			if acc != nil {
				res.Email = acc.KiroEmail()
			}
		}
		results = append(results, res)
	}

	response.Success(c, gin.H{
		"results": results,
		"summary": gin.H{
			"total":     len(req.Items),
			"succeeded": succeeded,
			"failed":    len(req.Items) - succeeded,
		},
	})
}

func (h *AccountHandler) createKiroAccountFromImport(c *gin.Context, item *kiroImportItem, req *KiroImportRequest) (*service.Account, error) {
	authMethod := strings.ToLower(strings.TrimSpace(item.AuthMethod))
	if authMethod == "" {
		authMethod = service.KiroAuthMethodSocial
	}
	if strings.HasPrefix(authMethod, "idc") || strings.Contains(authMethod, "identity") {
		authMethod = service.KiroAuthMethodIdC
	} else {
		authMethod = service.KiroAuthMethodSocial
	}

	if strings.TrimSpace(item.RefreshToken) == "" {
		return nil, errKiroImportField("refreshToken is required")
	}
	if strings.TrimSpace(item.MachineID) == "" {
		return nil, errKiroImportField("machineId is required (Kiro backends key requests by machineId)")
	}
	if authMethod == service.KiroAuthMethodIdC {
		if strings.TrimSpace(item.ClientID) == "" || strings.TrimSpace(item.ClientSecret) == "" {
			return nil, errKiroImportField("IdC accounts require clientId + clientSecret")
		}
	}

	creds := map[string]any{
		"auth_method":   authMethod,
		"provider":      strings.TrimSpace(item.Provider),
		"email":         strings.TrimSpace(item.Email),
		"user_id":       strings.TrimSpace(item.UserID),
		"machine_id":    strings.TrimSpace(item.MachineID),
		"access_token":  strings.TrimSpace(item.AccessToken),
		"refresh_token": strings.TrimSpace(item.RefreshToken),
		"id_token":      strings.TrimSpace(item.IDToken),
	}
	if authMethod == service.KiroAuthMethodIdC {
		region := strings.TrimSpace(item.Region)
		if region == "" {
			region = service.KiroDefaultRegion
		}
		creds["client_id"] = strings.TrimSpace(item.ClientID)
		creds["client_secret"] = strings.TrimSpace(item.ClientSecret)
		creds["region"] = region
	} else {
		if v := strings.TrimSpace(item.ProfileArn); v != "" {
			creds["profile_arn"] = v
		}
		if v := strings.TrimSpace(item.StartUrl); v != "" {
			creds["start_url"] = v
		}
	}

	extra := map[string]any{
		"kiro_imported_at":    time.Now().Unix(),
		"kiro_imported_label": strings.TrimSpace(item.Label),
	}
	if item.UsageData != nil {
		extra["kiro_usage_data"] = item.UsageData
	}

	name := strings.TrimSpace(item.Label)
	if name == "" {
		name = strings.TrimSpace(item.Email)
	}
	if name == "" {
		name = "kiro-" + strings.TrimSpace(item.ID)
	}
	if name == "kiro-" {
		name = "kiro-account"
	}

	concurrency := req.Concurrency
	if concurrency <= 0 {
		concurrency = 1
	}

	input := &service.CreateAccountInput{
		Name:                  name,
		Platform:              service.PlatformKiro,
		Type:                  service.AccountTypeOAuth,
		Credentials:           creds,
		Extra:                 extra,
		Concurrency:           concurrency,
		GroupIDs:              req.GroupIDs,
		SkipMixedChannelCheck: req.SkipMixedChannelCheck,
	}
	return h.adminService.CreateAccount(c.Request.Context(), input)
}

func errKiroImportField(msg string) error { return errors.New(msg) }
