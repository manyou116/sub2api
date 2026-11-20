package basispoints

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func prepareUpstreamTask(t *testing.T, scope string, input []any, options PrepareOptions, cache *ReplayCache) (object, *Bridge) {
	t.Helper()
	raw, _ := json.Marshal(object{"model": "gpt-6-astra", "input": input, "prompt_cache_key": "client-cache"})
	prepared, bridge, err := PrepareWithOptions(raw, scope, cache, options)
	if err != nil {
		t.Fatal(err)
	}
	var body object
	if err := decode(prepared, &body); err != nil {
		t.Fatal(err)
	}
	return body, bridge
}

func TestUpstreamTaskSharedAcrossScopesDoesNotShareContextOrCache(t *testing.T) {
	cache := &ReplayCache{}
	task := strings.Repeat("a", 64)
	aInput := []any{message("user", "only caller A's context")}
	bInput := []any{message("user", "only caller B's context")}
	a, aBridge := prepareUpstreamTask(t, "auth/caller-A", aInput, PrepareOptions{SessionID: "chat-A", UpstreamTaskID: task, UpstreamTurnID: strings.Repeat("b", 64)}, cache)
	b, bBridge := prepareUpstreamTask(t, "auth/caller-B", bInput, PrepareOptions{SessionID: "chat-B", UpstreamTaskID: task, UpstreamTurnID: strings.Repeat("c", 64)}, cache)
	if sessionMetadata(a, "task_id") != task || sessionMetadata(b, "task_id") != task {
		t.Fatal("leased task changed with caller or local logical session")
	}
	if sessionMetadata(a, "turn_id") == sessionMetadata(b, "turn_id") {
		t.Fatal("new generation did not use its supplied turn")
	}
	if aBridge.scope != "auth/caller-A" || bBridge.scope != "auth/caller-B" || aBridge.replay != cache || bBridge.replay != cache {
		t.Fatal("upstream identity replaced local replay scope")
	}
	if !reflect.DeepEqual(fixtureValue[[]any](a["input"])[1:], aInput) || !reflect.DeepEqual(fixtureValue[[]any](b["input"])[1:], bInput) {
		t.Fatal("another caller's context was inserted")
	}
	if a["prompt_cache_key"] == b["prompt_cache_key"] {
		t.Fatal("upstream task pool collapsed scoped prompt-cache identity")
	}
}

func TestUpstreamTaskKeepsNativeToolReplayCallerScoped(t *testing.T) {
	cache := &ReplayCache{}
	options := PrepareOptions{UpstreamTaskID: strings.Repeat("a", 64), UpstreamTurnID: strings.Repeat("b", 64)}
	call := object{"type": "function_call", "call_id": "call_client_A", "name": "lookup", "arguments": `{"item":"A"}`}
	output := object{"type": "function_call_output", "call_id": "call_client_A", "output": "answer A"}
	prepareUpstreamTask(t, "caller-A", []any{call, output}, options, cache)
	if cache.get("caller-A", "call_client_A") == nil {
		t.Fatal("fixture did not retain a native call")
	}
	options.UpstreamTurnID = strings.Repeat("c", 64)
	raw, _ := json.Marshal(object{"model": "gpt-6-astra", "input": []any{output}})
	if _, _, err := PrepareWithOptions(raw, "caller-A", cache, options); err != nil {
		t.Fatalf("same caller could not replay its tool output: %v", err)
	}
	if _, _, err := PrepareWithOptions(raw, "caller-B", cache, options); err == nil || !strings.Contains(err.Error(), "original tool item is unavailable") {
		t.Fatal("shared task exposed caller A's tool replay to caller B", err)
	}
}

func TestUpstreamTaskIdentityValidationAndClientPayloadCannotOverride(t *testing.T) {
	raw := []byte(`{"model":"gpt-6-astra","input":"hello"}`)
	valid := strings.Repeat("a", 64)
	for _, options := range []PrepareOptions{
		{UpstreamTaskID: valid},
		{UpstreamTurnID: valid},
		{UpstreamTaskID: "x", UpstreamTurnID: valid},
		{UpstreamTaskID: valid, UpstreamTurnID: strings.Repeat("g", 64)},
		{UpstreamTaskID: valid, UpstreamTurnID: strings.Repeat("A", 64)},
		{UpstreamTaskID: " " + valid, UpstreamTurnID: valid},
	} {
		if _, _, err := PrepareWithOptions(raw, "caller", nil, options); err == nil {
			t.Fatal("malformed internal task identity accepted")
		}
	}
	before, _, err := Prepare(raw, "caller", nil)
	if err != nil {
		t.Fatal(err)
	}
	clientBody := object{"model": "gpt-6-astra", "input": "hello", "task_id": valid, "turn_id": valid, "metadata": object{"task_id": valid, "turn_id": valid}, "upstream_task_id": valid, "upstream_turn_id": valid}
	injected, _ := json.Marshal(clientBody)
	after, _, err := PrepareWithOptions(injected, "caller", nil, PrepareOptions{})
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("client body selected the internal task identity", err)
	}
}
