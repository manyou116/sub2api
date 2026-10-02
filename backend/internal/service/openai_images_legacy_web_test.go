package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service/openaiimages/webdriver"
	"github.com/gin-gonic/gin"
	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

func TestOpenAIWebImageHTMLForbiddenFailsOverAndCoolsAccount(t *testing.T) {
	for _, tt := range []struct {
		name       string
		quotaKnown bool
		endpoint   string
	}{
		{name: "probe before generation"},
		{name: "generation requirements", quotaKnown: true},
		{name: "edit requirements", quotaKnown: true, endpoint: openAIImagesEditsEndpoint},
	} {
		t.Run(tt.name, func(t *testing.T) {
			account := &Account{
				ID: 15068, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
				Credentials: map[string]any{"access_token": "test-token"},
				Extra:       map[string]any{"openai_web_images": map[string]any{"enabled": true}},
			}
			repo := &webImgAccountRepo{accounts: map[int64]*Account{account.ID: account}}
			cfg := webImgTestCfg("memory")
			cfg.Gateway.OpenAIWebImages.UnknownQuotaPolicy = "optimistic"
			webImages := NewOpenAIWebImagesService(cfg, nil, repo)
			calls := 0
			webImages.driver = webdriver.NewDriver(func(string) (*req.Client, error) {
				client := req.C()
				client.GetTransport().WrapRoundTripFunc(func(http.RoundTripper) req.HttpRoundTripFunc {
					return func(*http.Request) (*http.Response, error) {
						calls++
						body := "<html><head><style global></style></head><body>blocked</body></html>"
						return &http.Response{
							StatusCode: http.StatusForbidden,
							Header:     http.Header{"Content-Type": []string{"text/html"}},
							Body:       io.NopCloser(strings.NewReader(body)),
						}, nil
					}
				})
				return client, nil
			})
			if tt.quotaKnown {
				webImages.setQuotaCache(context.Background(), account.ID, webImageQuotaCache{Remaining: 1, ProbedAt: time.Now()})
			}
			svc := &OpenAIGatewayService{accountRepo: repo, webImages: webImages}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
			parsed := &OpenAIImagesRequest{Endpoint: tt.endpoint, Model: "gpt-image-2", Prompt: "test", N: 1}

			result, err := svc.forwardOpenAIImagesLegacyWeb(c.Request.Context(), c, account, parsed, parsed.Model)
			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			require.Nil(t, result)
			require.Equal(t, http.StatusBadGateway, failover.StatusCode)
			require.False(t, failover.RetryableOnSameAccount)
			require.Equal(t, 1, calls, "do not retry an edge block on the same account")
			require.Empty(t, recorder.Body.String(), "allow the handler to switch accounts")
			require.True(t, webImages.IsWebRateLimited(context.Background(), account.ID))
			require.False(t, svc.isAccountSchedulableForOpenAIRequest(context.Background(), account, OpenAIImagesCapabilityBasic))
			require.Equal(t, int64(1), webImages.ParseAccountConfig(account).Stats.Fail)
			require.Nil(t, account.WebImageRateLimitResetAt, "edge cooldown must not consume daily image quota")

			_, err = svc.forwardOpenAIImagesLegacyWeb(c.Request.Context(), c, account, parsed, parsed.Model)
			require.ErrorAs(t, err, &failover)
			require.Equal(t, 1, calls, "cooldown skips a fresh probe on the next request")
		})
	}
}

type webImageAuthAccountRepo struct {
	AccountRepository
	setErrorCalls    int
	tempCalls        int
	updateExtraCalls int
}

func (r *webImageAuthAccountRepo) SetError(context.Context, int64, string) error {
	r.setErrorCalls++
	return nil
}

func (r *webImageAuthAccountRepo) SetTempUnschedulable(context.Context, int64, time.Time, string) error {
	r.tempCalls++
	return nil
}

func (r *webImageAuthAccountRepo) UpdateExtra(context.Context, int64, map[string]any) error {
	r.updateExtraCalls++
	return nil
}

