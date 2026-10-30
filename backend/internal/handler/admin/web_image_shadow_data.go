package admin

import (
	"context"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// DataWebImageShadow is configuration only. Parent credentials never appear here.
type DataWebImageShadow struct {
	Name               string         `json:"name"`
	Concurrency        int            `json:"concurrency"`
	Priority           int            `json:"priority"`
	GroupNames         []string       `json:"group_names"`
	Status             string         `json:"status"`
	Schedulable        bool           `json:"schedulable"`
	Notes              *string        `json:"notes,omitempty"`
	RateMultiplier     *float64       `json:"rate_multiplier,omitempty"`
	LoadFactor         *int           `json:"load_factor,omitempty"`
	ExpiresAt          *int64         `json:"expires_at,omitempty"`
	AutoPauseOnExpired bool           `json:"auto_pause_on_expired"`
	ModelMapping       map[string]any `json:"model_mapping"`
	ModelOptions       map[string]any `json:"model_options,omitempty"`
}

// WM exposes the parent's Codex resource. Route both the upstream read and its
// display-cache writes to that parent; Spark retains its independent display row.
func (h *OpenAIOAuthHandler) quotaResourceAccountID(ctx context.Context, id int64) (int64, error) {
	if h.adminService == nil {
		return id, nil
	}
	account, err := h.adminService.GetAccount(ctx, id)
	if err != nil {
		return 0, err
	}
	if account == nil {
		return 0, service.ErrAccountNotFound
	}
	if account.IsWorkImageShadow() {
		return *account.ParentAccountID, nil
	}
	return id, nil
}

func webImageBackupOptions(extra map[string]any) map[string]any {
	options, _ := extra["openai_web_images"].(map[string]any)
	result := map[string]any{}
	for _, key := range []string{"model_mode", "model", "thinking_effort", "require_download_attachment"} {
		if value, ok := options[key]; ok {
			result[key] = value
		}
	}
	if result["model_mode"] == "auto" {
		result["model_mode"] = "recommended"
	}
	return result
}

func (h *AccountHandler) exportWebImageShadow(ctx context.Context, account *service.Account) (*DataWebImageShadow, error) {
	return h.exportImageShadow(ctx, account, service.ImageShadowModeWeb)
}

func (h *AccountHandler) exportImageShadow(ctx context.Context, account *service.Account, mode string) (*DataWebImageShadow, error) {
	backup := &DataWebImageShadow{Name: account.Name, Concurrency: account.Concurrency,
		Priority: account.Priority, GroupNames: []string{}, Status: account.Status,
		Schedulable: account.Schedulable, ModelOptions: webImageBackupOptions(account.Extra),
		Notes: account.Notes, RateMultiplier: account.RateMultiplier, LoadFactor: account.LoadFactor,
		AutoPauseOnExpired: account.AutoPauseOnExpired}
	if account.ExpiresAt != nil {
		expires := account.ExpiresAt.Unix()
		backup.ExpiresAt = &expires
	}
	backup.ModelMapping, _ = account.Credentials["model_mapping"].(map[string]any)
	if backup.ModelMapping == nil {
		backup.ModelMapping = map[string]any{}
	}
	for _, id := range account.GroupIDs {
		group, err := h.adminService.GetGroup(ctx, id)
		if err != nil {
			return nil, err
		}
		if group == nil || group.Platform != service.PlatformOpenAI {
			return nil, fmt.Errorf("%s image shadow %d has an incompatible group %d", mode, account.ID, id)
		}
		backup.GroupNames = append(backup.GroupNames, group.Name)
	}
	return backup, nil
}

func (h *AccountHandler) restoreWebImageShadow(ctx context.Context, parentID int64, backup *DataWebImageShadow) error {
	return h.restoreImageShadow(ctx, parentID, backup, service.ImageShadowModeWeb)
}

func (h *AccountHandler) restoreImageShadow(ctx context.Context, parentID int64, backup *DataWebImageShadow, mode string) error {
	if backup.Concurrency <= 0 {
		return fmt.Errorf("web image shadow concurrency must be positive")
	}
	if backup.Status != service.StatusActive && backup.Status != "inactive" && backup.Status != service.StatusError {
		return fmt.Errorf("invalid Web image shadow status %q", backup.Status)
	}
	groupIDs := []int64{}
	if len(backup.GroupNames) > 0 {
		groups, err := h.adminService.GetAllGroupsIncludingInactive(ctx)
		if err != nil {
			return err
		}
		for _, name := range backup.GroupNames {
			var match int64
			for _, group := range groups {
				if group.Platform == service.PlatformOpenAI && group.Name == name {
					if match != 0 {
						return fmt.Errorf("ambiguous OpenAI group %q", name)
					}
					match = group.ID
				}
			}
			if match == 0 {
				return fmt.Errorf("OpenAI group %q not found; restore groups before importing shadows", name)
			}
			groupIDs = append(groupIDs, match)
		}
	}
	shadow, err := h.adminService.CreateWebImageShadow(ctx, parentID, service.WebImageShadowOptions{
		Mode: mode,
		Name: backup.Name, Concurrency: &backup.Concurrency, Priority: &backup.Priority,
		GroupIDs: &groupIDs, StartPaused: true,
	})
	if err != nil {
		return err
	}
	input := &service.UpdateAccountInput{Status: backup.Status,
		Notes: backup.Notes, RateMultiplier: backup.RateMultiplier, LoadFactor: backup.LoadFactor,
		ExpiresAt: backup.ExpiresAt, AutoPauseOnExpired: &backup.AutoPauseOnExpired,
		Extra: map[string]any{"openai_web_images": webImageBackupOptions(map[string]any{"openai_web_images": backup.ModelOptions})}}
	if backup.ModelMapping == nil {
		backup.ModelMapping = map[string]any{}
	}
	input.Credentials = map[string]any{"model_mapping": backup.ModelMapping}
	_, err = h.adminService.UpdateAccount(ctx, shadow.ID, input)
	if err == nil && backup.Schedulable {
		_, err = h.adminService.SetAccountSchedulable(ctx, shadow.ID, true)
	}
	if err != nil {
		rollbackErr := h.adminService.DeleteAccount(context.WithoutCancel(ctx), shadow.ID)
		return fmt.Errorf("restore Web image shadow: %w (rollback: %v)", err, rollbackErr)
	}
	return nil
}
