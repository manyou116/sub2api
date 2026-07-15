package service

import (
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// ResolveKiroCacheIdentity derives a tenant-isolated conversation identity for
// Kiro CodeWhisperer. The value is written to conversationState.conversationId
// and must never carry the client's raw session seed.
//
// Unlike Grok (where tools-only prefix is preferred for xAI prompt_cache_key),
// Kiro conversationId is a chat-thread id: prefer first-user / content anchors
// so multi-turn requests keep the same id. Tools-only seeds are last-resort
// (otherwise every tool-bearing client would share one thread and cold-cache).
//
// Fail closed: missing API key context yields empty identity so callers fall
// back carefully without cross-tenant sharing.
func ResolveKiroCacheIdentity(c *gin.Context, body []byte, explicitKey, upstreamModel string) string {
	apiKeyID := getAPIKeyIDFromContext(c)
	if apiKeyID <= 0 {
		return ""
	}
	model := strings.ToLower(strings.TrimSpace(upstreamModel))
	if model == "" {
		return ""
	}
	seed := resolveKiroCacheSeed(c, body, explicitKey)
	if seed == "" {
		return ""
	}
	return buildKiroCacheIdentity(apiKeyID, 0, model, seed)
}

// ResolveKiroConversationID is the account-scoped identity sent upstream.
// Prompt affinity on CodeWhisperer is account-local; sticky + this id together
// keep multi-turn traffic on the same OAuth account and conversation thread.
func ResolveKiroConversationID(c *gin.Context, body []byte, explicitKey, upstreamModel string, accountID int64) string {
	apiKeyID := getAPIKeyIDFromContext(c)
	if apiKeyID <= 0 || accountID <= 0 {
		return ""
	}
	model := strings.ToLower(strings.TrimSpace(upstreamModel))
	if model == "" {
		return ""
	}
	seed := resolveKiroCacheSeed(c, body, explicitKey)
	if seed == "" {
		return ""
	}
	return buildKiroCacheIdentity(apiKeyID, accountID, model, seed)
}

func buildKiroCacheIdentity(apiKeyID, accountID int64, model, seed string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	seed = strings.TrimSpace(seed)
	if apiKeyID <= 0 || model == "" || seed == "" {
		return ""
	}
	if accountID > 0 {
		return generateSessionUUID(fmt.Sprintf("kiro-prompt-cache:v2:%d:%d:%s:%s", apiKeyID, accountID, model, seed))
	}
	// accountID=0: sticky/session-hash fallback only (never send upstream alone)
	return generateSessionUUID(fmt.Sprintf("kiro-prompt-cache:v2:%d:0:%s:%s", apiKeyID, model, seed))
}

// resolveKiroCacheSeed returns a stable multi-turn seed for sticky + conversation id.
func resolveKiroCacheSeed(c *gin.Context, body []byte, explicitKey string) string {
	if seed := explicitKiroCacheSeed(c, body, explicitKey); seed != "" {
		return seed
	}
	// 1) first-user / input anchored (stable across append-only turns)
	if seed := deriveOpenAIAnchoredContentSessionSeed(body); seed != "" {
		return seed
	}
	// 2) full content session seed (model + tools + first user)
	if seed := deriveOpenAIContentSessionSeed(body); seed != "" {
		return seed
	}
	// 3) tools/system-only prefix — last resort for tool-only probes
	if seed := deriveOpenAIStablePrefixSessionSeed(body); seed != "" {
		return seed
	}
	return ""
}

func explicitKiroCacheSeed(c *gin.Context, body []byte, explicitKey string) string {
	seed := ""
	if c != nil && c.Request != nil {
		seed = strings.TrimSpace(c.GetHeader("session_id"))
		if seed == "" {
			seed = strings.TrimSpace(c.GetHeader("conversation_id"))
		}
		if seed == "" {
			// Common client headers used by OpenAI-compatible UIs
			seed = strings.TrimSpace(c.GetHeader("X-Session-Id"))
		}
		if seed == "" {
			seed = strings.TrimSpace(c.GetHeader("X-Conversation-Id"))
		}
	}
	if seed == "" && len(body) > 0 {
		// prompt_cache_key only — do NOT use body "user" (OpenAI SDKs often
		// set a static user id that would collapse every chat into one thread).
		seed = strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String())
	}
	if seed == "" {
		seed = strings.TrimSpace(explicitKey)
	}
	return seed
}
