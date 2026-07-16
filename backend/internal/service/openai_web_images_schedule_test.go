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

func TestIsSchedulableIgnoringTextRateLimitHonorsWebImageCooldown(t *testing.T) {
	reset := time.Now().Add(15 * time.Hour)
	acc := &Account{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true,
		WebImageRateLimitResetAt: &reset,
		Extra:                    map[string]any{"openai_web_images": map[string]any{"enabled": true}},
	}
	require.False(t, acc.IsSchedulableIgnoringTextRateLimit())
	require.True(t, acc.IsWebImageRateLimited())

	// Text RL alone still allowed for web path.
	textRL := time.Now().Add(time.Hour)
	acc2 := &Account{
		ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true,
		RateLimitResetAt: &textRL,
		Extra:            map[string]any{"openai_web_images": map[string]any{"enabled": true}},
	}
	require.True(t, acc2.IsSchedulableIgnoringTextRateLimit())
	require.False(t, acc2.IsWebImageRateLimited())
}

func TestShouldSkipAccountTextSlotForWebImages(t *testing.T) {
	t.Parallel()
	svc := &OpenAIGatewayService{
		webImages: NewOpenAIWebImagesService(&config.Config{Gateway: config.GatewayConfig{OpenAIWebImages: config.OpenAIWebImagesConfig{
			InflightBackend: "memory", DefaultMaxInflight: 1}}}, nil, nil),
	}
	webAcc := &Account{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
		Extra: map[string]any{"openai_web_images": map[string]any{"enabled": true, "max_inflight": 3}},
	}
	plainAcc := &Account{
		ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
	}

	require.True(t, shouldSkipAccountTextSlotForWebImages(svc, context.Background(), webAcc, "gpt-image-2", ""))
	require.True(t, shouldSkipAccountTextSlotForWebImages(svc, context.Background(), webAcc, "gpt-5.4", OpenAIImagesCapabilityBasic))
	require.True(t, shouldSkipAccountTextSlotForWebImages(svc, WithOpenAIImageGenerationIntent(context.Background()), webAcc, "gpt-5.4", ""))
	require.False(t, shouldSkipAccountTextSlotForWebImages(svc, context.Background(), webAcc, "gpt-5.4", ""))
	require.False(t, shouldSkipAccountTextSlotForWebImages(svc, context.Background(), plainAcc, "gpt-image-2", OpenAIImagesCapabilityBasic))
}

func TestReleaseAccountTextSlotIfWebImages(t *testing.T) {
	t.Parallel()
	svc := &OpenAIGatewayService{
		webImages: NewOpenAIWebImagesService(&config.Config{Gateway: config.GatewayConfig{OpenAIWebImages: config.OpenAIWebImagesConfig{
			InflightBackend: "memory", DefaultMaxInflight: 1}}}, nil, nil),
	}
	released := false
	webAcc := &Account{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
		Extra: map[string]any{"openai_web_images": map[string]any{"enabled": true}},
	}
	sel := &AccountSelectionResult{
		Account:  webAcc,
		Acquired: true,
		ReleaseFunc: func() {
			released = true
		},
	}
	ReleaseAccountTextSlotIfWebImages(svc, sel)
	require.True(t, released)
	require.False(t, sel.Acquired)
	require.Nil(t, sel.ReleaseFunc)
}

func TestIsWebImageInflightFullForRequest_SkipsSaturatedWebAccount(t *testing.T) {
	t.Parallel()
	webSvc := NewOpenAIWebImagesService(&config.Config{Gateway: config.GatewayConfig{OpenAIWebImages: config.OpenAIWebImagesConfig{
		InflightBackend: "memory", DefaultMaxInflight: 1, UnknownQuotaPolicy: "optimistic",
	}}}, nil, nil)
	svc := &OpenAIGatewayService{webImages: webSvc}
	acc := &Account{
		ID: 11, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
		Extra: map[string]any{"openai_web_images": map[string]any{"enabled": true, "max_inflight": 1}},
	}
	scheduler := &defaultOpenAIAccountScheduler{service: svc}
	req := OpenAIAccountScheduleRequest{
		RequestedModel:          "gpt-image-2",
		RequiredImageCapability: OpenAIImagesCapabilityBasic,
	}

	require.True(t, svc.isAccountSchedulableForOpenAIRequest(context.Background(), acc, OpenAIImagesCapabilityBasic))
	require.True(t, scheduler.isAccountRequestCompatible(context.Background(), acc, req))

	ok, err := webSvc.Acquire(context.Background(), acc.ID, 1, "busy")
	require.NoError(t, err)
	require.True(t, ok)

	require.True(t, webSvc.IsInflightFull(context.Background(), acc))
	require.False(t, svc.isAccountSchedulableForOpenAIRequest(context.Background(), acc, OpenAIImagesCapabilityBasic))
	require.False(t, scheduler.isAccountRequestCompatible(context.Background(), acc, req))
	// Chat/text must not be blocked by web inflight saturation.
	require.False(t, svc.isWebImageInflightFullForRequest(context.Background(), acc, "", "gpt-5.4"))

	webSvc.Release(context.Background(), acc.ID, "busy")
	require.True(t, svc.isAccountSchedulableForOpenAIRequest(context.Background(), acc, OpenAIImagesCapabilityBasic))
	require.True(t, scheduler.isAccountRequestCompatible(context.Background(), acc, req))
}

func TestIsWebImageInflightFullForRequest_IgnoresNonWebAccounts(t *testing.T) {
	t.Parallel()
	webSvc := NewOpenAIWebImagesService(&config.Config{Gateway: config.GatewayConfig{OpenAIWebImages: config.OpenAIWebImagesConfig{
		InflightBackend: "memory", DefaultMaxInflight: 1,
	}}}, nil, nil)
	svc := &OpenAIGatewayService{webImages: webSvc}
	plain := &Account{
		ID: 12, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
	}
	_, _ = webSvc.Acquire(context.Background(), plain.ID, 1, "x")
	require.False(t, webSvc.IsInflightFull(context.Background(), plain))
	require.False(t, svc.isWebImageInflightFullForRequest(context.Background(), plain, OpenAIImagesCapabilityBasic, "gpt-image-2"))
}
