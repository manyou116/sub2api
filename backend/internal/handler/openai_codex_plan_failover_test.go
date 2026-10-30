//go:build unit

package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type codexPlanHandlerRepo struct{ grokCredentialHandlerRepo }

func (r *codexPlanHandlerRepo) SetModelRateLimit(_ context.Context, id int64, model string, until time.Time, reason ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.accounts {
		if r.accounts[i].ID != id {
			continue
		}
		extra := cloneCredentialMap(r.accounts[i].Extra)
		limits, _ := extra["model_rate_limits"].(map[string]any)
		limits = cloneCredentialMap(limits)
		state := map[string]any{"rate_limit_reset_at": until.UTC().Format(time.RFC3339)}
		if len(reason) > 0 {
			state["reason"] = reason[0]
		}
		limits[model] = state
		extra["model_rate_limits"] = limits
		r.accounts[i].Extra = extra
	}
	return nil
}

type codexPlanHandlerUpstream struct {
	service.HTTPUpstream
	hits      []int64
	rejectAll bool
}

func (u *codexPlanHandlerUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	u.hits = append(u.hits, accountID)
	model := gjson.GetBytes(body, "model").String()
	if model == "gpt-6.1-sol" && (accountID == 801 || u.rejectAll) {
		return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"detail":"The 'gpt-6.1-sol' model is not supported when using Codex with a ChatGPT account."}`))}, nil
	}
	success := fmt.Sprintf(`{"id":"resp_codex_model_healthy","object":"response","status":"completed","model":%q,"output":[],"usage":{"input_tokens":1,"output_tokens":1}}`, model)
	contentType := "application/json"
	if gjson.GetBytes(body, "stream").Bool() {
		contentType = "text/event-stream"
		success = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + success + "}\n\n"
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(success))}, nil
}

func codexPlanFailoverRouter(t *testing.T, passthrough, rejectAll bool) (*gin.Engine, *codexPlanHandlerRepo, *codexPlanHandlerUpstream) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	accounts := make([]service.Account, 3)
	for i := range accounts {
		accounts[i] = service.Account{ID: int64(801 + i), Name: "model-fixture", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
			Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: i + 1,
			Credentials: map[string]any{"access_token": "fixture-token"}, Extra: map[string]any{"openai_passthrough": passthrough}}
	}
	repo := &codexPlanHandlerRepo{grokCredentialHandlerRepo: grokCredentialHandlerRepo{accounts: accounts}}
	upstream := &codexPlanHandlerUpstream{rejectAll: rejectAll}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Gateway.MaxAccountSwitches = 1
	settings := service.NewSettingService(&capacityHandlerSettings{value: "{}"}, cfg)
	rate := service.NewRateLimitService(repo, nil, cfg, nil, nil)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	gateway := service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, cfg, nil, nil,
		service.NewBillingService(cfg, nil), rate, billing, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, settings, nil)
	cache := &concurrencyCacheMock{acquireUserSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil },
		acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil }}
	h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(cache), billing, &service.APIKeyService{}, nil, nil, nil, nil, cfg)
	groupID := int64(901)
	key := &service.APIKey{ID: 902, GroupID: &groupID, User: &service.User{ID: 903, Status: service.StatusActive},
		Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive}}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), key)
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: key.User.ID, Concurrency: 1})
		c.Next()
	})
	router.POST("/v1/responses", h.Responses)
	return router, repo, upstream
}

func codexPlanHandlerRequest(router *gin.Engine, model string, stream bool) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":%q,"instructions":"fixture instructions","input":"hello","stream":%t}`, model, stream)))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	return rec
}

func TestCodexPlanHandlerSwitchesAndKeepsOtherModels(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("passthrough=%t/stream=%t", passthrough, stream), func(t *testing.T) {
				router, repo, upstream := codexPlanFailoverRouter(t, passthrough, false)
				rec := codexPlanHandlerRequest(router, "gpt-6.1-sol", stream)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				require.Contains(t, rec.Body.String(), "resp_codex_model_healthy")
				require.NotContains(t, rec.Body.String(), "not supported")
				require.Equal(t, []int64{801, 802}, upstream.hits, "the first request must switch without retrying the rejected account")
				account, err := repo.GetByID(context.Background(), 801)
				require.NoError(t, err)
				require.False(t, account.IsSchedulableForModel("gpt-6.1-sol"))
				require.True(t, account.IsSchedulableForModel("gpt-5.4"))
				require.True(t, account.IsSchedulable())
				require.Empty(t, repo.setErrorIDs)
				require.Empty(t, repo.setTempIDs)
				require.Empty(t, repo.rateLimitIDs)
				next := codexPlanHandlerRequest(router, "gpt-6.1-sol", stream)
				require.Equal(t, http.StatusOK, next.Code, next.Body.String())
				require.Equal(t, []int64{801, 802, 802}, upstream.hits, "subsequent requests must skip the rejected account/model pair")
				repo.mu.Lock()
				repo.accounts = repo.accounts[:1]
				repo.mu.Unlock()
				other := codexPlanHandlerRequest(router, "gpt-5.4", stream)
				require.Equal(t, http.StatusOK, other.Code, other.Body.String())
				require.Equal(t, []int64{801, 802, 802, 801}, upstream.hits, "the same account must remain usable for another model")
			})
		}
	}
}

func TestCodexPlanHandlerStopsAtSwitchBudget(t *testing.T) {
	router, _, upstream := codexPlanFailoverRouter(t, true, true)
	rec := codexPlanHandlerRequest(router, "gpt-6.1-sol", false)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "not supported")
	require.Equal(t, []int64{801, 802}, upstream.hits, "a third account must not exceed the configured switch limit")
}
