package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// SourceUser is an immutable attribution snapshot, never a local billing identity.
type SourceUser struct {
	SourceID   string `json:"source_id"`
	SourceName string `json:"source_name,omitempty"`
	UserID     string `json:"user_id"`
	Username   string `json:"username,omitempty"`
	TokenID    string `json:"token_id,omitempty"`
	RequestID  string `json:"request_id,omitempty"`
}

type SourceUserIntegration struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	APIKeyIDs []int64 `json:"api_key_ids"`
}

var SourceUserIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

type sourceUserContextKey struct{}

func WithSourceUser(ctx context.Context, user *SourceUser) context.Context {
	if user == nil {
		return ctx
	}
	return context.WithValue(ctx, sourceUserContextKey{}, *user)
}

func SourceUserFromContext(ctx context.Context) *SourceUser {
	if ctx == nil {
		return nil
	}
	user, ok := ctx.Value(sourceUserContextKey{}).(SourceUser)
	if !ok {
		return nil
	}
	return &user
}

type cachedSourceUserIntegrations struct {
	sources []SourceUserIntegration
	err     error
	until   time.Time
}

func (s *SettingService) GetSourceUserIntegrations(ctx context.Context) ([]SourceUserIntegration, error) {
	if s == nil || s.settingRepo == nil {
		return []SourceUserIntegration{}, nil
	}
	if v, _ := s.sourceUserIntegrationsCache.Load().(*cachedSourceUserIntegrations); v != nil && time.Now().Before(v.until) {
		return v.sources, v.err
	}
	s.sourceUserIntegrationsMu.Lock()
	defer s.sourceUserIntegrationsMu.Unlock()
	if v, _ := s.sourceUserIntegrationsCache.Load().(*cachedSourceUserIntegrations); v != nil && time.Now().Before(v.until) {
		return v.sources, v.err
	}
	ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	raw, err := s.settingRepo.GetValue(ctx, "source_user_integrations")
	if errors.Is(err, ErrSettingNotFound) {
		err = nil
	}
	sources := []SourceUserIntegration{}
	if err == nil && raw != "" {
		err = json.Unmarshal([]byte(raw), &sources)
	}
	if err == nil {
		err = ValidateSourceUserIntegrations(sources)
	}
	if err != nil {
		sources = []SourceUserIntegration{}
	}
	ttl := 30 * time.Second
	if err != nil {
		ttl = time.Second
	}
	s.sourceUserIntegrationsCache.Store(&cachedSourceUserIntegrations{sources, err, time.Now().Add(ttl)})
	return sources, err
}

func ValidateSourceUserIntegrations(sources []SourceUserIntegration) error {
	if len(sources) > 32 {
		return fmt.Errorf("at most 32 source integrations are allowed")
	}
	ids := map[string]bool{}
	keys := map[int64]bool{}
	for _, source := range sources {
		if !SourceUserIDPattern.MatchString(source.ID) || ids[source.ID] || len(source.Name) > 128 || len(source.APIKeyIDs) > 256 {
			return fmt.Errorf("invalid or duplicate source integration")
		}
		ids[source.ID] = true
		for _, key := range source.APIKeyIDs {
			if key <= 0 || keys[key] {
				return fmt.Errorf("each API key must belong to exactly one source")
			}
			keys[key] = true
		}
	}
	return nil
}

func (s *SettingService) SetSourceUserIntegrations(ctx context.Context, sources []SourceUserIntegration) error {
	if err := ValidateSourceUserIntegrations(sources); err != nil {
		return err
	}
	if sources == nil {
		sources = []SourceUserIntegration{}
	}
	data, err := json.Marshal(sources)
	if err != nil {
		return err
	}
	s.sourceUserIntegrationsMu.Lock()
	defer s.sourceUserIntegrationsMu.Unlock()
	if err := s.settingRepo.Set(ctx, "source_user_integrations", string(data)); err != nil {
		return err
	}
	s.sourceUserIntegrationsCache.Store(&cachedSourceUserIntegrations{sources, nil, time.Now().Add(30 * time.Second)})
	return nil
}
