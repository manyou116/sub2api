package basispoints

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeImageContentRepresentations(t *testing.T) {
	base := imageBody(t)
	var original object
	_ = json.Unmarshal(base, &original)
	url := imageLink(t, base)
	encoded := strings.TrimPrefix(url, "data:image/png;base64,")
	variants := []object{
		{"type": "image_url", "image_url": object{"url": url, "detail": "high"}},
		{"type": "input_image", "image_url": object{"url": url}},
		{"type": "image", "source": object{"type": "base64", "media_type": "image/png", "data": encoded}},
		{"type": "image", "mimeType": "image/png", "data": encoded},
	}
	for _, part := range variants {
		for _, field := range []string{"content", "output"} {
			relay, err := NewImageRelay("https://images.example", t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			item := object{field: []any{part}}
			if field == "output" {
				item["type"] = "function_call_output"
			}
			raw, _ := json.Marshal(object{"input": []any{item}})
			out, err := relay.Rewrite(raw, "scope")
			if err != nil {
				t.Fatal(err)
			}
			var v object
			_ = json.Unmarshal(out, &v)
			items, _ := v["input"].([]any)
			if len(items) != 1 {
				t.Fatal("unexpected item count")
			}
			item, _ = items[0].(object)
			parts, _ := item[field].([]any)
			if len(parts) != 1 {
				t.Fatal("unexpected part count")
			}
			p, _ := parts[0].(object)
			if p["type"] != "input_image" || !strings.HasPrefix(text(p["image_url"]), "https://images.example/") {
				t.Fatal("image not normalized and relayed")
			}
			if err := relay.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
}
func TestNormalizeHTTPSWithoutRelayAndRejectUnknown(t *testing.T) {
	source := object{"input": []any{object{"content": []any{object{"type": "image_url", "image_url": object{"url": "https://example.com/a.png", "detail": "low"}}}}}}
	if err := normalizeContent(source); err != nil {
		t.Fatal(err)
	}
	items, _ := source["input"].([]any)
	if len(items) != 1 {
		t.Fatal("unexpected item count")
	}
	item, _ := items[0].(object)
	parts, _ := item["content"].([]any)
	if len(parts) != 1 {
		t.Fatal("unexpected part count")
	}
	if err := validateHistoryContent(parts); err != nil {
		t.Fatal(err)
	}
	part, _ := parts[0].(object)
	if part["detail"] != "low" {
		t.Fatal("detail lost")
	}
	err := validateHistoryContent([]any{object{"type": "secret-user-content"}})
	if err == nil || strings.Contains(err.Error(), "secret-user-content") {
		t.Fatal("unknown content must fail without leaking arbitrary values")
	}
}
func TestToolDuplicatesCompareCanonicalContracts(t *testing.T) {
	b := &Bridge{tools: map[string]tool{}}
	schema := object{"type": "object", "properties": object{"cmd": object{"type": "string"}}}
	catalog, err := b.collectTools([]any{object{"type": "function", "name": "exec", "parameters": schema, "description": "old"}, object{"type": "function", "name": "exec", "input_schema": schema, "description": "new", "metadata": "presentation-only"}}, "")
	if err != nil || len(catalog) != 1 {
		t.Fatalf("equivalent tools rejected: %v", err)
	}
	_, err = b.collectTools([]any{object{"type": "function", "name": "exec", "parameters": object{"type": "object", "properties": object{"other": object{"type": "number"}}}}}, "")
	if err == nil {
		t.Fatal("conflicting schemas must not be silently merged")
	}
}

func TestAgentMessageEncryptedHistory(t *testing.T) {
	for _, tc := range []struct {
		name      string
		kind      string
		parts     []any
		wantError bool
	}{
		{"mixed", "agent_message", []any{object{"type": "input_text", "text": "visible"}, object{"type": "encrypted_content", "encrypted_content": "opaque-state"}, object{"type": "input_image", "image_url": "https://example.com/image.png"}}, false},
		{"opaque only", "agent_message", []any{object{"type": "encrypted_content", "encrypted_content": "opaque-state"}}, false},
		{"empty", "agent_message", []any{object{"type": "encrypted_content", "encrypted_content": ""}}, true},
		{"invalid value", "agent_message", []any{object{"type": "encrypted_content", "encrypted_content": 42}}, true},
		{"unknown", "agent_message", []any{object{"type": "secret-user-content"}}, true},
		{"wrong item", "message", []any{object{"type": "encrypted_content", "encrypted_content": "opaque-state"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := object{"type": tc.kind, "id": "agent_1", "author": object{"role": "assistant"}, "recipient": "all", "content": tc.parts}
			raw, _ := json.Marshal(object{"model": "gpt-6-astra", "input": []any{item}})
			out, _, err := Prepare(raw, "test-scope", nil)
			if (err != nil) != tc.wantError {
				t.Fatalf("unexpected validation result: %v", err)
			}
			if err != nil {
				if strings.Contains(err.Error(), "opaque-state") || strings.Contains(err.Error(), "secret-user-content") {
					t.Fatal("diagnostic exposed content")
				}
				return
			}
			var body object
			if err := json.Unmarshal(out, &body); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, v := range fixtureValue[[]any](body["input"]) {
				got, _ := v.(object)
				if got["id"] == "agent_1" {
					found = true
					if !reflect.DeepEqual(got, item) {
						t.Fatal("agent message or opaque state changed")
					}
				}
			}
			if !found {
				t.Fatal("agent history was dropped")
			}
		})
	}
}
