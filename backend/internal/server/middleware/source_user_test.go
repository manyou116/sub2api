//go:build unit

package middleware

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSourceUserTrustAndHeaderRemoval(t *testing.T) {
	for _, tc := range []struct {
		name, source, payload string
		key                   int64
		accepted              bool
	}{
		{"bound key", "factory", `{"user_id":"123","username":"用户甲","source_id":"forged","source_name":"forged"}`, 3, true},
		{"unbound key", "factory", `{"user_id":"123"}`, 4, false},
		{"wrong site", "other", `{"user_id":"123"}`, 3, false},
		{"invalid user", "factory", `{"user_id":"bad user"}`, 3, false},
		{"invalid JSON", "factory", `{`, 3, false},
		{"oversized", "factory", strings.Repeat("a", 2049), 3, false},
		{"control character", "factory", `{"user_id":"123","username":"evil\nname"}`, 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := service.NewSettingService(fakeSettingRepo{values: map[string]string{"source_user_integrations": `[{"id":"factory","name":"Factory","api_key_ids":[3]}]`}}, nil)
			router := gin.New()
			router.Use(func(c *gin.Context) { c.Set(string(ContextKeyAPIKey), &service.APIKey{ID: tc.key}); c.Next() }, SourceUser(settings))
			router.POST("/", func(c *gin.Context) {
				require.Empty(t, c.GetHeader("X-Sub2api-Source"))
				require.Empty(t, c.GetHeader("X-Sub2api-User"))
				identity := service.SourceUserFromContext(c.Request.Context())
				if tc.accepted {
					require.Equal(t, &service.SourceUser{SourceID: "factory", SourceName: "Factory", UserID: "123", Username: "用户甲"}, identity)
				} else {
					require.Nil(t, identity)
				}
				c.Status(http.StatusNoContent)
			})
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.Header.Set("X-Sub2api-Source", tc.source)
			req.Header.Set("X-Sub2api-User", base64.RawURLEncoding.EncodeToString([]byte(tc.payload)))
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			require.Equal(t, http.StatusNoContent, w.Code)
		})
	}
}
