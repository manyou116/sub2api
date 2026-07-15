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

// Regression: chat.completions bridge must rewrite URL path before Images() so
// ParseOpenAIImagesRequest does not return "unsupported images endpoint".
func TestTryChatCompletionsImageBridge_rewritesPathForImagesPipeline(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{
		"model":"gpt-image-2",
		"messages":[{"role":"user","content":"draw a tiny blue square"}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req

	groupID := int64(49)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID:      1,
		GroupID: &groupID,
		Group: &service.Group{
			ID:                   groupID,
			AllowImageGeneration: false,
		},
		User: &service.User{ID: 7},
	})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 7, Concurrency: 1})

	h := &OpenAIGatewayHandler{
		gatewayService:      &service.OpenAIGatewayService{},
		billingCacheService: &service.BillingCacheService{},
		apiKeyService:       &service.APIKeyService{},
		concurrencyHelper:   &ConcurrencyHelper{concurrencyService: &service.ConcurrencyService{}},
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				OpenAIChatImageBridge: config.OpenAIChatImageBridgeConfig{
					Enabled:       true,
					ResponseStyle: service.ChatImageBridgeStyleMarkdownDataURL,
				},
			},
		},
	}

	handled := h.tryChatCompletionsImageBridge(c, zap.NewNop(), body, "gpt-image-2", "gpt-image-2")
	require.True(t, handled)

	// Path rewrite succeeded: Images parser ran and group permission rejected before scheduling.
	// Before the fix this was 400 invalid_request_error "unsupported images endpoint".
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Equal(t, "permission_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	require.Contains(t, rec.Body.String(), service.ImageGenerationPermissionMessage())
	// Original request path restored for logging/context.
	require.Equal(t, "/v1/chat/completions", c.Request.URL.Path)
}

func TestTryChatCompletionsImageBridge_editsPathWhenReferenceImages(t *testing.T) {
	gin.SetMode(gin.TestMode)

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
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req

	groupID := int64(50)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID:      2,
		GroupID: &groupID,
		Group: &service.Group{
			ID:                   groupID,
			AllowImageGeneration: false,
		},
		User: &service.User{ID: 8},
	})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 8, Concurrency: 1})

	h := &OpenAIGatewayHandler{
		gatewayService:      &service.OpenAIGatewayService{},
		billingCacheService: &service.BillingCacheService{},
		apiKeyService:       &service.APIKeyService{},
		concurrencyHelper:   &ConcurrencyHelper{concurrencyService: &service.ConcurrencyService{}},
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				OpenAIChatImageBridge: config.OpenAIChatImageBridgeConfig{
					Enabled: true,
				},
			},
		},
	}

	handled := h.tryChatCompletionsImageBridge(c, zap.NewNop(), body, "gpt-image-2", "gpt-image-2")
	require.True(t, handled)
	// Same signal: path rewrite worked (edits) and permission gate ran.
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Equal(t, "permission_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	require.Equal(t, "/v1/chat/completions", c.Request.URL.Path)
}
