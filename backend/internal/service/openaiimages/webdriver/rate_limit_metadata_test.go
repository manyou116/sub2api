package webdriver

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStructuredUsageLimitPreservesMetadata(t *testing.T) {
	reset := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	message := strings.TrimSpace("You\u2019ve hit your usage limit. " + strings.Repeat("long upstream explanation ", 35))
	metadata := map[string]any{"error_code": "work_usage_limit_exceeded", "error_message": message, "reset_at": reset.Format(time.RFC3339)}
	node := map[string]any{"message": map[string]any{"author": map[string]string{"role": "assistant"}, "metadata": map[string]any{"error_metadata": metadata}}}
	for _, body := range []any{map[string]any{"v": node}, node, map[string]any{"mapping": map[string]any{"node": node}}, map[string]any{"error": metadata}} {
		data, err := json.Marshal(body)
		require.NoError(t, err)
		for _, stage := range []string{"sse", "poll", "probe"} {
			got := extractTaskRateLimitError(stage, data)
			require.NotNil(t, got)
			require.Equal(t, message, got.Message)
			require.Equal(t, "work_usage_limit_exceeded", got.Code)
			require.Equal(t, stage, got.Stage)
			require.Equal(t, &reset, got.ResetAt)
			require.False(t, IsImageQuotaLimitedMessage(got.Message))
		}
		got := classifyHTTP("conversation", 429, string(data))
		require.Equal(t, ErrorKindRateLimited, got.Kind)
		require.Equal(t, message, got.Message)
	}
}

func TestUsageLimitDetectionDoesNotInspectUserContent(t *testing.T) {
	for _, body := range []string{
		`{"v":{"message":{"author":{"role":"user"},"content":{"parts":["Explain usage limit and rate_limit"]}}}}`,
		`{"mapping":{"user":{"message":{"author":{"role":"user"},"content":{"parts":["work_usage_limit_exceeded"]}}}}}`,
		`{"v":{"message":{"author":{"role":"assistant"},"content":{"parts":["The image is ready."]}}}}`,
	} {
		require.Nil(t, extractTaskRateLimitError("sse", []byte(body)))
	}
	got := classifyHTTP("conversation", 429, `{"detail":"The usage limit has been reached"}`)
	require.Equal(t, ErrorKindRateLimited, got.Kind)
	require.Equal(t, "The usage limit has been reached", got.Message)
	for _, message := range []string{"You've hit the usage limit", "The usage limit has been reached. It resets in 2 hours"} {
		require.False(t, IsImageQuotaLimitedMessage(message))
	}
}
