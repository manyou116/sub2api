package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestKiroModelDiscoveryService_ListAvailableModels_PaginatesAndCaches(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/ListAvailableModels", r.URL.Path)
		require.Equal(t, "AI_EDITOR", r.URL.Query().Get("origin"))
		require.Equal(t, "50", r.URL.Query().Get("maxResults"))
		require.Equal(t, "arn:aws:codewhisperer:us-east-1:123:profile/test", r.URL.Query().Get("profileArn"))
		require.Equal(t, "Bearer token-1", r.Header.Get("Authorization"))
		require.NotEmpty(t, r.Header.Get("User-Agent"))
		require.NotEmpty(t, r.Header.Get("X-Amz-User-Agent"))
		require.NotEmpty(t, r.Header.Get("amz-sdk-invocation-id"))
		require.Equal(t, "attempt=1; max=1", r.Header.Get("amz-sdk-request"))
		require.Empty(t, r.Header.Get("TokenType"))
		require.Empty(t, r.URL.Query().Get("modelProvider"))

		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("nextToken") == "" {
			_, err := w.Write([]byte(`{
				"models": [
					{"modelId": "claude-sonnet-4.6", "modelName": "Claude Sonnet 4.6"},
					{"modelId": "claude-sonnet-4.6", "modelName": "Duplicate"}
				],
				"nextToken": "page-2"
			}`))
			require.NoError(t, err)
			return
		}
		require.Equal(t, "page-2", r.URL.Query().Get("nextToken"))
		_, err := w.Write([]byte(`{
			"availableModels": [
				{"modelId": "claude-opus-4.7", "modelName": "Claude Opus 4.7"}
			]
		}`))
		require.NoError(t, err)
	}))
	defer server.Close()

	svc := NewKiroModelDiscoveryService(nil)
	svc.endpointBase = server.URL + "/ListAvailableModels"
	svc.validateIP = false
	now := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }

	account := &Account{
		ID:       9,
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "token-1",
			"machine_id":   "machine-1",
			"profile_arn":  "arn:aws:codewhisperer:us-east-1:123:profile/test",
			"provider":     "BuilderId",
			"region":       "us-east-1",
		},
	}

	models, err := svc.ListAvailableModels(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, []KiroAvailableModel{
		{ID: "claude-opus-4.7", Type: "model", DisplayName: "Claude Opus 4.7"},
		{ID: "claude-sonnet-4.6", Type: "model", DisplayName: "Claude Sonnet 4.6"},
	}, models)
	require.Equal(t, int64(2), calls.Load())

	models[0].DisplayName = "mutated"
	cached, err := svc.ListAvailableModels(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, "Claude Opus 4.7", cached[0].DisplayName)
	require.Equal(t, int64(2), calls.Load())
}

func TestKiroModelDiscoveryService_ListAvailableModels_InternalProviderHeaderAndModelProviderFilter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "InternalModels", r.URL.Query().Get("modelProvider"))
		require.Equal(t, "true", r.Header.Get("redirect-for-internal"))
		_, err := w.Write([]byte(`{"models":[{"modelId":"internal-model"}]}`))
		require.NoError(t, err)
	}))
	defer server.Close()

	svc := NewKiroModelDiscoveryService(nil)
	svc.endpointBase = server.URL + "/ListAvailableModels"
	svc.validateIP = false
	account := &Account{
		ID:       10,
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":   "token-1",
			"machine_id":     "machine-1",
			"provider":       "Internal",
			"model_provider": "InternalModels",
		},
	}

	models, err := svc.ListAvailableModels(context.Background(), account)
	require.NoError(t, err)
	require.Len(t, models, 1)
	require.Equal(t, "internal-model", models[0].ID)
}

func TestKiroAvailableModelsCacheKeyIncludesModelProvider(t *testing.T) {
	account := &Account{
		ID:       10,
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"provider":       "Internal",
			"profile_arn":    "arn:aws:codewhisperer:us-east-1:123:profile/test",
			"region":         "us-east-1",
			"model_provider": "InternalModels",
			"expires_at":     int64(123),
		},
	}
	withProvider := kiroAvailableModelsCacheKey(account)
	account.Credentials["model_provider"] = "ExternalModels"
	withoutProvider := kiroAvailableModelsCacheKey(account)

	require.NotEqual(t, withProvider, withoutProvider)
	require.Contains(t, withProvider, "InternalModels")
	require.Contains(t, withoutProvider, "ExternalModels")
}

func TestKiroModelDiscoveryService_ListAvailableModels_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad token", http.StatusUnauthorized)
	}))
	defer server.Close()

	svc := NewKiroModelDiscoveryService(nil)
	svc.endpointBase = server.URL + "/ListAvailableModels"
	svc.validateIP = false
	account := &Account{
		ID:       11,
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "token-1",
			"machine_id":   "machine-1",
		},
	}

	_, err := svc.ListAvailableModels(context.Background(), account)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "HTTP 401"), err.Error())
}

func TestKiroListAvailableModelsResponseModelsPrefersModelsField(t *testing.T) {
	raw := []byte(`{
		"defaultModel": {"modelId": "claude-sonnet-4.6"},
		"models": [{"modelId": "from-models"}],
		"availableModels": [{"modelId": "from-available-models"}]
	}`)
	var resp kiroListAvailableModelsResponse
	require.NoError(t, json.Unmarshal(raw, &resp))
	require.Len(t, resp.models(), 1)
	require.Equal(t, "from-models", resp.models()[0].ModelID)
}

func TestKiroDefaultModelsIncludeLiveDiscoveredModels(t *testing.T) {
	ids := make(map[string]struct{}, len(KiroDefaultModels))
	for _, model := range KiroDefaultModels {
		ids[model.ID] = struct{}{}
	}

	for _, id := range []string{
		"auto",
		"claude-haiku-4.5",
		"claude-opus-4.5",
		"claude-opus-4.6",
		"claude-opus-4.7",
		"claude-sonnet-4",
		"claude-sonnet-4.5",
		"claude-sonnet-4.6",
		"deepseek-3.2",
		"glm-5",
		"minimax-m2.1",
		"minimax-m2.5",
		"qwen3-coder-next",
	} {
		_, ok := ids[id]
		require.True(t, ok, "missing Kiro default model %s", id)
	}
}
