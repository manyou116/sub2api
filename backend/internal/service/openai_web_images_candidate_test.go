package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type listRepoStub struct {
	webImgAccountRepo
	allowing []Account
	normal   []Account
}

func (r *listRepoStub) ListSchedulableByGroupIDAndPlatform(ctx context.Context, groupID int64, platform string) ([]Account, error) {
	return r.normal, nil
}
func (r *listRepoStub) ListSchedulableByPlatform(ctx context.Context, platform string) ([]Account, error) {
	return r.normal, nil
}
func (r *listRepoStub) ListSchedulableUngroupedByPlatform(ctx context.Context, platform string) ([]Account, error) {
	return r.normal, nil
}
func (r *listRepoStub) ListSchedulableByGroupIDAndPlatforms(ctx context.Context, groupID int64, platforms []string) ([]Account, error) {
	return r.normal, nil
}
func (r *listRepoStub) ListActiveAllowingTextRateLimitByGroupIDAndPlatforms(ctx context.Context, groupID int64, platforms []string) ([]Account, error) {
	return r.allowing, nil
}
func (r *listRepoStub) ListActiveAllowingTextRateLimitByPlatforms(ctx context.Context, platforms []string) ([]Account, error) {
	return r.allowing, nil
}

func TestListAccountsAllowingTextRateLimitIncludesRLAccount(t *testing.T) {
	reset := time.Now().Add(24 * time.Hour)
	rl := Account{ID: 74, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, RateLimitResetAt: &reset, Extra: map[string]any{"openai_web_images": map[string]any{"enabled": true, "max_inflight": 3}}}
	repo := &listRepoStub{normal: nil, allowing: []Account{rl}}
	svc := &OpenAIGatewayService{
		accountRepo: repo,
		webImages:   NewOpenAIWebImagesService(&config.Config{Gateway: config.GatewayConfig{OpenAIWebImages: config.OpenAIWebImagesConfig{Enabled: true, InflightBackend: "memory"}}}, nil, nil),
		cfg:         &config.Config{},
	}
	gid := int64(2)
	got, err := svc.listAccountsAllowingTextRateLimit(context.Background(), &gid, PlatformOpenAI)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, int64(74), got[0].ID)
	require.True(t, svc.isAccountSchedulableForOpenAIRequest(context.Background(), &got[0], OpenAIImagesCapabilityBasic))
	require.True(t, svc.isAccountSchedulableForOpenAIRequest(context.Background(), &got[0], OpenAIImagesCapabilityNative))
}
