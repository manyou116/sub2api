package basispoints

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func prepareSessionFixture(t *testing.T, scope, session string, input []any, cacheKey string) object {
	t.Helper()
	raw, err := json.Marshal(object{"model": "gpt-6-astra", "input": input, "prompt_cache_key": cacheKey})
	if err != nil {
		t.Fatal(err)
	}
	prepared, _, err := PrepareWithOptions(raw, scope, nil, PrepareOptions{SessionID: session})
	if err != nil {
		t.Fatal(err)
	}
	var result object
	if err := decode(prepared, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func sessionMetadata(body object, field string) string {
	return text(fixtureValue[object](body["metadata"])[field])
}

func TestExplicitSessionTaskSurvivesHistoryTrimAndCacheChange(t *testing.T) {
	first := []any{message("user", "first question")}
	full := []any{message("user", "first question"), message("assistant", "old answer"), message("user", "next question")}
	trimmed := []any{message("user", "only the client-selected summary")}
	a := prepareSessionFixture(t, "account-and-caller", "chat-A", first, "cache-1")
	b := prepareSessionFixture(t, "account-and-caller", "chat-A", full, "cache-2")
	c := prepareSessionFixture(t, "account-and-caller", "chat-A", trimmed, "")
	for _, next := range []object{b, c} {
		if sessionMetadata(next, "task_id") != sessionMetadata(a, "task_id") {
			t.Fatal("explicit task changed with context or cache hint")
		}
		if sessionMetadata(next, "turn_id") == sessionMetadata(a, "turn_id") {
			t.Fatal("new user turn reused the previous turn identity")
		}
	}
	gotInput := fixtureValue[[]any](c["input"])
	if len(gotInput) != 2 || !reflect.DeepEqual(gotInput[1:], trimmed) {
		t.Fatal("trimmed client history was expanded or changed")
	}
	retry := prepareSessionFixture(t, "account-and-caller", "chat-A", trimmed, "")
	if sessionMetadata(retry, "turn_id") != sessionMetadata(c, "turn_id") {
		t.Fatal("identical turn did not retain its deterministic identity")
	}
}

func TestExplicitSessionTaskPartitionsScopeAndSession(t *testing.T) {
	input := []any{message("user", "same first question")}
	seen := map[string]bool{}
	turns := map[string]bool{}
	for _, tc := range []struct{ scope, session string }{
		{"account-A/caller-1", "chat-A"},
		{"account-A/caller-1", "chat-B"},
		{"account-B/caller-1", "chat-A"},
		{"account-A/caller-2", "chat-A"},
	} {
		body := prepareSessionFixture(t, tc.scope, tc.session, input, "shared-cache")
		id := sessionMetadata(body, "task_id")
		if id == "" || seen[id] {
			t.Fatal("different logical conversations shared a task")
		}
		seen[id] = true
		turnID := sessionMetadata(body, "turn_id")
		if turnID == "" || turns[turnID] {
			t.Fatal("different logical conversations shared a turn")
		}
		turns[turnID] = true
	}
}

func TestSessionlessPrepareRemainsCompatible(t *testing.T) {
	raw := []byte(`{"model":"gpt-6-astra","input":"first","prompt_cache_key":"cache"}`)
	legacy, _, err := Prepare(raw, "scope", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range []string{"", "  "} {
		got, _, err := PrepareWithOptions(raw, "scope", nil, PrepareOptions{SessionID: session})
		if err != nil || !bytes.Equal(got, legacy) {
			t.Fatal("sessionless legacy output changed", err)
		}
	}
	a := prepareSessionFixture(t, "scope", "", []any{message("user", "first")}, "")
	b := prepareSessionFixture(t, "scope", "", []any{message("user", "second")}, "")
	if sessionMetadata(a, "task_id") == sessionMetadata(b, "task_id") {
		t.Fatal("sessionless unrelated inputs were merged")
	}
	c := prepareSessionFixture(t, "scope", "", []any{message("user", "first")}, "cache")
	d := prepareSessionFixture(t, "scope", "", []any{message("user", "second")}, "cache")
	if sessionMetadata(c, "task_id") != sessionMetadata(d, "task_id") {
		t.Fatal("legacy cache-key identity changed")
	}
}
