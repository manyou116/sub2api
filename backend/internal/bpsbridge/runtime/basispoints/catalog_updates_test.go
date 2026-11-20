package basispoints

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestHistoricalToolCatalogUpdates(t *testing.T) {
	old := object{"type": "function", "name": "run", "parameters": object{"type": "object", "properties": object{"old": object{"type": "string"}}}}
	latest := object{"type": "function", "name": "run", "parameters": object{"type": "object", "properties": object{"new": object{"type": "number"}}}}
	body := object{"model": "gpt-6-astra", "tools": []any{old}, "input": []any{
		object{"type": "additional_tools", "tools": []any{old}},
		object{"type": "function_call", "call_id": "call_old", "name": "run", "arguments": "{\"old\":\"keep\"}"},
		object{"type": "function_call_output", "call_id": "call_old", "output": "old result"},
		object{"type": "additional_tools", "tools": []any{latest}},
		object{"role": "user", "content": "continue"},
	}}
	raw, _ := json.Marshal(body)
	prepared, b, err := Prepare(raw, "scope", nil)
	if err != nil {
		t.Fatal(err)
	}
	props, _ := b.tools["run"].Parameters["properties"].(object)
	if len(b.tools) != 1 || props["new"] == nil {
		t.Fatal("latest catalog not selected")
	}
	var out object
	if err = decode(prepared, &out); err != nil {
		t.Fatal(err)
	}
	var historical bool
	items, ok := out["input"].([]any)
	if !ok {
		t.Fatal("missing input items")
	}
	for _, v := range items {
		item, ok := v.(object)
		if !ok {
			t.Fatal("unexpected input item")
		}
		if text(item["call_id"]) == "call_old" && text(item["type"]) == "function_call" {
			historical = strings.Contains(text(item["arguments"]), `\"old\":\"keep\"`)
		}
		if text(item["type"]) == "additional_tools" {
			t.Fatal("catalog update leaked upstream")
		}
	}
	if !historical {
		t.Fatal("past call was lost or rewritten against new schema")
	}
}

func TestToolConflictWithinOneBatchStillRejected(t *testing.T) {
	a := object{"type": "function", "name": "run", "parameters": object{"type": "object"}}
	b := object{"type": "custom", "name": "run"}
	for _, top := range []bool{true, false} {
		body := object{"model": "gpt-6-astra", "input": []any{object{"role": "user", "content": "test"}}}
		if top {
			body["tools"] = []any{a, b}
		} else {
			body["input"] = []any{object{"type": "additional_tools", "tools": []any{a, b}}}
		}
		raw, _ := json.Marshal(body)
		if _, _, err := Prepare(raw, "scope", nil); err == nil || !strings.Contains(err.Error(), "conflicting duplicate") {
			t.Fatal("conflicting batch accepted")
		}
	}
}

func TestHistoricalConfigurationEffort(t *testing.T) {
	for _, current := range []string{"", "medium"} {
		body := object{"model": "gpt-6-astra", "input": []any{
			object{"type": "configuration_update", "reasoning": object{"effort": "low"}},
			object{"type": "configuration_update", "reasoning": object{"effort": "high"}},
			object{"role": "user", "content": "continue"},
		}}
		if current != "" {
			body["reasoning"] = object{"effort": current}
		}
		raw, _ := json.Marshal(body)
		prepared, b, err := Prepare(raw, "scope", nil)
		if err != nil {
			t.Fatal(err)
		}
		want := current
		if want == "" {
			want = "high"
		}
		if b.Effort != want || strings.Contains(string(prepared), "configuration_update") {
			t.Fatal("incorrect history configuration translation")
		}
	}
	body := object{"model": "gpt-6-astra", "input": []any{object{"type": "configuration_update", "reasoning": object{"effort": "high"}, "tools": []any{}}}}
	raw, _ := json.Marshal(body)
	if _, _, err := Prepare(raw, "scope", nil); err == nil {
		t.Fatal("unsupported update silently discarded")
	}
}
