package service

import (
	"context"
	"encoding/json"
	"fmt"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// Legacy defaults are supplied explicitly from the old deployment configuration.
// This helper never participates in runtime routing and never changes the parent.
type WebImageShadowConversionOptions struct {
	Apply                bool           `json:"apply"`
	LegacyDefaultEnabled *bool          `json:"legacy_default_enabled"`
	LegacyDefaults       map[string]any `json:"legacy_defaults"`
}

type WebImageShadowConversion struct {
	ParentID     int64          `json:"parent_id"`
	Enabled      bool           `json:"enabled"`
	Applied      bool           `json:"applied"`
	ShadowID     int64          `json:"shadow_id,omitempty"`
	Concurrency  int            `json:"concurrency"`
	Priority     int            `json:"priority"`
	GroupIDs     []int64        `json:"group_ids"`
	ModelOptions map[string]any `json:"model_options"`
}

func (s *adminServiceImpl) ConvertLegacyWebImageShadow(ctx context.Context, parentID int64, opts WebImageShadowConversionOptions) (*WebImageShadowConversion, error) {
	parent, err := s.accountRepo.GetByID(ctx, parentID)
	if err != nil {
		return nil, err
	}
	if parent.IsShadow() || !parent.IsOpenAIOAuthLike() {
		return nil, infraerrors.BadRequest("WEB_IMAGE_SHADOW_INVALID_PARENT", "conversion requires a real OpenAI OAuth or setup-token parent")
	}
	legacy, _ := parent.Extra["openai_web_images"].(map[string]any)
	enabled, explicit := legacy["enabled"].(bool)
	if !explicit {
		enabled, explicit = parent.Extra["openai_oauth_legacy_images"].(bool)
	}
	if !explicit {
		if opts.LegacyDefaultEnabled == nil {
			return nil, infraerrors.BadRequest("WEB_IMAGE_LEGACY_DEFAULT_REQUIRED", "supply legacy_default_enabled from the old deployment to resolve inherited enablement")
		}
		enabled = *opts.LegacyDefaultEnabled
	}
	plan := &WebImageShadowConversion{ParentID: parentID, Enabled: enabled, Concurrency: 1,
		Priority: parent.Priority, GroupIDs: append([]int64{}, parent.GroupIDs...), ModelOptions: map[string]any{}}
	for _, config := range []map[string]any{opts.LegacyDefaults, legacy} {
		for _, key := range []string{"model_mode", "model", "thinking_effort", "require_download_attachment"} {
			if value, ok := config[key]; ok {
				plan.ModelOptions[key] = value
			}
		}
		if raw, ok := config["max_inflight"]; ok {
			data, marshalErr := json.Marshal(raw)
			if marshalErr != nil || json.Unmarshal(data, &plan.Concurrency) != nil || plan.Concurrency <= 0 {
				return nil, infraerrors.BadRequest("WEB_IMAGE_SHADOW_CONCURRENCY", "legacy max_inflight must be a positive integer")
			}
		}
	}
	if plan.ModelOptions["model_mode"] == "auto" {
		plan.ModelOptions["model_mode"] = "recommended"
	}
	children, err := s.accountRepo.ListShadowsByParent(ctx, parentID)
	if err != nil {
		return nil, err
	}
	if existing := findShadowDimension(children, QuotaDimensionWebImages); existing != nil {
		plan.ShadowID = existing.ID
		return plan, nil
	}
	if !enabled {
		return plan, nil
	}
	if err := ValidateWebImageShadowParent(parent); err != nil {
		return nil, err
	}
	if !opts.Apply {
		return plan, nil
	}
	shadow, err := s.createWebImageShadow(ctx, parent, WebImageShadowOptions{
		Concurrency: &plan.Concurrency, Priority: &plan.Priority, GroupIDs: &plan.GroupIDs,
	}, plan.ModelOptions)
	if err != nil {
		// Concurrent apply is idempotent once the database uniqueness guard wins.
		if current, qerr := s.accountRepo.ListShadowsByParent(ctx, parentID); qerr == nil {
			if existing := findShadowDimension(current, QuotaDimensionWebImages); existing != nil {
				plan.ShadowID = existing.ID
				return plan, nil
			}
		}
		return nil, fmt.Errorf("convert Web image shadow: %w", err)
	}
	plan.ShadowID, plan.Applied = shadow.ID, true
	return plan, nil
}
