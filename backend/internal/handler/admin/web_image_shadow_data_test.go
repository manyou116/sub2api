package admin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type webShadowDataAdmin struct {
	stubAdminService
	options     *service.WebImageShadowOptions
	update      *service.UpdateAccountInput
	failUpdate  bool
	deleted     bool
	schedulable bool
}

func (s *webShadowDataAdmin) CreateWebImageShadow(_ context.Context, parentID int64, opts service.WebImageShadowOptions) (*service.Account, error) {
	s.options = &opts
	return &service.Account{ID: 200, ParentAccountID: &parentID, QuotaDimension: service.QuotaDimensionWebImages}, nil
}
func (s *webShadowDataAdmin) UpdateAccount(_ context.Context, _ int64, input *service.UpdateAccountInput) (*service.Account, error) {
	s.update = input
	if s.failUpdate {
		return nil, errors.New("write failed")
	}
	return &service.Account{ID: 200}, nil
}
func (s *webShadowDataAdmin) DeleteAccount(_ context.Context, _ int64) error {
	s.deleted = true
	return nil
}
func (s *webShadowDataAdmin) SetAccountSchedulable(_ context.Context, _ int64, enabled bool) (*service.Account, error) {
	s.schedulable = enabled
	return &service.Account{ID: 200}, nil
}
func (s *webShadowDataAdmin) GetGroup(_ context.Context, id int64) (*service.Group, error) {
	for i := range s.groups {
		if s.groups[i].ID == id {
			return &s.groups[i], nil
		}
	}
	return nil, errors.New("missing group")
}

func TestWebImageShadowBackupRoundTrip(t *testing.T) {
	ctx := context.Background()
	for _, grouped := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty groups", true: "group name remap"}[grouped], func(t *testing.T) {
			source := &webShadowDataAdmin{}
			source.groups = []service.Group{{ID: 3, Name: "images", Platform: service.PlatformOpenAI}}
			h := &AccountHandler{adminService: source}
			account := &service.Account{Name: "web", Concurrency: 2, Priority: 0, Status: "inactive", Schedulable: false,
				Credentials: map[string]any{"model_mapping": map[string]any{"image-alias": "gpt-image-2"}, "access_token": "must-not-export"},
				Extra:       map[string]any{"openai_web_images": map[string]any{"model_mode": "fixed", "model": "gpt-5", "enabled": true}}}
			if grouped {
				account.GroupIDs = []int64{3}
			}
			backup, err := h.exportWebImageShadow(ctx, account)
			require.NoError(t, err)
			blob, err := json.Marshal(backup)
			require.NoError(t, err)
			require.NotContains(t, string(blob), "must-not-export")
			require.NotContains(t, string(blob), "enabled")
			var restored DataWebImageShadow
			require.NoError(t, json.Unmarshal(blob, &restored))
			target := &webShadowDataAdmin{}
			target.groups = []service.Group{{ID: 90, Name: "images", Platform: service.PlatformOpenAI}}
			h.adminService = target
			require.NoError(t, h.restoreWebImageShadow(ctx, 50, &restored))
			require.True(t, target.options.StartPaused)
			require.Zero(t, *target.options.Priority)
			require.Equal(t, 2, *target.options.Concurrency)
			if grouped {
				require.Equal(t, []int64{90}, *target.options.GroupIDs)
			} else {
				require.Empty(t, *target.options.GroupIDs)
			}
			require.False(t, target.schedulable)
			require.Equal(t, "inactive", target.update.Status)
			require.Equal(t, account.Credentials["model_mapping"], target.update.Credentials["model_mapping"])
		})
	}
}

func TestWebImageShadowRestoreFailureStaysSafe(t *testing.T) {
	backup := &DataWebImageShadow{Name: "web", Concurrency: 1, Status: service.StatusActive, Schedulable: true, GroupNames: []string{"missing"}}
	stub := &webShadowDataAdmin{}
	h := &AccountHandler{adminService: stub}
	require.Error(t, h.restoreWebImageShadow(context.Background(), 50, backup))
	require.Nil(t, stub.options, "resolve missing group before creating")
	backup.GroupNames = nil
	stub.failUpdate = true
	require.Error(t, h.restoreWebImageShadow(context.Background(), 50, backup))
	require.True(t, stub.options.StartPaused)
	require.True(t, stub.deleted, "failed restoration removes its paused partial shadow")
}

func TestWebImageShadowBackupPreservesExpiryBillingAndEmptyMapping(t *testing.T) {
	zero := 0.0
	load := 3
	note := "image account"
	expired := time.Now().Add(-time.Hour).Truncate(time.Second)
	account := &service.Account{Name: "expired", Concurrency: 1, Status: service.StatusActive,
		Schedulable: true, ExpiresAt: &expired, AutoPauseOnExpired: true,
		RateMultiplier: &zero, LoadFactor: &load, Notes: &note,
		Credentials: map[string]any{"model_mapping": map[string]any{}}}
	stub := &webShadowDataAdmin{}
	h := &AccountHandler{adminService: stub}
	backup, err := h.exportWebImageShadow(context.Background(), account)
	require.NoError(t, err)
	data, err := json.Marshal(backup)
	require.NoError(t, err)
	var restored DataWebImageShadow
	require.NoError(t, json.Unmarshal(data, &restored))
	require.NotNil(t, restored.ModelMapping)
	require.NoError(t, h.restoreWebImageShadow(context.Background(), 50, &restored))
	require.Equal(t, expired.Unix(), *stub.update.ExpiresAt)
	require.True(t, *stub.update.AutoPauseOnExpired)
	require.Zero(t, *stub.update.RateMultiplier)
	require.Equal(t, load, *stub.update.LoadFactor)
	require.Equal(t, note, *stub.update.Notes)
	require.Empty(t, stub.update.Credentials["model_mapping"])
	require.True(t, stub.options.StartPaused)
}
