package service

import (
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// resolveKiroCacheIdentity derives a tenant-isolated conversation identity for
// Kiro CodeWhisperer. The value is written to conversationState.conversationId
// (and agentContinuationId) and must never carry the client's raw session seed.
//
// Fail closed: missing API key context yields an empty identity so callers can
// fall back to a one-shot UUID without sharing cache across tenants.
func ResolveKiroCacheIdentity(c *gin.Context, body []byte, explicitKey, upstreamModel string) string {
	apiKeyID := getAPIKeyIDFromContext(c)
	if apiKeyID <= 0 {
		return ""
	}
	model := strings.ToLower(strings.TrimSpace(upstreamModel))
	if model == "" {
		return ""
	}

	seed := explicitKiroCacheSeed(c, body, explicitKey)
	if seed == "" {
		seed = deriveOpenAIStablePrefixSessionSeed(body)
		if seed == "" {
			seed = deriveOpenAIAnchoredContentSessionSeed(body)
		}
	}
	if seed == "" {
		return ""
	}

	isolatedSeed := fmt.Sprintf("kiro-prompt-cache:v1:%d:%s:%s", apiKeyID, model, seed)
	return generateSessionUUID(isolatedSeed)
}

func explicitKiroCacheSeed(c *gin.Context, body []byte, explicitKey string) string {
	seed := ""
	if c != nil && c.Request != nil {
		seed = strings.TrimSpace(c.GetHeader("session_id"))
		if seed == "" {
			seed = strings.TrimSpace(c.GetHeader("conversation_id"))
		}
	}
	if seed == "" && len(body) > 0 {
		seed = strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String())
	}
	if seed == "" {
		seed = strings.TrimSpace(explicitKey)
	}
	return seed
}
