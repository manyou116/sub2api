//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/repository" //nolint:depguard // Test-only adapter exercises the real shared Redis circuit through the handler.
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9" //nolint:depguard // Connects only to the isolated miniredis fixture, never a live datastore.
	"github.com/stretchr/testify/require"
)

type capacityHandlerSettings struct {
	service.SettingRepository
	value string
}

func (r *capacityHandlerSettings) GetValue(_ context.Context, key string) (string, error) {
	if key == "openai_capacity_cooldown_settings" {
		return r.value, nil
	}
	return "", nil
}

func (r *capacityHandlerSettings) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{}, nil
}

type capacityHandlerUpstream struct {
	service.HTTPUpstream
	hits []int64
	mode string
}

func (u *capacityHandlerUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.hits = append(u.hits, accountID)
	errorBody := `{"error":{"code":"server_is_overloaded","type":"server_error","message":"Our servers are currently overloaded. Please try again later."}}`
	failed := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_failed\",\"status\":\"failed\",\"error\":{\"code\":\"server_is_overloaded\",\"message\":\"Our servers are currently overloaded. Please try again later.\"}}}\n\n"
	status, contentType, body := http.StatusOK, "text/event-stream", "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_healthy\",\"status\":\"completed\",\"model\":\"gpt-5.4\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"healthy answer\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
	if accountID == 801 || u.mode == "exhausted" {
		switch u.mode {
		case "failed":
			body = failed
		case "error":
			body = "event: error\ndata: {\"type\":\"error\",\"code\":\"server_is_overloaded\",\"message\":\"Our servers are currently overloaded. Please try again later.\"}\n\n"
		case "after_output":
			body = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"already sent\",\"output_index\":0,\"content_index\":0}\n\n" + failed
		default:
			status, contentType, body = http.StatusServiceUnavailable, "application/json", errorBody
		}
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func capacityFailoverRouter(t *testing.T, mode string, passthrough bool) (*gin.Engine, *grokCredentialHandlerRepo, *capacityHandlerUpstream, service.OpenAICapacityCooldownStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	accounts := make([]service.Account, 3)
	for i := range accounts {
		accounts[i] = service.Account{ID: int64(801 + i), Name: "capacity-fixture", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
			Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: i + 1,
			Credentials: map[string]any{"access_token": "fixture-token"}, Extra: map[string]any{"openai_passthrough": passthrough}}
	}
	repo := &grokCredentialHandlerRepo{accounts: accounts}
	upstream := &capacityHandlerUpstream{mode: mode}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Gateway.MaxAccountSwitches = 1
	v := service.DefaultOpenAICapacityCooldownSettings()
	v.Enabled = true
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	settings := service.NewSettingService(&capacityHandlerSettings{value: string(raw)}, cfg)
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	store := repository.NewTempUnschedCache(rdb)
	rate := service.NewRateLimitService(repo, nil, cfg, nil, store)
	rate.SetSettingService(settings)
	billingCache := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCache.Stop)
	gateway := service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, cfg, nil, nil,
		service.NewBillingService(cfg, nil), rate, billingCache, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, settings, nil)
	cache := &concurrencyCacheMock{acquireUserSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil },
		acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil }}
	h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(cache), billingCache, &service.APIKeyService{}, nil, nil, nil, nil, cfg)
	groupID := int64(901)
	apiKey := &service.APIKey{ID: 902, GroupID: &groupID, User: &service.User{ID: 903, Status: service.StatusActive},
		Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive}}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), apiKey)
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.User.ID, Concurrency: 1})
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.ClientRequestID, "capacity-http-request"))
		c.Next()
	})
	router.POST("/v1/responses", h.Responses)
	return router, repo, upstream, store.(service.OpenAICapacityCooldownStore)
}

func TestCapacityCooldownHandlerSwitchesBeforeOutput(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, mode := range []string{"http503", "failed", "error"} {
				t.Run(fmt.Sprintf("%s/stream=%v/passthrough=%v", mode, stream, passthrough), func(t *testing.T) {
					router, repo, upstream, store := capacityFailoverRouter(t, mode, passthrough)
					rec := httptest.NewRecorder()
					req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":"gpt-5.4","input":"hello","stream":%v}`, stream)))
					req.Header.Set("Content-Type", "application/json")
					router.ServeHTTP(rec, req)
					require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
					require.Contains(t, rec.Body.String(), "resp_healthy")
					require.NotContains(t, rec.Body.String(), "server_is_overloaded")
					require.NotContains(t, rec.Body.String(), "resp_failed")
					require.Equal(t, []int64{801, 802}, upstream.hits, "switch directly instead of retrying the overloaded account")
					require.Empty(t, repo.setErrorIDs)
					require.Equal(t, []int64{801}, repo.setTempIDs, "the existing temporary-unschedulable state must expose the text pause")
					require.Empty(t, repo.rateLimitIDs, "overload is independent of quota/429")
					blocked, err := store.BlockedOpenAICapacityAccounts(context.Background(), "gpt-5.4")
					require.NoError(t, err)
					require.Equal(t, []int64{801}, blocked, "one overload must immediately cool the account at the default threshold")
					d, err := store.AdmitOpenAICapacity(context.Background(), 801, "gpt-5.4", "next-admission", 60)
					require.NoError(t, err)
					require.False(t, d.Allowed)
					require.EqualValues(t, 1, d.Generation, "duplicate observations must not open additional cooldowns")
					require.EqualValues(t, 1, d.Level)
					next := httptest.NewRecorder()
					nextReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.4-mini","input":"next","stream":false}`))
					nextReq.Header.Set("Content-Type", "application/json")
					router.ServeHTTP(next, nextReq)
					require.Equal(t, http.StatusOK, next.Code, next.Body.String())
					require.Equal(t, []int64{801, 802, 802}, upstream.hits, "another text model must also skip the cooled account")
				})
			}
		}
	}
}

func TestCapacityCooldownHandlerStopsAtSwitchBudget(t *testing.T) {
	router, _, upstream, _ := capacityFailoverRouter(t, "exhausted", false)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.4","input":"hello","stream":false}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	require.GreaterOrEqual(t, rec.Code, 500, rec.Body.String())
	require.Equal(t, []int64{801, 802}, upstream.hits, "the third account must not exceed the configured switch budget")
}

func TestCapacityCooldownHandlerDoesNotReplayAfterOutput(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprint(passthrough), func(t *testing.T) {
			router, _, upstream, store := capacityFailoverRouter(t, "after_output", passthrough)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.4","input":"hello","stream":true}`))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)
			require.Contains(t, rec.Body.String(), "already sent")
			require.NotContains(t, rec.Body.String(), "resp_healthy")
			require.Equal(t, []int64{801}, upstream.hits)
			blocked, err := store.BlockedOpenAICapacityAccounts(context.Background(), "gpt-5.4")
			require.NoError(t, err)
			require.Equal(t, []int64{801}, blocked, "post-output overload must trigger cooldown even though replay is forbidden")
		})
	}
}
