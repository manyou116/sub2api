//go:build unit

package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type deliveryHandlerRepo struct {
	service.AccountRepository
	account *service.Account
	writes  int
}

func (r *deliveryHandlerRepo) GetByID(context.Context, int64) (*service.Account, error) {
	copy := *r.account
	return &copy, nil
}
func (r *deliveryHandlerRepo) UpdateOpenAIOAuthCredentialsIfUnchanged(_ context.Context, _ int64, _ map[string]any, _ *int64, next map[string]any) (bool, error) {
	r.writes++
	r.account.Credentials = next
	return true, nil
}

type deliveryHandlerCache struct{ service.GeminiTokenCache }

func (*deliveryHandlerCache) AcquireRefreshLease(context.Context, string, time.Duration) (string, error) {
	return "owner", nil
}
func (*deliveryHandlerCache) OwnsRefreshLease(context.Context, string, string) (bool, error) {
	return true, nil
}
func (*deliveryHandlerCache) ReleaseRefreshLease(context.Context, string, string) error { return nil }
func (*deliveryHandlerCache) DeleteAccessToken(context.Context, string) error           { return nil }

func TestOpenAIOAuthDeliveryHTTPContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	payload, _ := json.Marshal(map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "client_id": "client", "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "workspace"}})
	creds := map[string]any{"access_token": "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig", "refresh_token": "rt", "client_id": "client", "chatgpt_account_id": "workspace"}
	repo := &deliveryHandlerRepo{account: &service.Account{ID: 7, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Credentials: creds}}
	handler := &AccountHandler{oauthRefreshAPI: service.NewOAuthRefreshAPI(repo, &deliveryHandlerCache{})}
	router := gin.New()
	router.GET("/accounts/oauth-delivery-capabilities", handler.OpenAIOAuthDeliveryCapabilities)
	router.GET("/accounts/:id/oauth-credentials", handler.OpenAIOAuthCredentials)
	router.PUT("/accounts/:id/oauth-credentials", handler.OpenAIOAuthCredentials)
	request := func(method, path, body string) (int, map[string]any) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
		var result map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
		return w.Code, result
	}
	status, capability := request("GET", "/accounts/oauth-delivery-capabilities", "")
	require.Equal(t, 200, status)
	require.Equal(t, service.OpenAIOAuthDeliveryCapability, capability["data"].(map[string]any)["capability"])
	status, first := request("GET", "/accounts/7/oauth-credentials", "")
	require.Equal(t, 200, status)
	data := first["data"].(map[string]any)
	revision := data["credential_revision"].(string)
	require.Len(t, revision, 64)
	require.Equal(t, float64(7), data["account_id"])
	status, _ = request("PUT", "/accounts/7/oauth-credentials", `{"credentials":{}}`)
	require.Equal(t, http.StatusBadRequest, status)
	require.Zero(t, repo.writes)
	body, _ := json.Marshal(map[string]any{"expected_credential_revision": strings.Repeat("0", 64), "credentials": creds})
	status, conflict := request("PUT", "/accounts/7/oauth-credentials", string(body))
	require.Equal(t, 409, status)
	require.Equal(t, "OAUTH_CREDENTIAL_CONFLICT", conflict["reason"])
	require.Zero(t, repo.writes)
	body, _ = json.Marshal(map[string]any{"expected_credential_revision": revision, "credentials": creds})
	status, result := request("PUT", "/accounts/7/oauth-credentials", string(body))
	require.Equal(t, 200, status)
	require.Equal(t, 1, repo.writes)
	require.NotEqual(t, revision, result["data"].(map[string]any)["credential_revision"])
}

func TestOpenAIOAuthDeliveryMissingCoordinationRejectsCapability(t *testing.T) {
	handler := &AccountHandler{}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)
	handler.OpenAIOAuthDeliveryCapabilities(c)
	require.Equal(t, 503, w.Code)
	require.Contains(t, w.Body.String(), "OAUTH_COORDINATION_UNAVAILABLE")
}

func TestOpenAIOAuthDeliveryCreateSchedulingRequest(t *testing.T) {
	var omitted, paused CreateAccountRequest
	require.NoError(t, json.Unmarshal([]byte(`{"name":"legacy"}`), &omitted))
	require.Nil(t, omitted.Schedulable)
	require.NoError(t, json.Unmarshal([]byte(`{"name":"delivery","schedulable":false}`), &paused))
	require.NotNil(t, paused.Schedulable)
	require.False(t, *paused.Schedulable)
}
