package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type capacitySettingsRepo struct {
	service.SettingRepository
	raw string
	err error
}

func (r *capacitySettingsRepo) GetValue(context.Context, string) (string, error) { return r.raw, r.err }
func (r *capacitySettingsRepo) Set(_ context.Context, _, raw string) error {
	if r.err != nil {
		return r.err
	}
	r.raw = raw
	return nil
}

func TestCapacityCooldownAdminSettingsContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &capacitySettingsRepo{}
	h := NewSettingHandler(service.NewSettingService(repo, &config.Config{}), nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.GET("/settings", h.GetOpenAICapacityCooldownSettings)
	router.PUT("/settings", h.UpdateOpenAICapacityCooldownSettings)
	call := func(method, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/settings", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		return rec
	}
	decode := func(rec *httptest.ResponseRecorder) service.OpenAICapacityCooldownSettings {
		t.Helper()
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var response struct {
			Data service.OpenAICapacityCooldownSettings `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
		return response.Data
	}
	require.Equal(t, service.DefaultOpenAICapacityCooldownSettings(), decode(call(http.MethodGet, "")))
	want := service.DefaultOpenAICapacityCooldownSettings()
	want.Enabled, want.SwitchAccountOnOverload, want.CooldownSeconds = true, false, 120
	body, err := json.Marshal(want)
	require.NoError(t, err)
	require.Equal(t, want, decode(call(http.MethodPut, string(body))))
	require.JSONEq(t, string(body), repo.raw)
	require.Equal(t, want, decode(call(http.MethodGet, "")))
	for _, bad := range []string{`{`, `{"window_seconds":0}`, `{"failure_threshold":1}`, `{"failure_rate_percent":0}`, `{"cooldown_seconds":601,"max_cooldown_seconds":600}`, `{"max_cooldown_seconds":7201}`} {
		rec := call(http.MethodPut, bad)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		require.JSONEq(t, string(body), repo.raw, "invalid input must not overwrite persisted settings")
	}
	repo.err = errors.New("storage unavailable")
	require.Equal(t, http.StatusInternalServerError, call(http.MethodPut, string(body)).Code)
}
