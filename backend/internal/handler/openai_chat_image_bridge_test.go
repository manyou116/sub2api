package handler

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

func newChatImageBridgeTestHandler(enabled bool) *OpenAIGatewayHandler {
	return &OpenAIGatewayHandler{
		gatewayService:      &service.OpenAIGatewayService{},
		billingCacheService: &service.BillingCacheService{},
		apiKeyService:       &service.APIKeyService{},
		concurrencyHelper:   &ConcurrencyHelper{concurrencyService: &service.ConcurrencyService{}},
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				OpenAIChatImageBridge: config.OpenAIChatImageBridgeConfig{
					Enabled:       enabled,
					ResponseStyle: service.ChatImageBridgeStyleMarkdownDataURL,
				},
			},
		},
	}
}

func newChatImageBridgeTestContext(t *testing.T, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req

	groupID := int64(49)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID:      1,
		UserID:  7,
		GroupID: &groupID,
		Group: &service.Group{
			ID:                   groupID,
			AllowImageGeneration: false,
		},
		User: &service.User{ID: 7},
	})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 7, Concurrency: 1})
	return c, rec
}

func TestChatCompletionsImageBridgeBypassesGPTImageEndpointGuard(t *testing.T) {
	body := []byte(`{
		"model":"gpt-image-2",
		"messages":[{"role":"user","content":"draw a tiny blue square"}]
	}`)
	c, rec := newChatImageBridgeTestContext(t, body)

	newChatImageBridgeTestHandler(true).ChatCompletions(c)

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Equal(t, "permission_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	require.Contains(t, rec.Body.String(), service.ImageGenerationPermissionMessage())
	require.NotContains(t, rec.Body.String(), "This model is not supported on the Chat Completions endpoint")
}

// Regression: chat.completions bridge must rewrite URL path before Images() so
// ParseOpenAIImagesRequest does not return "unsupported images endpoint".
func TestTryChatCompletionsImageBridgeRewritesPathForImagesPipeline(t *testing.T) {
	body := []byte(`{
		"model":"gpt-image-2",
		"messages":[{"role":"user","content":"draw a tiny blue square"}]
	}`)
	c, rec := newChatImageBridgeTestContext(t, body)
	h := newChatImageBridgeTestHandler(true)

	handled := h.tryChatCompletionsImageBridge(c, zap.NewNop(), body, "gpt-image-2", "gpt-image-2")
	require.True(t, handled)

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Equal(t, "permission_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	require.Contains(t, rec.Body.String(), service.ImageGenerationPermissionMessage())
	require.Equal(t, "/v1/chat/completions", c.Request.URL.Path)
}

func TestTryChatCompletionsImageBridgeUsesEditsPathWhenReferenceImages(t *testing.T) {
	body := []byte(`{
		"model":"gpt-image-2",
		"messages":[{
			"role":"user",
			"content":[
				{"type":"text","text":"make it blue"},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,abc"}}
			]
		}]
	}`)
	c, rec := newChatImageBridgeTestContext(t, body)
	h := newChatImageBridgeTestHandler(true)

	handled := h.tryChatCompletionsImageBridge(c, zap.NewNop(), body, "gpt-image-2", "gpt-image-2")
	require.True(t, handled)

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Equal(t, "permission_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	require.Equal(t, "/v1/chat/completions", c.Request.URL.Path)
}
