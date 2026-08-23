package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestShouldBridgeChatCompletionsToImages(t *testing.T) {
	require.True(t, ShouldBridgeChatCompletionsToImages("gpt-image-2"))
	require.True(t, ShouldBridgeChatCompletionsToImages("GPT-IMAGE-1.5"))
	require.False(t, ShouldBridgeChatCompletionsToImages("gpt-5.4"))
	require.False(t, ShouldBridgeChatCompletionsToImages(""))
}

func TestBuildOpenAIImagesBodyFromChatCompletions_stringContent(t *testing.T) {
	chat := []byte(`{
		"model":"gpt-image-2",
		"messages":[{"role":"user","content":"draw a blue square"}],
		"size":"1024x1024",
		"n":2
	}`)
	body, err := BuildOpenAIImagesBodyFromChatCompletions(chat, "gpt-image-2")
	require.NoError(t, err)
	require.Equal(t, "gpt-image-2", gjson.GetBytes(body, "model").String())
	require.Equal(t, "draw a blue square", gjson.GetBytes(body, "prompt").String())
	require.Equal(t, "1024x1024", gjson.GetBytes(body, "size").String())
	require.Equal(t, int64(2), gjson.GetBytes(body, "n").Int())
	require.Equal(t, "b64_json", gjson.GetBytes(body, "response_format").String())
	require.False(t, gjson.GetBytes(body, "stream").Bool())
}

func TestBuildOpenAIImagesBodyFromChatCompletions_multimodal(t *testing.T) {
	chat := []byte(`{
		"model":"gpt-image-2",
		"messages":[{
			"role":"user",
			"content":[
				{"type":"text","text":"make it red"},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,abc"}}
			]
		}]
	}`)
	body, err := BuildOpenAIImagesBodyFromChatCompletions(chat, "")
	require.NoError(t, err)
	require.Equal(t, "make it red", gjson.GetBytes(body, "prompt").String())
	require.Equal(t, "data:image/png;base64,abc", gjson.GetBytes(body, "images.0.image_url").String())
}

func TestBuildOpenAIImagesBodyFromChatCompletions_prefersLastUser(t *testing.T) {
	chat := []byte(`{
		"model":"gpt-image-2",
		"messages":[
			{"role":"user","content":"first"},
			{"role":"assistant","content":"ok"},
			{"role":"user","content":"second prompt"}
		]
	}`)
	body, err := BuildOpenAIImagesBodyFromChatCompletions(chat, "gpt-image-2")
	require.NoError(t, err)
	require.Equal(t, "second prompt", gjson.GetBytes(body, "prompt").String())
}

func TestBuildOpenAIImagesBodyFromChatCompletions_missingPrompt(t *testing.T) {
	chat := []byte(`{"model":"gpt-image-2","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://x"}}]}]}`)
	_, err := BuildOpenAIImagesBodyFromChatCompletions(chat, "gpt-image-2")
	require.Error(t, err)
}

func TestWrapImagesJSONAsChatCompletion_markdown(t *testing.T) {
	images := []byte(`{"created":1,"data":[{"b64_json":"QUJD"}]}`)
	out, err := WrapImagesJSONAsChatCompletion(images, "gpt-image-2", ChatImageBridgeStyleMarkdownDataURL)
	require.NoError(t, err)
	require.Equal(t, "chat.completion", gjson.GetBytes(out, "object").String())
	require.Equal(t, "assistant", gjson.GetBytes(out, "choices.0.message.role").String())
	require.Contains(t, gjson.GetBytes(out, "choices.0.message.content").String(), "data:image/png;base64,QUJD")
	require.Equal(t, "stop", gjson.GetBytes(out, "choices.0.finish_reason").String())
}

func TestWrapImagesJSONAsChatCompletion_multimodal(t *testing.T) {
	images := []byte(`{"created":1,"data":[{"b64_json":"QUJD"}]}`)
	out, err := WrapImagesJSONAsChatCompletion(images, "gpt-image-2", ChatImageBridgeStyleMultimodalParts)
	require.NoError(t, err)
	require.Equal(t, "image_url", gjson.GetBytes(out, "choices.0.message.content.0.type").String())
	require.Contains(t, gjson.GetBytes(out, "choices.0.message.content.0.image_url.url").String(), "QUJD")
}

func TestWrapImagesJSONAsChatCompletion_passthroughError(t *testing.T) {
	images := []byte(`{"error":{"message":"nope","type":"upstream_error"}}`)
	out, err := WrapImagesJSONAsChatCompletion(images, "gpt-image-2", "")
	require.NoError(t, err)
	require.Equal(t, "nope", gjson.GetBytes(out, "error.message").String())
}

func TestResolveChatImageBridgeEndpoint(t *testing.T) {
	require.Equal(t, openAIImagesGenerationsEndpoint, ResolveChatImageBridgeEndpoint([]byte(`{"model":"gpt-image-2","prompt":"x"}`)))
	require.Equal(t, openAIImagesEditsEndpoint, ResolveChatImageBridgeEndpoint([]byte(`{"model":"gpt-image-2","prompt":"x","images":[{"image_url":"data:image/png;base64,abc"}]}`)))
	require.Equal(t, openAIImagesEditsEndpoint, ResolveChatImageBridgeEndpoint([]byte(`{"model":"gpt-image-2","prompt":"x","image":"data:image/png;base64,abc"}`)))
}
