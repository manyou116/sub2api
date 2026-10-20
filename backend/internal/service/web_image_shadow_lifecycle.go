package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// WebImageShadowOptions preserves explicit zero priority and empty group bindings.
type WebImageShadowOptions struct {
	Name        string   `json:"name"`
	Concurrency *int     `json:"concurrency"`
	Priority    *int     `json:"priority"`
	GroupIDs    *[]int64 `json:"group_ids"`
	StartPaused bool     `json:"-"` // Internal backup restore; unavailable to create API input.
}

func ValidateWebImageShadowParent(parent *Account) error {
	if parent == nil || parent.IsShadow() || !parent.IsOpenAIOAuthLike() ||
		strings.EqualFold(strings.TrimSpace(parent.GetCredential("auth_mode")), OpenAIAuthModeAgentIdentity) ||
		strings.TrimSpace(parent.GetOpenAIAccessToken()) == "" {
		return infraerrors.BadRequest("WEB_IMAGE_SHADOW_INVALID_PARENT", "Web images require a real OpenAI OAuth or setup-token parent with an access token; agent identities are unsupported")
	}
	return nil
}

// Ordinary image quota is independent of text overload and the parent's scheduler switch.
func (a *Account) IsCredentialUsableForWebImages() bool {
	if a == nil || !a.IsActive() {
		return false
	}
	now := time.Now()
	if a.AutoPauseOnExpired && a.ExpiresAt != nil && !now.Before(*a.ExpiresAt) {
		return false
	}
	return a.TempUnschedulableUntil == nil || !now.Before(*a.TempUnschedulableUntil) ||
		IsOpenAICapacityCooldownReason(a.TempUnschedulableReason)
}

func findShadowDimension(shadows []*Account, dimension string) *Account {
	for _, shadow := range shadows {
		if shadow != nil && shadow.QuotaDimension == dimension {
			return shadow
		}
	}
	return nil
}

