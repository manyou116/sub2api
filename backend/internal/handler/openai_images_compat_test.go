package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func parseCompatSSEChunks(body string) []string {
	parts := strings.Split(body, "\n\n")
	chunks := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		for _, line := range strings.Split(part, "\n") {
			if strings.HasPrefix(line, "data: ") {
				chunks = append(chunks, strings.TrimSpace(strings.TrimPrefix(line, "data: ")))
			}
		}
	}
	return chunks
}

type compatSSEEvent struct {
	Name string
	Data string
}

func parseCompatSSEEvents(body string) []compatSSEEvent {
	parts := strings.Split(body, "\n\n")
	events := make([]compatSSEEvent, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var evt compatSSEEvent
		for _, line := range strings.Split(part, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				evt.Name = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
			case strings.HasPrefix(line, "data: "):
				evt.Data = strings.TrimSpace(strings.TrimPrefix(line, "data: "))
			}
		}
		events = append(events, evt)
	}
	return events
}

func TestWriteOpenAIImagesCompatChatCompletionsResponse_Stream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	resp, err := service.BuildOpenAIImagesMarkdownChatCompletionsResponse(
		[]byte("![image](data:image/png;base64,QUJD)\n"),
		"gpt-image-2",
		service.OpenAIUsage{InputTokens: 4, OutputTokens: 6},
	)
	require.NoError(t, err)

	require.NoError(t, writeOpenAIImagesCompatChatCompletionsResponse(c, resp, true, true))
	require.Contains(t, rec.Header().Get("Content-Type"), "text/event-stream")

	chunks := parseCompatSSEChunks(rec.Body.String())
	require.Len(t, chunks, 5)
	require.Equal(t, "assistant", gjson.Get(chunks[0], "choices.0.delta.role").String())
	require.Equal(t, "![image](data:image/png;base64,QUJD)\n", gjson.Get(chunks[1], "choices.0.delta.content").String())
	require.Equal(t, "stop", gjson.Get(chunks[2], "choices.0.finish_reason").String())
	require.Equal(t, int64(10), gjson.Get(chunks[3], "usage.total_tokens").Int())
	require.Equal(t, "[DONE]", chunks[4])
}

func TestWriteOpenAIImagesCompatResponsesResponse_Stream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	resp := service.BuildOpenAIImagesMarkdownResponsesResponse(
		[]byte("![image](data:image/png;base64,QUJD)\n"),
		"gpt-image-2",
		service.OpenAIUsage{InputTokens: 5, OutputTokens: 8, CacheReadInputTokens: 2},
	)

	require.NoError(t, writeOpenAIImagesCompatResponsesResponse(c, resp, true))
	require.Contains(t, rec.Header().Get("Content-Type"), "text/event-stream")

	events := parseCompatSSEEvents(rec.Body.String())
	require.Len(t, events, 7)
	require.Equal(t, "response.created", events[0].Name)
	require.Equal(t, "response.output_item.added", events[1].Name)
	require.Equal(t, "response.output_text.delta", events[2].Name)
	require.Equal(t, "response.output_text.done", events[3].Name)
	require.Equal(t, "response.output_item.done", events[4].Name)
	require.Equal(t, "response.completed", events[5].Name)
	require.Equal(t, "[DONE]", events[6].Data)

	require.Equal(t, "in_progress", gjson.Get(events[1].Data, "item.status").String())
	require.Equal(t, "assistant", gjson.Get(events[1].Data, "item.role").String())
	require.False(t, gjson.Get(events[1].Data, "item.content").Exists())
	require.Equal(t, "![image](data:image/png;base64,QUJD)\n", gjson.Get(events[2].Data, "delta").String())
	require.Equal(t, "![image](data:image/png;base64,QUJD)\n", gjson.Get(events[3].Data, "text").String())
	require.Equal(t, "completed", gjson.Get(events[4].Data, "item.status").String())
	require.False(t, gjson.Get(events[4].Data, "item.content").Exists())
	require.Equal(t, "completed", gjson.Get(events[5].Data, "response.status").String())
	require.Equal(t, "![image](data:image/png;base64,QUJD)\n", gjson.Get(events[5].Data, "response.output.0.content.0.text").String())
	require.Equal(t, int64(13), gjson.Get(events[5].Data, "response.usage.total_tokens").Int())
	require.Equal(t, int64(2), gjson.Get(events[5].Data, "response.usage.input_tokens_details.cached_tokens").Int())
}
