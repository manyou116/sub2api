//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtractUpstreamErrorMessageXAIStringError(t *testing.T) {
	t.Parallel()

	msg := extractUpstreamErrorMessage([]byte(`{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for model grok-4.5-build-free for now."}`))
	require.Contains(t, msg, "included free usage")
	require.Equal(t, "subscription:free-usage-exhausted", extractUpstreamErrorCode([]byte(`{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage"}`)))
}

func TestExtractUpstreamErrorMessageOpenAIObjectError(t *testing.T) {
	t.Parallel()

	msg := extractUpstreamErrorMessage([]byte(`{"error":{"message":"bad request","type":"invalid_request_error","code":"invalid_value"}}`))
	require.Equal(t, "bad request", msg)
	require.Equal(t, "invalid_value", extractUpstreamErrorCode([]byte(`{"error":{"message":"bad request","type":"invalid_request_error","code":"invalid_value"}}`)))
}
