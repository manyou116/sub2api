//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccountTestService_RoutesKiroPlatform(t *testing.T) {
	// Guard: IsKiro must be true for platform constant used by TestAccountConnection.
	acc := &Account{Platform: PlatformKiro, Type: AccountTypeOAuth}
	require.True(t, acc.IsKiro())
	require.False(t, acc.IsGemini())
	require.False(t, acc.IsOpenAI())

	// Default model mapping used by the Kiro test path.
	require.Equal(t, "claude-sonnet-4.5", resolveKiroInternalModel(acc, "claude-sonnet-4.5"))
	// Anthropic public marketing ids must not be required by Kiro test defaults.
	require.NotEqual(t, "claude-sonnet-5", MapKiroModel("claude-sonnet-4.5"))
}

func TestKiroProbeTextStream_DeltaFragments(t *testing.T) {
	// ProbeTextStream forwards extractKiroDelta text fragments one-by-one; admin
	// account test appends each as a content SSE event (token-by-token feel).
	payloads := [][]byte{
		[]byte(`{"content":"Hel"}`),
		[]byte(`{"content":"lo"}`),
		[]byte(`{"content":"!"}`),
	}
	var got []string
	var assembled string
	for _, p := range payloads {
		ev := extractKiroDelta(p)
		if ev.Text == "" {
			continue
		}
		got = append(got, ev.Text)
		assembled += ev.Text
	}
	require.Equal(t, []string{"Hel", "lo", "!"}, got)
	require.Equal(t, "Hello!", assembled)
}

