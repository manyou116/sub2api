package webdriver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestExecutionModeAndAttachmentAreExplicit(t *testing.T) {
	for _, tc := range []struct {
		name, mode, model, prompt string
		attachment                *bool
		wantAttachment            bool
	}{
		{name: "work defaults to attachment", mode: "work", model: "gpt-5-6-thinking", wantAttachment: true},
		{name: "web suffix does not select work", mode: "web", model: "gpt-5.6-luna-wm"},
		{name: "prompt cannot select attachment", mode: "web", model: "plain-model", prompt: "Give me a downloadable file and download link"},
		{name: "web explicit attachment", mode: "web", model: "plain-model", attachment: boolPointer(true), wantAttachment: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var polls, downloads int
			d := protocolFaultDriver(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/backend-api/sentinel/chat-requirements/prepare":
					return protocolResponse(200, `{"prepare_token":"p"}`), nil
				case "/backend-api/sentinel/chat-requirements/finalize":
					return protocolResponse(200, `{"token":"t"}`), nil
				case "/backend-api/f/conversation/prepare":
					return protocolResponse(200, `{}`), nil
				case "/backend-api/f/conversation":
					var payload map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
					if tc.mode == "work" {
						require.Equal(t, "tpp", payload["conversation_origin"])
						require.Equal(t, "fast", payload["service_tier"])
					} else {
						require.NotContains(t, payload, "conversation_origin")
						require.NotContains(t, payload, "service_tier")
					}
					return protocolResponse(200, "data: {\"conversation_id\":\"cid\",\"asset_pointer\":\"file-service://preview\"}\n\n"), nil
				case "/backend-api/conversation/cid":
					polls++
					if polls == 1 {
						return protocolResponse(200, `{"message":{"content":{"parts":[{"asset_pointer":"file-service://preview"}]}}}`), nil
					}
					return protocolResponse(200, `{"mapping":{"reply":{"message":{"id":"reply","author":{"role":"assistant"},"content":{"parts":["[Download](sandbox:/workspace/image.png)"]}}}}}`), nil
				case "/backend-api/files/download/preview":
					require.False(t, tc.wantAttachment, "preview alone must not satisfy attachment constraint")
					return protocolResponse(200, `{"download_url":"https://example.invalid/image"}`), nil
				case "/backend-api/conversation/cid/interpreter/download":
					require.Equal(t, "/workspace/image.png", r.URL.Query().Get("sandbox_path"))
					fallthrough
				case "/image":
					downloads++
					return protocolResponse(200, string(protocolPNG(t))), nil
				default:
					return nil, fmt.Errorf("unexpected path %s", r.URL.Path)
				}
			})
			d.PollInitialWait, d.PollInterval, d.PollTimeout = 0, time.Millisecond, time.Second
			result, err := d.Generate(context.Background(), Auth{AccessToken: tc.name}, GenerateRequest{Mode: tc.mode, Model: tc.model, Prompt: tc.prompt, RequireDownloadAttachment: tc.attachment})
			require.NoError(t, err)
			require.Equal(t, tc.wantAttachment, result.Meta.DownloadAttachment)
			require.Equal(t, 1, downloads)
			if tc.wantAttachment {
				require.Equal(t, 2, polls)
			} else {
				require.Zero(t, polls)
			}
			require.Equal(t, 2, result.Data[0].Width)
			require.Equal(t, 2, result.Data[0].Height)
			require.Equal(t, "png", result.Data[0].Format)
		})
	}
}

func boolPointer(value bool) *bool { return &value }

func TestInvalidExecutionModeDoesNotContactUpstream(t *testing.T) {
	d := protocolFaultDriver(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid mode contacted upstream")
		return nil, nil
	})
	_, err := d.Generate(context.Background(), Auth{AccessToken: "local"}, GenerateRequest{Mode: "invalid"})
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "mode must be web or work"))
}

func TestWorkQuotaErrorPreservesTypeAndReset(t *testing.T) {
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	for _, body := range []string{
		fmt.Sprintf(`{"error":{"type":"usage_limit_reached","message":"Codex usage limit reached","resets_at":%d}}`, reset.Unix()),
		fmt.Sprintf(`{"error":{"code":"model_quota_exhausted","message":"Model quota exhausted","reset_at":%q}}`, reset.Format(time.RFC3339)),
	} {
		failure := classifyHTTP("sse", http.StatusTooManyRequests, body)
		require.Equal(t, ErrorKindRateLimited, failure.Kind)
		require.NotEmpty(t, failure.Code)
		require.NotNil(t, failure.ResetAt)
		require.WithinDuration(t, reset, *failure.ResetAt, time.Second)
	}
	failure := classifyHTTP("sse", http.StatusTooManyRequests, `{"error":{"type":"usage_limit_reached","resets_in_seconds":120}}`)
	require.Equal(t, "usage_limit_reached", failure.Code)
	require.NotNil(t, failure.ResetAt)
	require.WithinDuration(t, time.Now().Add(120*time.Second), *failure.ResetAt, time.Second)
}