func TestOpenAIWebImageAuthFailureUsesTextAccountPolicy(t *testing.T) {
	for _, tt := range []struct {
		name          string
		body          string
		wantSetError  int
		wantTempBlock int
	}{
		{
			name:         "revoked token permanently disables account",
			body:         `{"error":{"code":"token_invalidated","message":"token invalidated"}}`,
			wantSetError: 1,
		},
		{
			name:          "generic oauth 401 remains refreshable",
			body:          `{"error":{"message":"Unauthorized"}}`,
			wantTempBlock: 1,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := &webImageAuthAccountRepo{}
			svc := &OpenAIGatewayService{rateLimitService: &RateLimitService{accountRepo: repo, cfg: &config.Config{}}}
			account := &Account{
				ID:       9,
				Platform: PlatformOpenAI,
				Type:     AccountTypeOAuth,
				Credentials: map[string]any{
					"refresh_token": "refresh-token",
				},
			}

			shouldDisable := svc.handleOpenAIAccountUpstreamError(context.Background(), account, http.StatusUnauthorized, http.Header{}, []byte(tt.body), account.GetMappedModel("gpt-image-2"))

			require.True(t, shouldDisable)
			require.Equal(t, tt.wantSetError, repo.setErrorCalls)
			require.Equal(t, tt.wantTempBlock, repo.tempCalls)
		})
	}
}

func TestOpenAIWebImageAuthFailureAlwaysFailsOver(t *testing.T) {
	body := []byte(`{"error":{"code":"token_invalidated","message":"token invalidated"}}`)
	account := &Account{
		ID:       9,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"refresh_token": "refresh-token",
		},
	}

	t.Run("applies shared account state before failover", func(t *testing.T) {
		repo := &webImageAuthAccountRepo{}
		webImages := NewOpenAIWebImagesService(webImgTestCfg("memory"), nil, repo)
		svc := &OpenAIGatewayService{
			webImages:        webImages,
			rateLimitService: &RateLimitService{accountRepo: repo, cfg: &config.Config{}},
		}

		err := svc.openAIWebImageAuthFailover(context.Background(), account, &webdriver.Error{
			Kind: webdriver.ErrorKindAuth, Message: "token invalidated", ResponseBody: body,
		}, "gpt-image-2")
		var failoverErr *UpstreamFailoverError
		require.ErrorAs(t, err, &failoverErr)
		require.Equal(t, http.StatusUnauthorized, failoverErr.StatusCode)
		require.Equal(t, body, failoverErr.ResponseBody)
		require.False(t, failoverErr.RetryableOnSameAccount)
		require.Equal(t, 1, repo.setErrorCalls)
		require.Equal(t, 1, repo.updateExtraCalls)
	})

	t.Run("still fails over when shared policy makes no state change", func(t *testing.T) {
		repo := &webImageAuthAccountRepo{}
		webImages := NewOpenAIWebImagesService(webImgTestCfg("memory"), nil, repo)
		svc := &OpenAIGatewayService{webImages: webImages}

		err := svc.openAIWebImageAuthFailover(context.Background(), account, &webdriver.Error{
			Kind: webdriver.ErrorKindAuth, Message: "token invalidated", ResponseBody: body,
		}, "gpt-image-2")
		var failoverErr *UpstreamFailoverError
		require.ErrorAs(t, err, &failoverErr)
		require.Equal(t, http.StatusUnauthorized, failoverErr.StatusCode)
		require.Equal(t, body, failoverErr.ResponseBody)
		require.Equal(t, 0, repo.setErrorCalls)
		require.Equal(t, 1, repo.updateExtraCalls)
	})
}

func TestAppendOpenAIWebImagesDownloadAttachmentPrompt(t *testing.T) {
	out := appendOpenAIWebImagesDownloadAttachmentPrompt("生成海报", "2160x3840")
	for _, want := range []string{
		"生成海报",
		"请使用图像生成流程创建原图文件。",
		"画布严格为 2160x3840 像素。",
		"最终将 PNG 作为可下载文件/附件保存并提供下载链接。",
		"不要只发送聊天内预览图或压缩图。",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
}

func TestAppendOpenAIWebImagesDownloadAttachmentPromptSkipsInvalidSize(t *testing.T) {
	out := appendOpenAIWebImagesDownloadAttachmentPrompt("生成海报", "4k")
	if out != "生成海报" {
		t.Fatalf("unexpected prompt: %q", out)
	}
}

func TestInferOpenAIWebImageTestSize(t *testing.T) {
	got := inferOpenAIWebImageTestSize("画布严格为 2160×3840 像素，输出图片尺寸为 2160x3840。")
	if got != "2160x3840" {
		t.Fatalf("got %q", got)
	}
}
