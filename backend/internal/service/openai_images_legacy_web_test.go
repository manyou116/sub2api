package service

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service/openaiimages/webdriver"
	"github.com/stretchr/testify/require"
)

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
