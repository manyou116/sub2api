//go:build unit

package service

import (
	"bytes"
	"context"
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
)

func TestBPSAccountConnection(t *testing.T) {
	for _, tc := range []struct{ name, terminal string }{
		{"completed", `{"type":"response.completed"}`},
		{"failed", `{"type":"response.failed","response":{"error":{"message":"private upstream detail"}}}`},
		{"truncated", ""},
		{"stalled", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := make(chan string, 2)
			bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, "read failed", http.StatusBadRequest)
					return
				}
				requests <- r.Method + " " + r.URL.Path + "\n" + string(body)
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("X-BPS-Queue-Ms", "25")
				w.Header().Set("X-BPS-Ws-Dial-Start-Unix-Ms", "1000")
				w.Header().Set("X-BPS-Ws-Dial-Done-Unix-Ms", "1120")
				fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n")
				if tc.name == "stalled" {
					w.(http.Flusher).Flush()
					select {
					case <-r.Context().Done():
					case <-time.After(10 * time.Second):
					}
					return
				}
				if tc.terminal != "" {
					fmt.Fprintf(w, "data: %s\n\n", tc.terminal)
				}
			}))
			t.Cleanup(bridge.Close)
			dir := filepath.Join(t.TempDir(), "bps")
			require.NoError(t, os.Mkdir(dir, 0700))
			t.Setenv("SUB2API_BPS_STATE_DIR", dir)
			repo := newSparkShadowRepoStub()
			parent := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
				Credentials: map[string]any{"chatgpt_account_id": "workspace", "chatgpt_user_id": "user"}}
			require.NoError(t, repo.Create(t.Context(), parent))
			shadow := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &parent.ID,
				QuotaDimension: quotaDimensionBPS, Concurrency: 1,
				Credentials: map[string]any{"model_mapping": map[string]any{"bps-text": "gpt-5.4", "image-alias": "gpt-image-2"}}}
			require.NoError(t, repo.Create(t.Context(), shadow))
			t.Cleanup(func() {
				if entry, ok := bpsWorkers.LoadAndDelete(shadow.ID); ok {
					slot := entry.(*bpsRuntimeWorker)
					slot.mu.Lock()
					defer slot.mu.Unlock()
					if slot.idle != nil {
						slot.idle.Stop()
					}
					if slot.worker != nil {
						slot.worker.Stop()
					}
				}
			})
			svc := &AccountTestService{accountRepo: repo}
			models, err := svc.FetchOpenAIAccountModels(t.Context(), shadow)
			require.NoError(t, err)
			require.Len(t, models, 1)
			require.Equal(t, "bps-text", models[0].ID)
			run := func(mode string, background bool) (string, error) {
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil)
				c.Set(accountTestBackgroundKey, background)
				err := svc.TestAccountConnection(c, shadow.ID, "bps-text", "", mode)
				require.Equal(t, "text/event-stream", w.Header().Get("Content-Type"))
				return w.Body.String(), err
			}
			_, err = run(AccountTestModeDefault, false)
			require.ErrorContains(t, err, "authorization is missing or expired")
			_, err = run(AccountTestModeCompact, false)
			require.ErrorContains(t, err, "standard test mode")
			_, err = run(AccountTestModeDefault, true)
			require.ErrorContains(t, err, "manual connection tests only")
			process, err := bpsRuntime(shadow.ID)
			require.NoError(t, err)
			require.NoError(t, os.Mkdir(process.Dir, 0700))
			require.NoError(t, bpsbridge.Save(bpsGrantPath(process), bpsbridge.BoundGrant{
				Grant: bpsbridge.Grant{AccessToken: "fixture-only"}, Identity: bpsbridge.Identity{Account: "workspace", User: "user"},
				ExpiresAt: time.Now().Add(time.Hour),
			}))
			bpsWorkers.Store(shadow.ID, &bpsRuntimeWorker{
				key:    bpsRuntimeKey{shadowID: shadow.ID, dir: process.Dir, concurrency: shadow.Concurrency},
				worker: bpsTestWorker{endpoint: bridge.URL},
			})
			output, err := run(AccountTestModeDefault, false)
			require.Contains(t, output, `"text":"OK"`)
			require.Contains(t, output, `"status":"bps_connected"`)
			require.Contains(t, output, "queue=25ms")
			require.Contains(t, output, "ws_dial_span=120ms")
			require.Contains(t, output, "first_text=")
			switch tc.name {
			case "completed":
				require.NoError(t, err)
				require.Contains(t, output, `"success":true`)
				require.Contains(t, output, "completed=")
			case "failed":
				require.ErrorContains(t, err, "upstream reported a failed response")
				require.NotContains(t, output, `"success":true`)
				require.NotContains(t, output, "private upstream detail")
			default:
				require.ErrorIs(t, err, errBPSProbeConnectedOnly)
				require.Contains(t, output, `"status":"bps_connected_only"`)
				require.NotContains(t, output, `"success":true`)
			}
			require.Len(t, requests, 1)
			requestLine, body, _ := strings.Cut(<-requests, "\n")
			require.Equal(t, "POST /v1/responses", requestLine)
			require.JSONEq(t, `{"model":"gpt-5.4","input":"Reply with only OK.","stream":true,"store":false,"reasoning":{"effort":"low"}}`, body)
		})
	}
}

type bpsTestWorker struct{ endpoint string }

func (w bpsTestWorker) Stop()                 {}
func (w bpsTestWorker) Done() <-chan struct{} { return nil }
func (w bpsTestWorker) Responses(ctx context.Context, _ bpsbridge.BoundGrant, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.endpoint+"/v1/responses", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}