func (s *adminServiceImpl) validateWebImageShadowUpdate(ctx context.Context, account *Account, input *UpdateAccountInput) error {
	if account.IsWebImageShadow() && input.Concurrency != nil && *input.Concurrency <= 0 {
		return infraerrors.BadRequest("WEB_IMAGE_SHADOW_CONCURRENCY", "Web image shadow concurrency must be positive")
	}
	if account.IsWebImageShadow() {
		if options, provided := input.Extra["openai_web_images"]; provided {
			if err := validateWebImageOptions(options); err != nil {
				return err
			}
		}
	}
	_, changesToken := input.Credentials["access_token"]
	_, changesAuthMode := input.Credentials[openAIAuthModeCredentialKey]
	changesType := input.Type != "" && input.Type != account.Type
	if account.IsShadow() || !account.IsOpenAIOAuthLike() || (!changesType && !changesToken && !changesAuthMode) {
		return nil
	}
	candidate := *account
	if input.Type != "" {
		candidate.Type = input.Type
	}
	if len(input.Credentials) > 0 {
		candidate.Credentials = MergePreservingSensitiveCreds(account.Credentials, input.Credentials)
	}
	if !changesType && ValidateWebImageShadowParent(&candidate) == nil {
		return nil
	}
	children, err := s.accountRepo.ListShadowsByParent(ctx, account.ID)
	if err != nil {
		return err
	}
	for _, child := range children {
		if child.IsSparkShadow() && !candidate.IsOpenAIOAuth() {
			return infraerrors.BadRequest("SPARK_SHADOW_PARENT_IMMUTABLE_TYPE", "a parent with a Spark shadow must remain OpenAI OAuth")
		}
		if child.IsWebImageShadow() {
			if err := ValidateWebImageShadowParent(&candidate); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *adminServiceImpl) CreateWebImageShadow(ctx context.Context, parentID int64, opts WebImageShadowOptions) (*Account, error) {
	parent, err := s.accountRepo.GetByID(ctx, parentID)
	if err != nil {
		return nil, err
	}
	return s.createWebImageShadow(ctx, parent, opts, nil)
}

func (s *adminServiceImpl) createWebImageShadow(ctx context.Context, parent *Account, opts WebImageShadowOptions, modelOptions map[string]any) (*Account, error) {
	if err := ValidateWebImageShadowParent(parent); err != nil {
		return nil, err
	}
	if err := validateWebImageOptions(modelOptions); err != nil {
		return nil, err
	}
	children, err := s.accountRepo.ListShadowsByParent(ctx, parent.ID)
	if err != nil {
		return nil, err
	}
	conflict := func() error {
		return infraerrors.New(http.StatusConflict, "WEB_IMAGE_SHADOW_ALREADY_EXISTS", "parent account already has a Web image shadow")
	}
	if findShadowDimension(children, QuotaDimensionWebImages) != nil {
		return nil, conflict()
	}
	concurrency, priority := 1, parent.Priority
	if opts.Concurrency != nil {
		concurrency = *opts.Concurrency
	}
	if concurrency <= 0 {
		return nil, infraerrors.BadRequest("WEB_IMAGE_SHADOW_CONCURRENCY", "Web image shadow concurrency must be positive")
	}
	if opts.Priority != nil {
		priority = *opts.Priority
	}
	groupIDs := append([]int64{}, parent.GroupIDs...)
	if opts.GroupIDs != nil {
		groupIDs = append([]int64{}, (*opts.GroupIDs)...)
	}
	if len(groupIDs) > 0 && s.groupRepo != nil {
		if err := s.validateGroupIDsExist(ctx, groupIDs); err != nil {
			return nil, err
		}
	}
	if err := s.ValidateAccountGroupBindings(ctx, groupIDs); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		name = parent.Name + " (Web Images)"
	}
	if runes := []rune(name); len(runes) > 100 {
		name = string(runes[:100])
	}
	shadow := &Account{
		Name: name, Platform: PlatformOpenAI, Type: parent.Type,
		ParentAccountID: &parent.ID, QuotaDimension: QuotaDimensionWebImages,
		Concurrency: concurrency, Priority: priority, ProxyID: parent.ProxyID,
		Status: StatusActive, Schedulable: !opts.StartPaused,
		Credentials: map[string]any{"model_mapping": map[string]any{"gpt-image-2": "gpt-image-2"}},
	}
	if len(modelOptions) > 0 {
		shadow.Extra = map[string]any{"openai_web_images": modelOptions}
	}
	if err := s.accountRepo.Create(ctx, shadow); err != nil {
		if current, qerr := s.accountRepo.ListShadowsByParent(ctx, parent.ID); qerr == nil && findShadowDimension(current, QuotaDimensionWebImages) != nil {
			return nil, conflict()
		}
		return nil, err
	}
	if len(groupIDs) > 0 {
		if err := s.accountRepo.BindGroups(ctx, shadow.ID, groupIDs); err != nil {
			rollbackErr := s.accountRepo.Delete(context.WithoutCancel(ctx), shadow.ID)
			return nil, fmt.Errorf("bind Web image shadow groups: %w (rollback: %v)", err, rollbackErr)
		}
	}
	shadow.GroupIDs = groupIDs
	return shadow, nil
}

func validateWebImageOptions(value any) error {
	if value == nil {
		return nil
	}
	options, ok := value.(map[string]any)
	invalid := func() error {
		return infraerrors.BadRequest("WEB_IMAGE_OPTIONS_INVALID", "Web image settings accept model_mode (auto/fixed), model, thinking_effort and require_download_attachment only")
	}
	if !ok {
		return invalid()
	}
	for key, value := range options {
		switch key {
		case "model_mode":
			if value != "auto" && value != "fixed" {
				return invalid()
			}
		case "model", "thinking_effort":
			text, ok := value.(string)
			if !ok || len(text) > 200 || strings.ContainsAny(text, "\r\n") {
				return invalid()
			}
		case "require_download_attachment":
			if _, ok := value.(bool); !ok {
				return invalid()
			}
		default:
			return invalid()
		}
	}
	return nil
}
