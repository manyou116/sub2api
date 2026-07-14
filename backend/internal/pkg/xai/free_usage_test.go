//go:build unit

package xai

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFreeUsageExhausted(t *testing.T) {
	t.Parallel()
	require.True(t, FreeUsageExhausted([]byte(`{"code":"subscription:free-usage-exhausted","error":"included free usage"}`)))
	require.False(t, FreeUsageExhausted([]byte(`{"code":"rate_limit"}`)))
	require.False(t, FreeUsageExhausted(nil))
}

func TestFreeUsageTokenWindow(t *testing.T) {
	t.Parallel()
	body := []byte(`You've used all the included free usage for model grok-4.5-build-free for now. Usage resets over a rolling 24-hour window — tokens (actual/limit): 2090399/2000000.`)
	used, limit, ok := FreeUsageTokenWindow(body)
	require.True(t, ok)
	require.Equal(t, int64(2090399), used)
	require.Equal(t, int64(2000000), limit)

	_, _, ok = FreeUsageTokenWindow([]byte(`no tokens here`))
	require.False(t, ok)
}
