package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type resetCapacityAccountRepo struct {
	service.AdminAccountRepository
	account *service.Account
}

func (r *resetCapacityAccountRepo) GetByID(context.Context, int64) (*service.Account, error) {
	return r.account, nil
}
func (r *resetCapacityAccountRepo) ClearError(context.Context, int64) error {
	r.account.Status, r.account.ErrorMessage = service.StatusActive, ""
	return nil
}
func (r *resetCapacityAccountRepo) ClearRateLimit(context.Context, int64) error { return nil }
func (r *resetCapacityAccountRepo) ClearAntigravityQuotaScopes(context.Context, int64) error {
	return nil
}
func (r *resetCapacityAccountRepo) ClearModelRateLimits(context.Context, int64) error   { return nil }
func (r *resetCapacityAccountRepo) ClearWebImageRateLimit(context.Context, int64) error { return nil }
func (r *resetCapacityAccountRepo) ClearTempUnschedulable(context.Context, int64) error {
	r.account.TempUnschedulableUntil, r.account.TempUnschedulableReason = nil, ""
	return nil
}

// Exercise the real HTTP -> admin service -> gateway adapter -> Redis Lua path.
// DB fields are already empty, matching the production batch-reset remnant.
func TestAccountHandlerResetStatusClearsHiddenCapacityCircuit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, batch := range []bool{false, true} {
		name := "single"
		if batch {
			name = "batch"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			server := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: server.Addr()})
			t.Cleanup(func() { _ = client.Close() })
			cache := repository.NewTempUnschedCache(client)
			store, ok := cache.(service.OpenAICapacityCooldownStore)
			require.True(t, ok)
			settings := service.DefaultOpenAICapacityCooldownSettings()
			old, err := store.ObserveOpenAICapacity(ctx, 42, "gpt-6.1-sol", "old-request", 0, 1, settings)
			require.NoError(t, err)
			require.True(t, old.Tripped)
			blocked, err := store.AdmitOpenAICapacity(ctx, 42, "gpt-6.1-sol", "before-reset", 60)
			require.NoError(t, err)
			require.False(t, blocked.Allowed)
			require.NoError(t, cache.SetTempUnsched(ctx, 42, &service.TempUnschedState{
				UntilUnix: time.Now().Add(time.Hour).Unix(), MatchedKeyword: "openai_slow_first_output",
			}))
			repo := &resetCapacityAccountRepo{account: &service.Account{
				ID: 42, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
				Status: service.StatusError, ErrorMessage: "old error", Schedulable: false,
			}}
			rate := service.NewRateLimitService(repo, nil, nil, nil, cache)
			gateway := service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, nil,
				nil, nil, nil, rate, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			adminService := service.NewAdminService(nil, nil, nil, repo, nil, nil, nil, nil, nil, nil,
				nil, nil, nil, nil, nil, nil, nil, nil, gateway, nil, nil, nil, nil)
			handler := admin.NewAccountHandler(adminService, nil, nil, nil, nil, nil, rate, nil, nil, nil, nil, nil, nil, nil)
			router := gin.New()
			router.POST("/accounts/:id/clear-error", handler.ClearError)
			router.POST("/accounts/batch-clear-error", handler.BatchClearError)
			path, body := "/accounts/42/clear-error", ""
			if batch {
				path, body = "/accounts/batch-clear-error", `{"account_ids":[42]}`
			}
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			if batch {
				require.Contains(t, response.Body.String(), `"success":1`)
				require.Contains(t, response.Body.String(), `"failed":0`)
			}
			require.Equal(t, service.StatusActive, repo.account.Status)
			require.Empty(t, repo.account.ErrorMessage)
			require.False(t, repo.account.Schedulable, "manual scheduling stays unchanged")
			cached, err := cache.GetTempUnsched(ctx, 42)
			require.NoError(t, err)
			require.Nil(t, cached)
			admission, err := store.AdmitOpenAICapacity(ctx, 42, "gpt-6.1-sol", "after-reset", 60)
			require.NoError(t, err)
			require.True(t, admission.Allowed)
			require.False(t, admission.Probe)
			require.Greater(t, admission.Generation, old.Generation)
			late, err := store.ObserveOpenAICapacity(ctx, 42, "gpt-6.1-sol", "old-request", old.Generation, 1, settings)
			require.NoError(t, err)
			require.False(t, late.Tripped, "old requests cannot reinstate the cleared pause")
		})
	}
}
