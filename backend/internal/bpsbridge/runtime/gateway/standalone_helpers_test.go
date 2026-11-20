package gateway

import (
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/bpsbridge/runtime/auth"
)

func userGrant(subject, nonce string) auth.DeviceCredentials {
	b, _ := json.Marshal(map[string]any{"sub": subject, "nonce": nonce,
		"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": "shared-workspace"}})
	return auth.DeviceCredentials{ClientID: auth.ExcelClientID, AccessToken: "e30." + base64.RawURLEncoding.EncodeToString(b) + ".sig"}
}

func sessionMessage(role, text string) map[string]any {
	kind := "input_text"
	if role == "assistant" {
		kind = "output_text"
	}
	return map[string]any{"type": "message", "role": role, "content": []any{map[string]any{"type": kind, "text": text}}}
}

func sessionInput(body map[string]any) []any {
	var result []any
	for _, raw := range fixtureValue[[]any](body["input"]) {
		item := fixtureValue[map[string]any](raw)
		if item["role"] != "developer" {
			result = append(result, item)
		}
	}
	return result
}

func sessionTask(body map[string]any) string {
	return fixtureValue[string](fixtureValue[map[string]any](body["metadata"])["task_id"])
}

func auditRollingReceives(t *testing.T, stage string, times []time.Time, rps int) {
	t.Helper()
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	const margin = 20 * time.Millisecond
	for i := rps; i < len(times); i++ {
		span := times[i].Sub(times[i-rps])
		if span < time.Second-margin {
			t.Fatalf("%s: %d arrivals in %s, exceeding %d/s rolling window", stage, rps+1, span, rps)
		}
	}
	t.Logf("%s: checked all %d rolling intervals with %s receive-time tolerance", stage, max(0, len(times)-rps), margin)
}

func TestSlotTaskStableAcrossRestartAndUserBoundaries(t *testing.T) {
	key := strings.Repeat("a", 32)
	base := slotTaskID(key, "user-and-account", 1)
	if base != slotTaskID(key, "user-and-account", 1) {
		t.Fatal("restart changed task identity")
	}
	for _, id := range []string{
		slotTaskID(key, "other-user", 1),
		slotTaskID(key, "user-and-account", 2),
		slotTaskID(strings.Repeat("b", 32), "user-and-account", 1),
	} {
		if id == base {
			t.Fatal("task escaped its installation/account/slot boundary")
		}
	}
}
