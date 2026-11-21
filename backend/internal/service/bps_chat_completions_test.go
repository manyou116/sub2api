//go:build unit

package service

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/bpsbridge"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBPSChatSchedulingEligibility(t *testing.T) {
	parent := int64(1)
	shadow := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &parent, QuotaDimension: quotaDimensionBPS}
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{"messages":[{"role":"user","content":"hello"}]}`, true},
		{`{"messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"ping"}}]}`, true},
		{`{"input":"hello"}`, false},
		{`{"messages":[]}`, false},
		{`{"messages":[{}],"n":2}`, false},
		{`{"messages":[{}],"previous_response_id":"resp_prev"}`, false},
		{`{"messages":[{}],"tools":[{"type":"web_search"}]}`, false},
	} {
		ctx := WithBPSChatCompletions(t.Context(), "gpt-6-luna", []byte(tc.body))
		require.Equal(t, tc.want, !bpsRouteMismatch(ctx, shadow, "gpt-6-luna", false), tc.body)
	}
}

func TestBPSChatCompletionsThroughEmbeddedWorker(t *testing.T) {
	const created = `{"type":"response.created","response":{"id":"resp_bps","model":"gpt-6-luna"}}`
	const delta = `{"type":"response.output_text.delta","delta":"hello"}`
	const completed = `{"type":"response.completed","response":{"id":"resp_bps","model":"gpt-6-luna","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`
	const toolItem = `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_ping","name":"ping"}}`
	const toolDelta = `{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{}"}`
	const toolCompleted = `{"type":"response.completed","response":{"id":"resp_bps","model":"gpt-6-luna","status":"completed","output":[{"type":"function_call","name":"ping","call_id":"call_ping","arguments":"{}"}]}}`
	const failed = `{"type":"response.failed","response":{"status":"failed","error":{"message":"private upstream detail"}}}`

	for _, tc := range []struct {
		name         string
		stream, tool bool
		frames       []string
		wantError    string
	}{
		{name: "buffered", frames: []string{created, delta, completed}},
		{name: "stream", stream: true, frames: []string{created, delta, completed}},
		{name: "function-stream", stream: true, tool: true, frames: []string{created, toolItem, toolDelta, toolCompleted}},
		{name: "function-buffered", tool: true, frames: []string{created, toolItem, toolDelta, toolCompleted}},
		{name: "failure-after-output", stream: true, frames: []string{created, delta, failed}, wantError: "bps_generation_failed"},
		{name: "disconnect", stream: true, frames: []string{created, delta}, wantError: "bps_result_unconfirmed"},
		{name: "failure-before-output", frames: []string{failed}, wantError: "bps_generation_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := make(chan []byte, 4)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, "fixture read failed", http.StatusBadRequest)
					return
				}
				requests <- body
				w.Header().Set("Content-Type", "text/event-stream")
				for _, frame := range tc.frames {
					_, _ = fmt.Fprintf(w, "data: %s\n\n", frame)
				}
			}))
			defer upstream.Close()
			stateDir := filepath.Join(t.TempDir(), "bps")
			require.NoError(t, os.Mkdir(stateDir, 0700))
			t.Setenv("SUB2API_BPS_STATE_DIR", stateDir)
			repo := newSparkShadowRepoStub()
			parent := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
				Credentials: map[string]any{"chatgpt_account_id": "workspace", "chatgpt_user_id": "user"}}
			require.NoError(t, repo.Create(t.Context(), parent))
			shadow := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &parent.ID,
				QuotaDimension: quotaDimensionBPS, Concurrency: 1,
				Credentials: map[string]any{"model_mapping": map[string]any{"bps-test": "gpt-6-luna"}}}
			require.NoError(t, repo.Create(t.Context(), shadow))
			runtime, err := bpsRuntime(shadow.ID)
			require.NoError(t, err)
			require.NoError(t, os.Mkdir(runtime.Dir, 0700))
			require.NoError(t, bpsbridge.Save(bpsGrantPath(runtime), bpsbridge.BoundGrant{
				Grant: bpsbridge.Grant{AccessToken: "fixture-token"}, Identity: bpsbridge.Identity{Account: "workspace", User: "user"},
				ExpiresAt: time.Now().Add(time.Hour),
			}))
			slot := &bpsRuntimeWorker{key: bpsRuntimeKey{shadowID: shadow.ID, dir: runtime.Dir, concurrency: shadow.Concurrency},
				worker: bpsTestWorker{endpoint: upstream.URL}}
			bpsWorkers.Store(shadow.ID, slot)
			t.Cleanup(func() {
				bpsWorkers.Delete(shadow.ID)
				slot.mu.Lock()
				defer slot.mu.Unlock()
				if slot.idle != nil {
					slot.idle.Stop()
				}
			})
			body := fmt.Appendf(nil, `{"model":"bps-test","stream":%t,"messages":[{"role":"user","content":"hello"}]`, tc.stream)
			if tc.tool {
				body = append(body, `,"tools":[{"type":"function","function":{"name":"ping"}}]`...)
			}
			body = append(body, '}')
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			ctx := WithBPSChatCompletions(t.Context(), "bps-test", body)
			c.Request = httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
			svc := &OpenAIGatewayService{accountRepo: repo}
			result, err := svc.ForwardAsChatCompletions(ctx, c, shadow, body, "", "")
			require.Len(t, requests, 1, "submit the generation exactly once")
			sent := gjson.ParseBytes(<-requests)
			require.Equal(t, "gpt-6-luna", sent.Get("model").String())
			require.Equal(t, "hello", sent.Get("input.0.content").String())
			require.True(t, sent.Get("stream").Bool(), "BPS upstream always streams")
			require.False(t, sent.Get("store").Bool())
			if tc.tool {
				require.Equal(t, "ping", sent.Get("tools.0.name").String())
			}
			output := w.Body.String()
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				var failover *UpstreamFailoverError
				require.False(t, errors.As(err, &failover))
				require.Nil(t, result)
				require.Contains(t, output, tc.wantError)
				require.NotContains(t, output, "private upstream detail")
				require.NotContains(t, output, `"finish_reason":"stop"`)
				if tc.stream {
					require.Equal(t, 1, strings.Count(output, "data: [DONE]"))
				} else {
					require.Equal(t, http.StatusBadGateway, w.Code)
				}
				return
			}
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, w.Code)
			require.Equal(t, "bps-test", result.Model)
			require.Equal(t, "gpt-6-luna", result.UpstreamModel)
			require.Equal(t, "/bps/responses", result.UpstreamEndpoint)
			require.Equal(t, tc.stream, result.Stream)
			require.Equal(t, "resp_bps", result.ResponseID)
			if tc.tool {
				require.Contains(t, output, `"finish_reason":"tool_calls"`)
				require.Contains(t, output, "call_ping")
			} else {
				require.Contains(t, output, "hello")
				require.Equal(t, 3, result.Usage.InputTokens)
				require.Equal(t, 2, result.Usage.OutputTokens)
			}
			if tc.stream {
				require.Contains(t, output, "chat.completion.chunk")
				require.Equal(t, 1, strings.Count(output, "data: [DONE]"))
				require.NotContains(t, output, "response.output_text.delta")
			} else {
				require.Equal(t, "chat.completion", gjson.Get(output, "object").String())
			}
		})
	}
}
