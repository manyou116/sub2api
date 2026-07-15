//go:build unit

package xai

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFreeUsageExhausted(t *testing.T) {
	t.Parallel()
	require.True(t, FreeUsageExhausted([]byte(`{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for model grok-4.5-build-free for now."}`)))
	require.True(t, FreeUsageExhausted([]byte(`included free usage`)))
	require.False(t, FreeUsageExhausted([]byte(`{"code":"rate_limit"}`)))
	require.False(t, FreeUsageExhausted(nil))
}

func TestFreeUsageTokenWindow(t *testing.T) {
	t.Parallel()
	// Live cli-chat-proxy Free account sample (2026-07-15).
	body := []byte(`{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for model grok-4.5-build-free for now. Usage resets over a rolling 24-hour window — tokens (actual/limit): 2115457/2000000. Upgrade to a Grok subscription for higher limits: https://grok.com/supergrok"}`)
	used, limit, ok := FreeUsageTokenWindow(body)
	require.True(t, ok)
	require.Equal(t, int64(2115457), used)
	require.Equal(t, int64(2000000), limit)

	_, _, ok = FreeUsageTokenWindow([]byte(`no tokens here`))
	require.False(t, ok)
}
