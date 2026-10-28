package service

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service/openaiimages/webdriver"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

func TestWebImageWorkQuotaAdmission(t *testing.T) {
	for _, model := range []string{"auto", "gpt-5.6-luna-wm"} {
		for _, blocked := range []bool{false, true} {
			t.Run(model+"/"+map[bool]string{false: "healthy", true: "limited"}[blocked], func(t *testing.T) {
				ctx := context.Background()
				account := webImageCapacityAccount(42)
				account.Extra["openai_web_images"] = map[string]any{"enabled": true, "model_mode": "fixed", "model": model}
				if blocked {
					until := time.Now().Add(time.Hour)
					account.RateLimitResetAt = &until
				}
				cfg := webImgTestCfg("memory")
				cfg.Gateway.OpenAIWebImages.DefaultUpstreamModel = "auto"
				web := NewOpenAIWebImagesService(cfg, nil, nil)
				gw := &OpenAIGatewayService{webImages: web}
				wantBlocked := model != "auto" && blocked
				require.Equal(t, !wantBlocked, gw.isAccountSchedulableForOpenAIRequest(ctx, account, OpenAIImagesCapabilityNative))
				require.Equal(t, wantBlocked, gw.isOpenAIAccountRuntimeBlockedForRequest(ctx, account, "", "gpt-image-2"), "legacy/sticky gate")
				scheduler := &defaultOpenAIAccountScheduler{service: gw}
				require.Equal(t, !wantBlocked, scheduler.isAccountRequestCompatible(ctx, account, OpenAIAccountScheduleRequest{RequestedModel: "gpt-image-2", RequiredImageCapability: OpenAIImagesCapabilityNative}))
				require.True(t, shouldSkipAccountTextSlotForWebImages(gw, ctx, account, "gpt-image-2", OpenAIImagesCapabilityNative), "quota sharing must not change image concurrency")
				web.setCooldownCache(ctx, account.ID, time.Now().Add(time.Minute))
				require.False(t, gw.isAccountSchedulableForOpenAIRequest(ctx, account, OpenAIImagesCapabilityNative), "both modes honor image backoff")
			})
		}
	}
}

func TestWebImageWorkQuotaRecheckBeforeUpstream(t *testing.T) {
	selected := webImageCapacityAccount(42)
	current := *selected
	until := time.Now().Add(time.Hour)
	current.RateLimitResetAt = &until
	current.Extra = map[string]any{"openai_web_images": map[string]any{"enabled": true, "model_mode": "fixed", "model": "gpt-5.6-luna-wm"}}
	web := NewOpenAIWebImagesService(webImgTestCfg("memory"), nil, &webImgAccountRepo{accounts: map[int64]*Account{42: &current}})
	web.Driver().ClientFactory = func(string) (*req.Client, error) { t.Fatal("quota-blocked account reached upstream"); return nil, nil }
	gw := &OpenAIGatewayService{webImages: web}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	_, err := gw.forwardOpenAIImagesLegacyWeb(context.Background(), c, selected, &OpenAIImagesRequest{Model: "gpt-image-2"}, "")
	var capacity *OpenAIWebImagesCapacityError
	require.ErrorAs(t, err, &capacity)
	require.Zero(t, web.ParseAccountConfig(&current).Stats.Fail)
}

func TestWebImageUsageLimitBackoffPreservesQuota(t *testing.T) {
	for _, message := range []string{"You've hit your usage limit", "The usage limit has been reached"} {
		t.Run(message, func(t *testing.T) {
			mr := miniredis.RunT(t)
			account := webImageCapacityAccount(42)
			web := NewOpenAIWebImagesService(webImgTestCfg("redis"), newWebImageTestRedisClient(t, mr.Addr()), &webImgAccountRepo{accounts: map[int64]*Account{42: account}})
			ctx := context.Background()
			web.setQuotaCache(ctx, 42, webImageQuotaCache{Remaining: 7, ProbedAt: time.Now()})
			web.MarkRateLimitFailure(ctx, account, &webdriver.Error{Kind: webdriver.ErrorKindRateLimited, Code: "work_usage_limit_exceeded", Message: message})
			q, known := web.getQuotaCache(ctx, 42)
			require.True(t, known)
			require.Equal(t, 7, q.Remaining)
			require.Nil(t, account.WebImageRateLimitResetAt)
			require.True(t, web.IsWebRateLimited(ctx, 42))
			mr.FastForward(91 * time.Second)
			require.False(t, web.IsWebRateLimited(ctx, 42), "cooldown expiry permits recovery")
		})
	}
}

func TestWebImageCooldownNeverShortens(t *testing.T) {
	for _, backend := range []string{"memory", "redis"} {
		t.Run(backend, func(t *testing.T) {
			web := NewOpenAIWebImagesService(webImgTestCfg(backend), nil, nil)
			if backend == "redis" {
				web.rdb = newWebImageTestRedisClient(t, miniredis.RunT(t).Addr())
			}
			ctx := context.Background()
			long := time.Now().Add(10 * time.Minute)
			var wg sync.WaitGroup
			for i := 0; i < 20; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					until := time.Now().Add(30 * time.Second)
					if i == 0 {
						until = long
					}
					web.setCooldownCache(ctx, 42, until)
				}(i)
			}
			wg.Wait()
			got, ok := web.getCooldownCache(ctx, 42)
			require.True(t, ok)
			require.WithinDuration(t, long, got, time.Millisecond)
			account := webImageCapacityAccount(42)
			durable := long.Add(time.Hour)
			account.WebImageRateLimitResetAt = &durable
			web.accountRepo = &webImgAccountRepo{accounts: map[int64]*Account{42: account}}
			got, ok = web.getCooldown(ctx, 42)
			require.True(t, ok)
			require.WithinDuration(t, durable, got, time.Nanosecond)
		})
	}
}
