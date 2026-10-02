//go:build unit

package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestResolveKiroCacheIdentity_StableAndIsolated(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"claude-sonnet-4.5","prompt_cache_key":"client-session","messages":[{"role":"user","content":"hi"}]}`)

	c1, _ := gin.CreateTestContext(httptest.NewRecorder())
	req1, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c1.Request = req1
	c1.Set("api_key", &APIKey{ID: 11})
	id1a := ResolveKiroCacheIdentity(c1, body, "", "claude-sonnet-4.5")
	id1b := ResolveKiroCacheIdentity(c1, body, "", "claude-sonnet-4.5")
	require.NotEmpty(t, id1a)
	require.Equal(t, id1a, id1b)

	c2, _ := gin.CreateTestContext(httptest.NewRecorder())
	req2, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c2.Request = req2
	c2.Set("api_key", &APIKey{ID: 22})
	id2 := ResolveKiroCacheIdentity(c2, body, "", "claude-sonnet-4.5")
	require.NotEmpty(t, id2)
	require.NotEqual(t, id1a, id2)

	// No api key → empty (fail closed)
	c3, _ := gin.CreateTestContext(httptest.NewRecorder())
	req3, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c3.Request = req3
	require.Empty(t, ResolveKiroCacheIdentity(c3, body, "", "claude-sonnet-4.5"))
}
