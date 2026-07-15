package xai

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// FreeUsageExhaustedCooldown is the rolling window advertised by cli-chat-proxy
// free-usage-exhausted errors ("Usage resets over a rolling 24-hour window").
const FreeUsageExhaustedCooldown = 24 * time.Hour

// FreeUsageDefaultTokenLimit is the free rolling token budget reported by xAI
// when the error body omits an explicit limit.
const FreeUsageDefaultTokenLimit = int64(2_000_000)

var freeUsageTokenWindowRe = regexp.MustCompile(`(?i)tokens\s*\(\s*actual\s*/\s*limit\s*\)\s*:\s*(\d+)\s*/\s*(\d+)`)

// FreeUsageExhausted reports free-tier usage exhaustion from an upstream body.
// Live Free accounts return this only in the JSON body (no Retry-After / ratelimit headers).
func FreeUsageExhausted(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "free-usage-exhausted") ||
		strings.Contains(lower, "included free usage")
}

// FreeUsageTokenWindow parses "tokens (actual/limit): used/limit" from free-usage errors.
func FreeUsageTokenWindow(body []byte) (used, limit int64, ok bool) {
	if len(body) == 0 {
		return 0, 0, false
	}
	m := freeUsageTokenWindowRe.FindSubmatch(body)
	if len(m) != 3 {
		return 0, 0, false
	}
	u, errU := strconv.ParseInt(string(m[1]), 10, 64)
	l, errL := strconv.ParseInt(string(m[2]), 10, 64)
	if errU != nil || errL != nil || l <= 0 {
		return 0, 0, false
	}
	return u, l, true
}
