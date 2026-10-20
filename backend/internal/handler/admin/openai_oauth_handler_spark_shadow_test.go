//go:build unit

package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestCreateShadow_ReturnsCreatedShadow(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stub := &stubAdminService{}
	h := NewOpenAIOAuthHandler(nil, stub, nil, nil)

	router := gin.New()
	router.POST("/api/v1/admin/accounts/:id/shadow", h.CreateShadow)

	body := `{"name":"p-spark","priority":50,"concurrency":2,"group_ids":[10,20]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/42/shadow", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	data, ok := resp["data"].(map[string]any)
	require.True(t, ok, "response should have data field")

	// parent_account_id must be present and equal to the path param
	pid, ok := data["parent_account_id"].(float64)
	require.True(t, ok, "parent_account_id should be present")
	require.Equal(t, float64(42), pid)

	// quota_dimension must be "spark"
	require.Equal(t, service.QuotaDimensionSpark, data["quota_dimension"])

	// name round-trips
	require.Equal(t, "p-spark", data["name"])
}

func TestCreateShadow_InvalidID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := NewOpenAIOAuthHandler(nil, &stubAdminService{}, nil, nil)

	router := gin.New()
	router.POST("/api/v1/admin/accounts/:id/shadow", h.CreateShadow)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/not-a-number/shadow",
		strings.NewReader(`{"name":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreateShadow_ServiceError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stub := &stubAdminService{createSparkShadowErr: errors.New("database unavailable")}
	h := NewOpenAIOAuthHandler(nil, stub, nil, nil)

	router := gin.New()
	router.POST("/api/v1/admin/accounts/:id/shadow", h.CreateShadow)

	body := `{"name":"p-spark","priority":50}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/42/shadow", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	// A generic (non-ApplicationError) service error maps to 500 via response.ErrorFrom.
	require.GreaterOrEqual(t, rec.Code, http.StatusBadRequest)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestCreateShadow_BadBody(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := NewOpenAIOAuthHandler(nil, &stubAdminService{}, nil, nil)

	router := gin.New()
	router.POST("/api/v1/admin/accounts/:id/shadow", h.CreateShadow)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/42/shadow",
		strings.NewReader(`{not valid json`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestWebImageShadowCreationPreservesExplicitValues(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewOpenAIOAuthHandler(nil, &stubAdminService{}, nil, nil)
	router := gin.New()
	router.POST("/accounts/:id/web-image-shadow", h.CreateWebImageShadow)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/accounts/42/web-image-shadow", strings.NewReader(`{"priority":0,"concurrency":3,"group_ids":[]}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var response struct {
		Data struct {
			ParentAccountID int64  `json:"parent_account_id"`
			QuotaDimension  string `json:"quota_dimension"`
			Concurrency     int    `json:"concurrency"`
			Priority        int    `json:"priority"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	require.Equal(t, int64(42), response.Data.ParentAccountID)
	require.Equal(t, service.QuotaDimensionWebImages, response.Data.QuotaDimension)
	require.Equal(t, 3, response.Data.Concurrency)
	require.Zero(t, response.Data.Priority)
}
