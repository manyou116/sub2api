package admin

import (
	"context"
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

type compressionSettingsRepo struct {
	service.SettingRepository
	values map[string]string
	err    error
}

func (r *compressionSettingsRepo) GetValue(_ context.Context, key string) (string, error) {
	return r.values[key], r.err
}
func (r *compressionSettingsRepo) Set(_ context.Context, key, value string) error {
	if r.err != nil {
		return r.err
	}
	r.values[key] = value
	return nil
}

func (r *compressionSettingsRepo) SetMultiple(ctx context.Context, updates map[string]string) error {
	if r.err != nil {
		return r.err
	}
	for key, value := range updates {
		r.values[key] = value
	}
	return nil
}

func TestOpenAIRequestCompressionAdminSettings(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &compressionSettingsRepo{values: map[string]string{"unrelated": "keep"}}
	newRouter := func() *gin.Engine {
		h := NewSettingHandler(service.NewSettingService(repo, &config.Config{}), nil, nil, nil, nil, nil, nil)
		r := gin.New()
		r.GET("/settings", h.GetOpenAIRequestCompression)
		r.PUT("/settings", h.UpdateOpenAIRequestCompression)
		return r
	}
	router := newRouter()
	call := func(method, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/settings", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		return rec
	}
	require.JSONEq(t, `{"code":0,"message":"success","data":{"enabled":true,"image_guard_enabled":true,"image_guard_max_bytes":8388608,"bare_400_fallback_enabled":true}}`, call(http.MethodGet, "").Body.String())
	for _, value := range []string{"false", "true", "false"} {
		put := call(http.MethodPut, `{"enabled":`+value+`}`)
		require.Equal(t, 200, put.Code, put.Body.String())
		require.JSONEq(t, `{"code":0,"message":"success","data":{"enabled":`+value+`,"image_guard_enabled":true,"image_guard_max_bytes":8388608,"bare_400_fallback_enabled":true}}`, put.Body.String())
		require.Equal(t, value, repo.values["openai_request_compression_enabled"])
		router = newRouter()
		require.JSONEq(t, put.Body.String(), call(http.MethodGet, "").Body.String(), "reload from persisted data")
	}
	for _, invalid := range []string{`{}`, `null`, `{"enabled":null}`, `{"enabled":"true"}`, `{"enabled":0}`, `{`} {
		resp := call(http.MethodPut, invalid)
		require.Equal(t, 400, resp.Code, resp.Body.String())
		require.Equal(t, "false", repo.values["openai_request_compression_enabled"])
	}
	require.Equal(t, "keep", repo.values["unrelated"])
	put := call(http.MethodPut, `{"image_guard_enabled":false,"image_guard_max_bytes":16777216}`)
	require.Equal(t, 200, put.Code, put.Body.String())
	require.JSONEq(t, `{"code":0,"message":"success","data":{"enabled":false,"image_guard_enabled":false,"image_guard_max_bytes":16777216,"bare_400_fallback_enabled":true}}`, put.Body.String())
	require.JSONEq(t, `{"image_guard_enabled":false,"image_guard_max_bytes":16777216,"bare_400_fallback_enabled":true}`, repo.values["openai_request_compression_options"])
	for _, invalid := range []string{`{"enabled":true,"image_guard_max_bytes":0}`, `{"image_guard_max_bytes":68157440}`} {
		require.Equal(t, 400, call(http.MethodPut, invalid).Code)
		require.Equal(t, "false", repo.values["openai_request_compression_enabled"], "invalid payload must not partially enable compression")
	}
	fallback := call(http.MethodPut, `{"bare_400_fallback_enabled":false}`)
	require.Equal(t, 200, fallback.Code)
	router = newRouter()
	require.JSONEq(t, fallback.Body.String(), call(http.MethodGet, "").Body.String())
	repo.err = errors.New("storage unavailable")
	require.Equal(t, 500, call(http.MethodPut, `{"enabled":true}`).Code)
	router = newRouter()
	require.Equal(t, 500, call(http.MethodGet, "").Code)
}
