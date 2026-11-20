//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func (r *webImageTestRepo) ConfiguredImageShadowModes(_ context.Context, groupID *int64, includeGrouped bool) ([]string, error) {
	modes := map[string]bool{}
	for _, account := range r.accounts {
		if account.IsImageShadow() {
			modes[imageShadowMode(account)] = true
		}
	}
	result := make([]string, 0, len(modes))
	for mode := range modes {
		result = append(result, mode)
	}
	return result, nil
}

type imageModeRepoSpy struct {
	AccountRepository
	calls          int
	groupID        *int64
	includeGrouped bool
	modes          []string
}

func (r *imageModeRepoSpy) ConfiguredImageShadowModes(_ context.Context, groupID *int64, includeGrouped bool) ([]string, error) {
	r.calls++
	r.groupID, r.includeGrouped = groupID, includeGrouped
	return r.modes, nil
}

func TestImageShadowRoutingQueriesOnlyGenericOnce(t *testing.T) {
	repo := &imageModeRepoSpy{modes: []string{ImageShadowModeWeb}}
	s := &OpenAIGatewayService{accountRepo: repo, webImages: &OpenAIWebImageService{}}
	groupID := int64(42)
	for _, model := range []string{"gpt-image-1", "gpt-image-1-mini", "gpt-image-2-web", "gpt-image-2-work"} {
		_, err := s.PrepareImageShadowRoutingContext(context.Background(), &groupID, model)
		require.NoError(t, err)
	}
	require.Zero(t, repo.calls)
	ctx, err := s.PrepareImageShadowRoutingContext(context.Background(), &groupID, "gpt-image-2")
	require.NoError(t, err)
	require.Equal(t, 1, repo.calls)
	require.Equal(t, &groupID, repo.groupID)
	require.False(t, repo.includeGrouped)
	repo.modes = []string{ImageShadowModeWork}
	for range 3 {
		next, err := s.imageShadowRoutingContext(ctx, &groupID, "gpt-image-2")
		require.NoError(t, err)
		require.Same(t, ctx, next)
	}
	require.Equal(t, 1, repo.calls, "failover must not rediscover or switch the configured resource")
	parent := int64(1)
	web := &Account{ParentAccountID: &parent, QuotaDimension: QuotaDimensionWebImages}
	require.False(t, imageShadowRouteMismatch(ctx, web, "gpt-image-2"))
	require.True(t, imageShadowRouteMismatch(ctx, web, "gpt-image-2-work"), "channel mapping cannot change an already selected mode")
}

func TestImageShadowRoutingScopeMatchesRunMode(t *testing.T) {
	group := int64(42)
	for _, tc := range []struct {
		name      string
		runMode   string
		groupID   *int64
		wantGroup *int64
		wantAll   bool
	}{
		{"standard_group", config.RunModeStandard, &group, &group, false},
		{"standard_ungrouped", config.RunModeStandard, nil, nil, false},
		{"simple_all", config.RunModeSimple, &group, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &imageModeRepoSpy{}
			s := &OpenAIGatewayService{accountRepo: repo, webImages: &OpenAIWebImageService{}, cfg: &config.Config{RunMode: tc.runMode}}
			ctx, err := s.PrepareImageShadowRoutingContext(context.Background(), tc.groupID, "gpt-image-2")
			require.NoError(t, err)
			require.Equal(t, tc.wantGroup, repo.groupID)
			require.Equal(t, tc.wantAll, repo.includeGrouped)
			_, err = s.imageShadowRoutingContext(ctx, tc.groupID, "gpt-image-2")
			require.NoError(t, err)
			require.Equal(t, 1, repo.calls, "empty mode configuration is memoized too")
		})
	}
}
