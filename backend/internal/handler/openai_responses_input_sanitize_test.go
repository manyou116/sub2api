package handler

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tidwall/gjson"
)

func TestSanitizeOpenAIResponsesInput_RewritesCallPrefixToFcPrefix(t *testing.T) {
	body := []byte(`{
		"model": "gpt-5.5",
		"input": [
			{"role": "user", "content": "hi"},
			{
				"id": "call_Bmc3n8wEPuB408BfwjnMb9ZK",
				"type": "function_call",
				"call_id": "call_Bmc3n8wEPuB408BfwjnMb9ZK",
				"name": "read",
				"arguments": "{}"
			}
		]
	}`)

	out := sanitizeOpenAIResponsesInput(body)

	id := gjson.GetBytes(out, "input.1.id").String()
	callID := gjson.GetBytes(out, "input.1.call_id").String()
	assert.Equal(t, "fc_Bmc3n8wEPuB408BfwjnMb9ZK", id, "id should be rewritten to fc_ prefix")
	assert.Equal(t, "call_Bmc3n8wEPuB408BfwjnMb9ZK", callID, "call_id must remain untouched (pairing key)")
}

func TestSanitizeOpenAIResponsesInput_NonCallPrefixGetsFcPrepended(t *testing.T) {
	body := []byte(`{"input":[{"id":"abc123","type":"function_call","call_id":"call_x","name":"f","arguments":"{}"}]}`)
	out := sanitizeOpenAIResponsesInput(body)
	assert.Equal(t, "fc_abc123", gjson.GetBytes(out, "input.0.id").String())
}

func TestSanitizeOpenAIResponsesInput_AlreadyCompliantIdLeftAlone(t *testing.T) {
	body := []byte(`{"input":[{"id":"fc_existing","type":"function_call","call_id":"call_x","name":"f","arguments":"{}"}]}`)
	out := sanitizeOpenAIResponsesInput(body)
	assert.Equal(t, string(body), string(out), "compliant body must be byte-identical")
}

func TestSanitizeOpenAIResponsesInput_NoIdFieldLeftAlone(t *testing.T) {
	body := []byte(`{"input":[{"type":"function_call","call_id":"call_x","name":"f","arguments":"{}"}]}`)
	out := sanitizeOpenAIResponsesInput(body)
	assert.Equal(t, string(body), string(out))
}

func TestSanitizeOpenAIResponsesInput_OtherItemTypesUntouched(t *testing.T) {
	body := []byte(`{"input":[
		{"role":"user","content":"hi"},
		{"type":"function_call_output","call_id":"call_x","output":"ok"},
		{"id":"call_X","type":"message","role":"assistant","content":"world"}
	]}`)
	out := sanitizeOpenAIResponsesInput(body)
	assert.Equal(t, string(body), string(out))
}

func TestSanitizeOpenAIResponsesInput_MultipleFunctionCallsAllRewritten(t *testing.T) {
	body := []byte(`{"input":[
		{"id":"call_a","type":"function_call","call_id":"call_a","name":"f1","arguments":"{}"},
		{"id":"call_b","type":"function_call","call_id":"call_b","name":"f2","arguments":"{}"},
		{"id":"fc_c","type":"function_call","call_id":"call_c","name":"f3","arguments":"{}"}
	]}`)
	out := sanitizeOpenAIResponsesInput(body)
	assert.Equal(t, "fc_a", gjson.GetBytes(out, "input.0.id").String())
	assert.Equal(t, "fc_b", gjson.GetBytes(out, "input.1.id").String())
	assert.Equal(t, "fc_c", gjson.GetBytes(out, "input.2.id").String())
	assert.Equal(t, "call_a", gjson.GetBytes(out, "input.0.call_id").String())
	assert.Equal(t, "call_b", gjson.GetBytes(out, "input.1.call_id").String())
	assert.Equal(t, "call_c", gjson.GetBytes(out, "input.2.call_id").String())
}

func TestSanitizeOpenAIResponsesInput_NoInputArrayLeftAlone(t *testing.T) {
	body := []byte(`{"model":"gpt-5","stream":true}`)
	out := sanitizeOpenAIResponsesInput(body)
	assert.Equal(t, string(body), string(out))
}

func TestSanitizeOpenAIResponsesInput_StringInputLeftAlone(t *testing.T) {
	body := []byte(`{"input":"just a string"}`)
	out := sanitizeOpenAIResponsesInput(body)
	assert.Equal(t, string(body), string(out))
}

func TestSanitizeOpenAIResponsesInput_InvalidJsonLeftAlone(t *testing.T) {
	body := []byte(`not json at all`)
	out := sanitizeOpenAIResponsesInput(body)
	assert.Equal(t, string(body), string(out))
}
