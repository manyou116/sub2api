//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

func TestAccountTestServiceWebImagesUsesSelectedAccountContext(t *testing.T) {
	account := webImageCapacityAccount(42)
	account.Credentials["chatgpt_account_id"] = "selected-account"
	account.Credentials["user_agent"] = "selected-agent"
	account.Extra["openai_device_id"] = "selected-device"
	account.Extra["openai_session_id"] = "selected-session"
	account.Extra["openai_web_images_client_profile"] = "codex_browser"
	repo := &webImgAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	cfg := webImgTestCfg("memory")
	cfg.Gateway.OpenAIWebImages.DefaultModelMode = "auto"
	cfg.Gateway.OpenAIWebImages.DefaultUpstreamModel = "auto"
	cfg.Gateway.OpenAIWebImages.DefaultThinkingEffort = "extended"
	web := NewOpenAIWebImagesService(cfg, nil, repo)
	web.Driver().KeepConversation = true
	calls := map[string]int{}
	web.Driver().ClientFactory = func(string) (*req.Client, error) {
		client := req.C()
		client.GetTransport().WrapRoundTripFunc(func(http.RoundTripper) req.HttpRoundTripFunc {
			return func(r *http.Request) (*http.Response, error) {
				calls[r.URL.Path]++
				if r.URL.Host == "chatgpt.com" {
					require.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
					require.Equal(t, "selected-account", r.Header.Get("ChatGPT-Account-Id"))
					require.Equal(t, "selected-agent", r.Header.Get("User-Agent"))
					require.Equal(t, "selected-device", r.Header.Get("Oai-Did"))
					require.Equal(t, "selected-session", r.Header.Get("Oai-Session-Id"))
					require.Equal(t, "codex_webview", r.Header.Get("X-OpenAI-Web-Frontend"))
				} else {
					require.Empty(t, r.Header.Get("Authorization"))
				}
				body := `{}`
				switch r.URL.Path {
				case "/backend-api/conversation/init":
					body = `{"limits_progress":[{"feature_name":"image_gen","remaining":3}]}`
				case "/backend-api/sentinel/chat-requirements/prepare":
					body = `{"prepare_token":"mock-prepare","proofofwork":{"required":false}}`
				case "/backend-api/sentinel/chat-requirements/finalize":
					body = `{"token":"mock-requirements"}`
				case "/backend-api/f/conversation/prepare", "/backend-api/f/conversation":
					var payload map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
					require.Equal(t, "auto", payload["model"])
					require.NotContains(t, payload, "thinking_effort")
					if strings.HasSuffix(r.URL.Path, "/prepare") {
						body = `{"conduit_token":"mock-conduit"}`
					} else {
						require.Equal(t, "codex_browser", payload["client_contextual_info"].(map[string]any)["app_surface"])
						body = "data: {\"conversation_id\":\"mock-conversation\",\"asset_pointer\":\"file-service://file_output\"}\n\n"
					}
				case "/backend-api/files/download/file_output":
					body = `{"download_url":"https://mock.invalid/image"}`
				case "/image":
					body = "generated-image"
				default:
					t.Errorf("unexpected upstream path %s", r.URL.Path)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
			}
		})
		return client, nil
	}
	svc := &AccountTestService{accountRepo: repo, webImages: web}
	c, recorder := newTestContext()
	err := svc.testOpenAIImageWeb(c, context.Background(), account, "test-token", "gpt-image-2", "draw a red square")
	require.NoError(t, err)
	require.Equal(t, 1, calls["/backend-api/conversation/init"])
	require.Equal(t, 1, calls["/backend-api/f/conversation"])
	require.Contains(t, recorder.Body.String(), `"type":"image"`)
	require.Contains(t, recorder.Body.String(), `"type":"test_complete","success":true`)
	require.False(t, web.IsInflightFull(context.Background(), account), "admin test releases its image slot")
}
