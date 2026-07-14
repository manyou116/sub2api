package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestWebImagesBypassTextRateLimitForScheduling(t *testing.T) {
	reset := time.Now().Add(24 * time.Hour)
	acc := &Account{
		ID: 74, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
		RateLimitResetAt: &reset,
		Extra:            map[string]any{"openai_web_images": map[string]any{"enabled": true, "max_inflight": 3}},
	}
	// Text path must still see rate limit.
	require.False(t, acc.IsSchedulable())
	// Web image path ignores text rate limit.
	require.True(t, acc.IsSchedulableIgnoringTextRateLimit())

	svc := &OpenAIGatewayService{
		webImages: NewOpenAIWebImagesService(&config.Config{Gateway: config.GatewayConfig{OpenAIWebImages: config.OpenAIWebImagesConfig{
			InflightBackend: "memory", DefaultMaxInflight: 1}}}, nil, nil),
	}
	// Simulate text 429 memory fuse.
	svc.BlockAccountScheduling(acc, time.Now().Add(time.Hour), "429")
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(acc))
	require.False(t, svc.isOpenAIAccountRuntimeBlockedForRequest(acc, OpenAIImagesCapabilityBasic, "gpt-image-2"))
	require.True(t, svc.shouldBypassTextRateLimitForWebImages(acc, OpenAIImagesCapabilityBasic, "gpt-image-2"))
	require.True(t, svc.isAccountSchedulableForOpenAIRequest(context.Background(), acc, OpenAIImagesCapabilityBasic))

	// Chat must still be blocked.
	require.False(t, svc.isAccountSchedulableForOpenAIRequest(context.Background(), acc, ""))
	require.True(t, svc.isOpenAIAccountRuntimeBlockedForRequest(acc, "", "gpt-5.4"))
}

func TestImageEligibilityIgnoresCodexQuotaAutoPause(t *testing.T) {
	reset := time.Now().Add(24 * time.Hour)
	acc := &Account{
		ID: 74, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
		RateLimitResetAt: &reset,
		Extra: map[string]any{
			"openai_web_images":          map[string]any{"enabled": true, "max_inflight": 3},
			"codex_7d_used_percent":      100,
			"codex_primary_used_percent": 100,
		},
	}
	require.True(t, isOpenAICompatibleAccountEligibleForRequest(context.Background(), acc, PlatformOpenAI, "gpt-image-2", false, ""))
	// Text model remains blocked by RateLimitResetAt via IsSchedulableForModel.
	require.False(t, isOpenAICompatibleAccountEligibleForRequest(context.Background(), acc, PlatformOpenAI, "gpt-5.4", false, ""))
}
