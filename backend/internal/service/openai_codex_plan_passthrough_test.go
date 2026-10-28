//go:build unit

package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const codexPlanPassthroughMessage = "The 'gpt-6.1-sol' model is not supported when using Codex with a ChatGPT account."

func TestCodexPlanPassthroughFailsOverBeforeResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	bodies := []string{
		fmt.Sprintf(`{"detail":%q}`, codexPlanPassthroughMessage),
		fmt.Sprintf(`{"error":{"type":"invalid_request_error","message":%q}}`, codexPlanPassthroughMessage),
		fmt.Sprintf(`{"error":{"code":"unsupported_model","message":%q}}`, codexPlanPassthroughMessage),
	}
	for index, body := range bodies {
		for _, path := range []string{"/v1/responses", "/v1/responses/compact"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("body=%d/path=%s/stream=%t", index, path, stream), func(t *testing.T) {
					repo := &modelNotFoundAccountRepoStub{}
					upstream := &httpUpstreamRecorder{resp: &http.Response{
						StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": {"application/json"}},
						Body: io.NopCloser(strings.NewReader(body)),
					}}
					svc := &OpenAIGatewayService{
						cfg: &config.Config{}, accountRepo: repo, httpUpstream: upstream,
						rateLimitService: &RateLimitService{accountRepo: repo},
					}
					account := openAICodexPlanGatedOAuthAccount()
					account.Credentials["access_token"] = "fixture-token"
					account.Extra = map[string]any{"openai_passthrough": true}
					requestBody := []byte(fmt.Sprintf(`{"model":"gpt-6.1-sol","instructions":"fixture instructions","input":"hello","stream":%t}`, stream))
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(requestBody))
					_, err := svc.Forward(context.Background(), c, account, requestBody)
					var failover *UpstreamFailoverError
					require.ErrorAs(t, err, &failover)
					require.True(t, failover.ShouldRetryNextAccount())
					require.False(t, failover.RetryableOnSameAccount)
					require.Equal(t, http.StatusBadRequest, failover.StatusCode)
					require.Equal(t, []byte(body), failover.ResponseBody)
					require.False(t, IsResponseCommitted(c))
					require.False(t, c.Writer.Written())
					require.Empty(t, rec.Body.String(), "the rejected attempt must not reach the client")
					require.Len(t, upstream.requests, 1, "do not retry the rejected account or change its model")
					require.Equal(t, "gpt-6.1-sol", gjson.GetBytes(upstream.lastBody, "model").String())
					require.Len(t, repo.modelRateLimitCalls, 1)
					call := repo.modelRateLimitCalls[0]
					require.Equal(t, "gpt-6.1-sol", call.scope)
					require.Equal(t, upstreamCodexPlanGatedModelReason, call.reason)
					require.WithinDuration(t, time.Now().Add(30*time.Minute), call.resetAt, 5*time.Second)
					require.Zero(t, repo.tempCalls, "model availability must not pause the whole account")
				})
			}
		}
	}
}

func TestCodexPlanPassthroughFailoverScope(t *testing.T) {
	body := []byte(fmt.Sprintf(`{"detail":%q}`, codexPlanPassthroughMessage))
	for _, tc := range []struct {
		name    string
		account *Account
		status  int
		body    []byte
		want    bool
	}{
		{"oauth", &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, 400, body, true},
		{"setup token", &Account{Platform: PlatformOpenAI, Type: AccountTypeSetupToken}, 400, body, true},
		{"api key", &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, 400, body, false},
		{"other platform", &Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth}, 400, body, false},
		{"missing account", nil, 400, body, false},
		{"different status", &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, 422, body, false},
		{"invalid parameter", &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, 400, []byte(`{"error":{"message":"Parameter tools is not supported for this model"}}`), false},
		{"invalid schema", &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, 400, []byte(openAIInvalidFunctionParametersBody), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, shouldFailoverOpenAIPassthroughResponse(tc.account, tc.status, tc.body))
		})
	}
}
