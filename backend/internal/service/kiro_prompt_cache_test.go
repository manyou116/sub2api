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

func TestResolveKiroCacheIdentity_MultiTurnStableOnFirstUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Request = req
	c.Set("api_key", &APIKey{ID: 7})

	turn1 := []byte(`{
		"model":"claude-sonnet-4.5",
		"tools":[{"type":"function","function":{"name":"search","parameters":{"type":"object"}}}],
		"messages":[{"role":"user","content":"hello world"}]
	}`)
	turn2 := []byte(`{
		"model":"claude-sonnet-4.5",
		"tools":[{"type":"function","function":{"name":"search","parameters":{"type":"object"}}}],
		"messages":[
			{"role":"user","content":"hello world"},
			{"role":"assistant","content":"hi"},
			{"role":"user","content":"follow up"}
		]
	}`)
	id1 := ResolveKiroCacheIdentity(c, turn1, "", "claude-sonnet-4.5")
	id2 := ResolveKiroCacheIdentity(c, turn2, "", "claude-sonnet-4.5")
	require.NotEmpty(t, id1)
	require.Equal(t, id1, id2)

	// Different first user must not share the thread.
	other := []byte(`{
		"model":"claude-sonnet-4.5",
		"tools":[{"type":"function","function":{"name":"search","parameters":{"type":"object"}}}],
		"messages":[{"role":"user","content":"totally different prompt"}]
	}`)
	require.NotEqual(t, id1, ResolveKiroCacheIdentity(c, other, "", "claude-sonnet-4.5"))
}

func TestResolveKiroCacheSeed_PrefersFirstUserOverToolsOnly(t *testing.T) {
	// Tools-only prefix would collapse every tool-bearing client into one thread.
	// Content/first-user anchors must win when present.
	withToolsAndUser := []byte(`{
		"model":"claude-sonnet-4.5",
		"tools":[{"type":"function","function":{"name":"search","parameters":{"type":"object"}}}],
		"messages":[{"role":"user","content":"unique prompt A"}]
	}`)
	toolsOnly := []byte(`{
		"model":"claude-sonnet-4.5",
		"tools":[{"type":"function","function":{"name":"search","parameters":{"type":"object"}}}]
	}`)

	seedContent := resolveKiroCacheSeed(nil, withToolsAndUser, "")
	seedTools := resolveKiroCacheSeed(nil, toolsOnly, "")
	require.NotEmpty(t, seedContent)
	require.NotEmpty(t, seedTools)
	require.NotEqual(t, seedContent, seedTools)
	require.Contains(t, seedContent, "first_user=")
	require.NotContains(t, seedTools, "first_user=")
}

func TestResolveKiroConversationID_AccountScoped(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Request = req
	c.Set("api_key", &APIKey{ID: 9})

	body := []byte(`{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"hi"}]}`)
	a := ResolveKiroConversationID(c, body, "", "claude-sonnet-4.5", 101)
	b := ResolveKiroConversationID(c, body, "", "claude-sonnet-4.5", 202)
	sticky := ResolveKiroCacheIdentity(c, body, "", "claude-sonnet-4.5")

	require.NotEmpty(t, a)
	require.NotEmpty(t, b)
	require.NotEmpty(t, sticky)
	require.NotEqual(t, a, b, "different OAuth accounts must not share conversationId")
	require.NotEqual(t, a, sticky, "upstream id must be account-scoped (v2), not sticky-only identity")
	require.NotEqual(t, b, sticky)

	// Same account is stable across multi-turn appends.
	turn2 := []byte(`{"model":"claude-sonnet-4.5","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"yo"},
		{"role":"user","content":"again"}
	]}`)
	require.Equal(t, a, ResolveKiroConversationID(c, turn2, "", "claude-sonnet-4.5", 101))

	// Fail closed without account.
	require.Empty(t, ResolveKiroConversationID(c, body, "", "claude-sonnet-4.5", 0))
}

func TestExplicitKiroCacheSeed_HeadersAndPromptCacheKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Session-Id", "header-session")
	c.Request = req
	require.Equal(t, "header-session", explicitKiroCacheSeed(c, nil, ""))

	// Body prompt_cache_key when headers absent.
	c2, _ := gin.CreateTestContext(httptest.NewRecorder())
	req2, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c2.Request = req2
	body := []byte(`{"prompt_cache_key":"pck-1","user":"sdk-default-user","messages":[{"role":"user","content":"x"}]}`)
	require.Equal(t, "pck-1", explicitKiroCacheSeed(c2, body, ""))

	// body.user alone is intentionally ignored (too broad for thread affinity).
	bodyUserOnly := []byte(`{"user":"sdk-default-user","messages":[{"role":"user","content":"x"}]}`)
	require.Empty(t, explicitKiroCacheSeed(c2, bodyUserOnly, ""))
}
