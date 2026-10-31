//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpstreamRequestBodyStatsFallbackAndSnapshot(t *testing.T) {
	ctx := WithUpstreamRequestBodyStats(context.Background())
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	upstream := &httpUpstreamRecorder{resp: oauthCompressionResponse(200, "ok"), responses: []*http.Response{
		oauthCompressionResponse(415, "unsupported"), oauthCompressionResponse(200, "ok"),
	}}
	s := &OpenAIGatewayService{httpUpstream: upstream}
	req := oauthCompressionRequest(t, oauthCompressionBody()).WithContext(ctx)
	resp, err := s.doOpenAIUpstream(req, "", account)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	stats := upstreamRequestBodyStats(ctx)
	require.Equal(t, 2, stats.Attempts)
	require.Equal(t, 1, stats.CompressedAttempts)
	require.Equal(t, 1, stats.Fallbacks)
	require.Equal(t, req.ContentLength*2, stats.RawBytes)
	require.Equal(t, upstream.requests[0].ContentLength+req.ContentLength, stats.EncodedBytes)
	require.Equal(t, "encoding_rejected", stats.Reason)
	require.Equal(t, req.ContentLength, stats.FinalEncodedBytes)
	frozen := CopyUpstreamRequestBodyStats(ctx, context.Background())
	// A subsequent account retry adds a new attempt without changing the snapshot.
	account.Extra = map[string]any{"openai_request_compression": false}
	resp, err = s.doOpenAIUpstream(req, "", account)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, 3, upstreamRequestBodyStats(ctx).Attempts)
	require.Equal(t, "account_disabled", upstreamRequestBodyStats(ctx).Reason)
	require.Equal(t, stats, upstreamRequestBodyStats(frozen))
	require.Nil(t, upstreamRequestBodyStats(context.Background()))
	require.Nil(t, upstreamRequestBodyStats(WithUpstreamRequestBodyStats(context.Background())))
}

func TestUpstreamRequestBodyStatsTinyAndUncovered(t *testing.T) {
	ctx := WithUpstreamRequestBodyStats(context.Background())
	upstream := &httpUpstreamRecorder{resp: oauthCompressionResponse(200, "ok")}
	s := &OpenAIGatewayService{httpUpstream: upstream}
	req := oauthCompressionRequest(t, []byte(`{"stream":true}`)).WithContext(ctx)
	resp, err := s.doOpenAIUpstream(req, "", &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	stats := upstreamRequestBodyStats(ctx)
	require.Equal(t, "not_smaller", stats.Reason)
	require.Equal(t, stats.RawBytes, stats.EncodedBytes)
	ctx = WithUpstreamRequestBodyStats(context.Background())
	req = req.WithContext(ctx)
	req.URL.Host = "custom.example.org"
	resp, err = s.doOpenAIUpstream(req, "", &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Nil(t, upstreamRequestBodyStats(ctx))
}

func TestUpstreamRequestBodyStatsRecordUsage(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{}
	svc := newOpenAIRecordUsageServiceWithBillingRepoForTest(usageRepo,
		&openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}},
		&openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	ctx := WithUpstreamRequestBodyStats(context.Background())
	recordUpstreamRequestBody(ctx, 1024, 256, "zstd", "compressed", false)
	err := svc.RecordUsage(CopyUpstreamRequestBodyStats(ctx, context.Background()), &OpenAIRecordUsageInput{
		Result: &OpenAIForwardResult{RequestID: "body-stats-usage", Model: "gpt-5.1", Usage: OpenAIUsage{InputTokens: 8, OutputTokens: 4}},
		APIKey: &APIKey{ID: 10049}, User: &User{ID: 20049}, Account: &Account{ID: 30049},
	})
	require.NoError(t, err)
	require.NotNil(t, usageRepo.lastLog)
	require.Equal(t, upstreamRequestBodyStats(ctx), usageRepo.lastLog.UpstreamRequestBody)
}
