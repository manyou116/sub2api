package handler

import (
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestNewKiroOpenAIForwardResult_PropagatesUsageTelemetry(t *testing.T) {
	firstTokenMs := 123
	headers := http.Header{"X-Amzn-Requestid": []string{"req-1"}}
	result := &service.KiroChatResult{
		InternalModel:            "claude-opus-4.6",
		Stream:                   true,
		InputTokens:              100,
		OutputTokens:             20,
		CacheCreationInputTokens: 7,
		CacheReadInputTokens:     11,
		Duration:                 1500 * time.Millisecond,
		FirstTokenMs:             &firstTokenMs,
		UpstreamHeaders:          headers,
	}

	forwardResult := newKiroOpenAIForwardResult("trace-1", "claude-opus-4.6-thinking", result)

	require.Equal(t, "trace-1", forwardResult.RequestID)
	require.Equal(t, "claude-opus-4.6-thinking", forwardResult.Model)
	require.Equal(t, "claude-opus-4.6", forwardResult.UpstreamModel)
	require.True(t, forwardResult.Stream)
	require.Equal(t, headers, forwardResult.ResponseHeaders)
	require.Equal(t, 1500*time.Millisecond, forwardResult.Duration)
	require.Equal(t, &firstTokenMs, forwardResult.FirstTokenMs)
	require.Equal(t, 100, forwardResult.Usage.InputTokens)
	require.Equal(t, 20, forwardResult.Usage.OutputTokens)
	require.Equal(t, 7, forwardResult.Usage.CacheCreationInputTokens)
	require.Equal(t, 11, forwardResult.Usage.CacheReadInputTokens)
}
