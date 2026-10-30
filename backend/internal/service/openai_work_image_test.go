//go:build unit

package service

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service/openaiimages/webdriver"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestWorkImageSharesParentCodexButNotWebQuota(t *testing.T) {
	s, repo, parent, work, cache := webImageFixture(t)
	work.QuotaDimension = QuotaDimensionWorkImages
	parent.Schedulable = false
	until := time.Now().Add(time.Hour)
	zero := 0
	repo.cooldowns[parent.ID] = until
	cache.statuses[parent.ID] = WebImageStatus{QuotaKnown: true, Remaining: &zero, ResetAt: &until}
	s.driver = &fakeWebImageDriver{probe: func(context.Context, webdriver.Auth) (*webdriver.Quota, error) {
		t.Fatal("WM must not probe Web quota")
		return nil, nil
	}, generate: func(_ context.Context, auth webdriver.Auth, request webdriver.GenerateRequest) (*webdriver.GenerateResult, error) {
		require.Equal(t, "parent-token", auth.AccessToken)
		require.Equal(t, "work", request.Mode)
		require.True(t, *request.RequireDownloadAttachment)
		require.Equal(t, "gpt-5.6-luna-wm", request.Model)
		return &webdriver.GenerateResult{Data: []webdriver.ImageData{{B64JSON: "aW1hZ2U=", Width: 1254, Height: 1254}}}, nil
	}}
	_, _, err := s.generate(context.Background(), work, &OpenAIImagesRequest{Model: "gpt-image-2-work", Prompt: "test"})
	require.NoError(t, err)
	_, err = s.Probe(context.Background(), work.ID)
	require.ErrorContains(t, err, "shares parent Codex")
	parent.RateLimitResetAt = &until
	_, err = s.eligible(context.Background(), work)
	require.ErrorContains(t, err, "text quota")
	web := *work
	web.QuotaDimension = QuotaDimensionWebImages
	delete(repo.cooldowns, parent.ID)
	delete(cache.statuses, parent.ID)
	_, err = s.eligible(context.Background(), &web)
	require.NoError(t, err)
}

func TestWorkImageQuotaFailureWritesParentAndPreservesTerminal429(t *testing.T) {
	for _, code := range []string{"usage_limit_reached", "model_quota_exhausted"} {
		t.Run(code, func(t *testing.T) {
			s, repo, parent, work, _ := webImageFixture(t)
			work.QuotaDimension = QuotaDimensionWorkImages
			s.gateway.rateLimitService = NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
			until := time.Now().Add(time.Hour)
			s.driver = &fakeWebImageDriver{generate: func(context.Context, webdriver.Auth, webdriver.GenerateRequest) (*webdriver.GenerateResult, error) {
				failure := webdriver.NewError(webdriver.ErrorKindRateLimited, "sse", "Quota exhausted", 429, true)
				failure.Code = code
				failure.ResetAt = &until
				failure.Submission = webdriver.SubmissionAccepted
				return nil, failure
			}}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest("POST", "/v1/images/generations", nil)
			_, err := s.gateway.forwardWebImage(context.Background(), c, work, &OpenAIImagesRequest{Model: "gpt-image-2-work", Prompt: "test"}, "")
			require.Error(t, err)
			require.Equal(t, 429, recorder.Code)
			require.Empty(t, repo.cooldowns)
			require.Nil(t, work.RateLimitResetAt)
			if code == "usage_limit_reached" {
				require.NotNil(t, parent.RateLimitResetAt)
			} else {
				require.False(t, parent.IsSchedulableForModel("gpt-5.6-luna"))
			}
		})
	}
}

func TestImageShadowCatalogAndModeAreValidated(t *testing.T) {
	require.NoError(t, validateImageShadowOptions(map[string]any{"model_mode": "recommended", "model": "old-model", "thinking_effort": "extended"}, "work"))
	require.Error(t, validateImageShadowOptions(map[string]any{"model_mode": "fixed", "model": "gpt-5.6-luna-wm"}, "web"))
	require.Error(t, validateImageShadowOptions(map[string]any{"model_mode": "fixed", "model": "gpt-5.6-luna-wm", "thinking_effort": "xhigh"}, "work"))
	require.Error(t, validateImageShadowOptions(map[string]any{"require_download_attachment": false}, "work"))
	require.NoError(t, validateImageShadowOptions(map[string]any{"model_mode": "fixed", "model": "auto"}, "web"))
}

func TestWorkImageCodexSnapshotUsesRatioAndExpires(t *testing.T) {
	for _, tc := range []struct {
		name    string
		used    float64
		reset   time.Duration
		blocked bool
	}{
		{"not_exhausted", 99, time.Hour, false},
		{"exhausted", 100, time.Hour, true},
		{"reset", 100, -time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, parent, work, _ := webImageFixture(t)
			work.QuotaDimension = QuotaDimensionWorkImages
			parent.Schedulable = false
			parent.Extra["codex_5h_used_percent"] = tc.used
			parent.Extra["codex_5h_reset_at"] = time.Now().Add(tc.reset).Format(time.RFC3339)
			parent.Extra["codex_usage_updated_at"] = time.Now().Format(time.RFC3339)
			parent.Extra["auto_pause_5h_disabled"] = true
			_, err := s.eligible(context.Background(), work)
			if tc.blocked {
				require.ErrorContains(t, err, "Codex quota is exhausted")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestImageShadowRoutingRequiresChoiceEvenWhenOtherModePaused(t *testing.T) {
	s, repo, parent, web, _ := webImageFixture(t)
	work := *web
	work.ID = 3
	work.QuotaDimension = QuotaDimensionWorkImages
	work.Schedulable = false
	repo.accounts[3] = &work
	_, err := s.gateway.imageShadowRoutingContext(context.Background(), nil, "gpt-image-2")
	require.ErrorIs(t, err, ErrImageShadowModeRequired)
	ctx, err := s.gateway.imageShadowRoutingContext(context.Background(), nil, "gpt-image-2-web")
	require.NoError(t, err)
	require.False(t, imageShadowRouteMismatch(ctx, web, "gpt-image-2"))
	require.True(t, imageShadowRouteMismatch(ctx, &work, "gpt-image-2"))
	require.True(t, imageShadowRouteMismatch(ctx, parent, "gpt-image-2"))
}
