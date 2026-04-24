package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBuildOpenAIImageCompatRequestBodyFromChatCompletions(t *testing.T) {
	body := []byte(`{
		"model":"gpt-image-2",
		"stream":true,
		"messages":[
			{"role":"system","content":"you are an image model"},
			{"role":"user","content":[
				{"type":"text","text":"draw a tiny green square"},
				{"type":"image_url","image_url":{"url":"https://example.com/reference.png"}}
			]}
		],
		"size":"1024x1024",
		"background":"transparent",
		"n":2
	}`)

	compatBody, matched, err := BuildOpenAIImageCompatRequestBodyFromChatCompletions(body)
	require.NoError(t, err)
	require.True(t, matched)
	require.True(t, gjson.ValidBytes(compatBody))
	require.Equal(t, "gpt-image-2", gjson.GetBytes(compatBody, "model").String())
	require.Equal(t, "draw a tiny green square", gjson.GetBytes(compatBody, "prompt").String())
	require.Equal(t, "1024x1024", gjson.GetBytes(compatBody, "size").String())
	require.Equal(t, "transparent", gjson.GetBytes(compatBody, "background").String())
	require.Equal(t, int64(2), gjson.GetBytes(compatBody, "n").Int())
	require.Equal(t, "markdown", gjson.GetBytes(compatBody, "response_format").String())
	require.False(t, gjson.GetBytes(compatBody, "stream").Exists())
}

func TestBuildOpenAIImageCompatRequestBodyFromChatCompletions_MissingPrompt(t *testing.T) {
	body := []byte(`{
		"model":"gpt-image-2",
		"messages":[{"role":"assistant","content":"no user prompt"}]
	}`)

	compatBody, matched, err := BuildOpenAIImageCompatRequestBodyFromChatCompletions(body)
	require.Nil(t, compatBody)
	require.True(t, matched)
	require.ErrorContains(t, err, "chat completions image generation requires a user text prompt")
}

func TestBuildOpenAIImageCompatRequestBodyFromResponses(t *testing.T) {
	body := []byte(`{
		"model":"gpt-image-2",
		"stream":true,
		"input":[
			{"role":"user","content":[{"type":"input_text","text":"draw a skyline at dusk"}]}
		],
		"tools":[{"type":"image_generation","size":"1536x1024","quality":"high","output_format":"webp","background":"opaque"}]
	}`)

	compatBody, matched, err := BuildOpenAIImageCompatRequestBodyFromResponses(body)
	require.NoError(t, err)
	require.True(t, matched)
	require.True(t, gjson.ValidBytes(compatBody))
	require.Equal(t, "gpt-image-2", gjson.GetBytes(compatBody, "model").String())
	require.Equal(t, "draw a skyline at dusk", gjson.GetBytes(compatBody, "prompt").String())
	require.Equal(t, "1536x1024", gjson.GetBytes(compatBody, "size").String())
	require.Equal(t, "high", gjson.GetBytes(compatBody, "quality").String())
	require.Equal(t, "webp", gjson.GetBytes(compatBody, "output_format").String())
	require.Equal(t, "opaque", gjson.GetBytes(compatBody, "background").String())
	require.Equal(t, "markdown", gjson.GetBytes(compatBody, "response_format").String())
	require.False(t, gjson.GetBytes(compatBody, "stream").Exists())
}

func TestBuildOpenAIImagesMarkdownChatCompletionsResponse(t *testing.T) {
	markdown := []byte("![image](data:image/png;base64,QUJD)\n")
	resp, err := BuildOpenAIImagesMarkdownChatCompletionsResponse(markdown, "gpt-image-2", OpenAIUsage{
		InputTokens:          7,
		OutputTokens:         11,
		CacheReadInputTokens: 3,
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, "chat.completion", resp.Object)
	require.Equal(t, "gpt-image-2", resp.Model)
	require.Len(t, resp.Choices, 1)
	require.Equal(t, "assistant", resp.Choices[0].Message.Role)
	require.Equal(t, "stop", resp.Choices[0].FinishReason)

	var content string
	require.NoError(t, json.Unmarshal(resp.Choices[0].Message.Content, &content))
	require.Equal(t, string(markdown), content)
	require.NotNil(t, resp.Usage)
	require.Equal(t, 7, resp.Usage.PromptTokens)
	require.Equal(t, 11, resp.Usage.CompletionTokens)
	require.Equal(t, 18, resp.Usage.TotalTokens)
	require.NotNil(t, resp.Usage.PromptTokensDetails)
	require.Equal(t, 3, resp.Usage.PromptTokensDetails.CachedTokens)
}

func TestBuildOpenAIImagesMarkdownResponsesResponse(t *testing.T) {
	markdown := []byte("![image](data:image/png;base64,QUJD)\n")
	resp := BuildOpenAIImagesMarkdownResponsesResponse(markdown, "gpt-image-2", OpenAIUsage{
		InputTokens:          5,
		OutputTokens:         9,
		CacheReadInputTokens: 2,
	})
	require.NotNil(t, resp)
	require.Equal(t, "response", resp.Object)
	require.Equal(t, "completed", resp.Status)
	require.Equal(t, "gpt-image-2", resp.Model)
	require.Len(t, resp.Output, 1)
	require.Equal(t, "message", resp.Output[0].Type)
	require.Equal(t, "assistant", resp.Output[0].Role)
	require.Equal(t, "completed", resp.Output[0].Status)
	require.Len(t, resp.Output[0].Content, 1)
	require.Equal(t, "output_text", resp.Output[0].Content[0].Type)
	require.Equal(t, string(markdown), resp.Output[0].Content[0].Text)
	require.NotNil(t, resp.Usage)
	require.Equal(t, 5, resp.Usage.InputTokens)
	require.Equal(t, 9, resp.Usage.OutputTokens)
	require.Equal(t, 14, resp.Usage.TotalTokens)
	require.NotNil(t, resp.Usage.InputTokensDetails)
	require.Equal(t, 2, resp.Usage.InputTokensDetails.CachedTokens)
}
