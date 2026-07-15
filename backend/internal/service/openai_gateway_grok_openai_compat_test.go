//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestSanitizeGrokResponsesImages_FlattensObjectImageURL(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model":"grok-4.5",
		"input":[{
			"type":"message",
			"role":"user",
			"content":[
				{"type":"input_text","text":"what is in this image?"},
				{"type":"input_image","image_url":{"url":"data:image/png;base64,QUJD"},"detail":"auto"}
			]
		}]
	}`)

	out, err := sanitizeGrokResponsesImages(body)
	require.NoError(t, err)
	require.Equal(t, "input_image", gjson.GetBytes(out, "input.0.content.1.type").String())
	require.Equal(t, "data:image/png;base64,QUJD", gjson.GetBytes(out, "input.0.content.1.image_url").String())
	require.Equal(t, "auto", gjson.GetBytes(out, "input.0.content.1.detail").String())
	require.False(t, gjson.GetBytes(out, "input.0.content.1.image_url.url").Exists())
}

func TestSanitizeGrokResponsesImages_ConvertsChatStyleImageURLPart(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model":"grok-4.5",
		"input":[{
			"role":"user",
			"content":[
				{"type":"text","text":"describe"},
				{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,Zm9v"}}
			]
		}]
	}`)

	out, err := sanitizeGrokResponsesImages(body)
	require.NoError(t, err)
	require.Equal(t, "input_image", gjson.GetBytes(out, "input.0.content.1.type").String())
	require.Equal(t, "data:image/jpeg;base64,Zm9v", gjson.GetBytes(out, "input.0.content.1.image_url").String())
}

func TestSanitizeGrokResponsesImages_DropsEmptyBase64(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model":"grok-4.5",
		"input":[{
			"type":"message",
			"role":"user",
			"content":[
				{"type":"input_text","text":"hi"},
				{"type":"input_image","image_url":"data:image/png;base64,"},
				{"type":"input_image","image_url":{"url":"data:image/png;base64,   "}}
			]
		}]
	}`)

	out, err := sanitizeGrokResponsesImages(body)
	require.NoError(t, err)
	require.Equal(t, 1, int(gjson.GetBytes(out, "input.0.content.#").Int()))
	require.Equal(t, "input_text", gjson.GetBytes(out, "input.0.content.0.type").String())
	require.Equal(t, "hi", gjson.GetBytes(out, "input.0.content.0.text").String())
}

func TestSanitizeGrokResponsesImages_DropsImageOnlyEmptyMessage(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model":"grok-4.5",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"only text"}]}
		]
	}`)

	out, err := sanitizeGrokResponsesImages(body)
	require.NoError(t, err)
	require.Equal(t, 1, int(gjson.GetBytes(out, "input.#").Int()))
	require.Equal(t, "only text", gjson.GetBytes(out, "input.0.content.0.text").String())
}

func TestPatchGrokResponsesBody_NormalizesCodexVisionPayload(t *testing.T) {
	t.Parallel()

	// Codex-like Responses body: object image_url + include + namespace tools.
	body := []byte(`{
		"model":"grok-4.5",
		"stream":true,
		"store":true,
		"include":["reasoning.encrypted_content"],
		"tools":[
			{"type":"namespace","name":"client_tools"},
			{"type":"function","name":"shell","parameters":{"type":"object"}}
		],
		"input":[{
			"type":"message",
			"role":"user",
			"content":[
				{"type":"input_text","text":"read this screenshot"},
				{"type":"input_image","detail":"high","image_url":{"url":"data:image/png;base64,QUJDRA=="}}
			]
		}]
	}`)

	patched, err := patchGrokResponsesBody(body, "grok-4.5")
	require.NoError(t, err)
	require.Equal(t, "grok-4.5", gjson.GetBytes(patched, "model").String())
	require.False(t, gjson.GetBytes(patched, "include").Exists(), "Codex include must be stripped for Grok")
	require.False(t, gjson.GetBytes(patched, "store").Exists())
	require.False(t, gjson.GetBytes(patched, `tools.#(type=="namespace")`).Exists())
	require.True(t, gjson.GetBytes(patched, `tools.#(type=="function")`).Exists())
	require.Equal(t, "input_image", gjson.GetBytes(patched, "input.0.content.1.type").String())
	require.Equal(t, "data:image/png;base64,QUJDRA==", gjson.GetBytes(patched, "input.0.content.1.image_url").String())
	require.Equal(t, "high", gjson.GetBytes(patched, "input.0.content.1.detail").String())
	require.Equal(t, gjson.String, gjson.GetBytes(patched, "input.0.content.1.image_url").Type)
}

func TestSanitizeGrokResponsesImages_TopLevelInputImage(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"input":[
			{"type":"input_image","image_url":{"url":"data:image/png;base64,QUI="}},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"caption this"}]}
		]
	}`)
	out, err := sanitizeGrokResponsesImages(body)
	require.NoError(t, err)
	require.Equal(t, "input_image", gjson.GetBytes(out, "input.0.type").String())
	require.Equal(t, "data:image/png;base64,QUI=", gjson.GetBytes(out, "input.0.image_url").String())
}


func TestSanitizeGrokMessageNames_DropsNonUserNames(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"input":[
			{"type":"message","role":"developer","name":"system","content":[{"type":"input_text","text":"sys"}]},
			{"type":"message","role":"assistant","name":"bot","content":[{"type":"output_text","text":"hi"}]},
			{"type":"message","role":"user","name":"alice","content":[{"type":"input_text","text":"q"}]},
			{"type":"function_call","name":"shell","call_id":"c1","arguments":"{}"}
		]
	}`)

	out, err := sanitizeGrokMessageNames(body)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(out, "input.0.name").Exists())
	require.False(t, gjson.GetBytes(out, "input.1.name").Exists())
	require.Equal(t, "alice", gjson.GetBytes(out, "input.2.name").String())
	require.Equal(t, "shell", gjson.GetBytes(out, "input.3.name").String())
}

func TestPatchGrokResponsesBody_StripsIllegalNamesAndKeepsVision(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model":"grok-4.5",
		"stream":true,
		"input":[
			{"type":"message","role":"developer","name":"system","content":[{"type":"input_text","text":"You are Codex."}]},
			{"type":"message","role":"user","name":"u1","content":[
				{"type":"input_text","text":"what color?"},
				{"type":"input_image","detail":"high","image_url":{"url":"data:image/png;base64,QUJD"}}
			]}
		]
	}`)

	patched, err := patchGrokResponsesBody(body, "grok-4.5")
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(patched, "input.0.name").Exists())
	require.Equal(t, "u1", gjson.GetBytes(patched, "input.1.name").String())
	require.Equal(t, "input_image", gjson.GetBytes(patched, "input.1.content.1.type").String())
	require.Equal(t, "data:image/png;base64,QUJD", gjson.GetBytes(patched, "input.1.content.1.image_url").String())
	require.Equal(t, "high", gjson.GetBytes(patched, "input.1.content.1.detail").String())
}
