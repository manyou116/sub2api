package webdriver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

func TestBrowserRoutingGenerationAndEdit(t *testing.T) {
	fixtureBytes, err := os.ReadFile("testdata/codex_browser_request.json")
	require.NoError(t, err)
	var capture map[string]any
	require.NoError(t, json.Unmarshal(fixtureBytes, &capture))
	for _, edit := range []bool{false, true} {
		for _, tt := range []struct {
			name, model, profile, conduit, effort string
		}{
			{name: "auto browser", model: "auto", conduit: "fresh-conduit"},
			{name: "auto codex browser", model: "auto", profile: "codex_browser", conduit: "fresh-conduit"},
			{name: "auto ignores inherited extended", model: "auto", effort: "extended", conduit: "fresh-conduit"},
			{name: "no token is not preparation success", model: "auto"},
			{name: "explicit model and effort", model: "gpt-5.6-luna-wm", effort: "min", conduit: "fresh-conduit"},
		} {
			name := "generation/" + tt.name
			if edit {
				name = "edit/" + tt.name
			}
			t.Run(name, func(t *testing.T) {
				var parentID, traceID string
				var steps []string
				d := NewDriver(func(proxy string) (*req.Client, error) {
					require.Equal(t, "http://mock-proxy.invalid:8080", proxy)
					client := req.C()
					client.GetTransport().WrapRoundTripFunc(func(http.RoundTripper) req.HttpRoundTripFunc {
						return func(r *http.Request) (*http.Response, error) {
							steps = append(steps, r.URL.Path)
							if r.URL.Host == "chatgpt.com" {
								require.Equal(t, "Bearer current-account-token", r.Header.Get("Authorization"))
								require.Equal(t, "current-account", r.Header.Get("ChatGPT-Account-Id"))
								require.Equal(t, "current-device", r.Header.Get("Oai-Did"))
								require.Equal(t, "current-session", r.Header.Get("Oai-Session-Id"))
								require.Equal(t, "mock-user-agent", r.Header.Get("User-Agent"))
								require.Empty(t, r.Header.Get("Cookie"))
								require.Empty(t, r.Header.Get("Oai-Client-Version"))
								require.Empty(t, r.Header.Get("Oai-Client-Build-Number"))
								if tt.profile == "codex_browser" {
									require.Equal(t, "Codex Browser", r.Header.Get("Originator"))
									require.Equal(t, "codex_webview", r.Header.Get("X-OpenAI-Web-Frontend"))
									require.Equal(t, "browser", r.Header.Get("X-OpenAI-Codex-Window-Type"))
								} else {
									require.Empty(t, r.Header.Get("Originator"))
									require.Empty(t, r.Header.Get("X-OpenAI-Web-Frontend"))
								}
							} else {
								require.Empty(t, r.Header.Get("Authorization"), "external file servers must not receive account credentials")
								require.Empty(t, r.Header.Get("ChatGPT-Account-Id"))
							}
							body := `{}`
							switch r.URL.Path {
							case "/backend-api/sentinel/chat-requirements/prepare":
								body = `{"prepare_token":"fresh-prepare","proofofwork":{"required":false}}`
							case "/backend-api/sentinel/chat-requirements/finalize":
								var payload map[string]any
								require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
								require.Equal(t, map[string]any{"prepare_token": "fresh-prepare", "proofofwork": "", "turnstile": ""}, payload)
								body = `{"token":"fresh-requirements"}`
							case "/backend-api/files":
								body = `{"file_id":"file_input","upload_url":"https://mock.invalid/upload"}`
							case "/upload", "/backend-api/files/file_input/uploaded":
							case "/backend-api/f/conversation/prepare", "/backend-api/f/conversation":
								var payload map[string]any
								require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
								require.Equal(t, tt.model, payload["model"])
								require.NotContains(t, payload, "conversation_mode")
								require.Equal(t, "fresh-requirements", r.Header.Get("OpenAI-Sentinel-Chat-Requirements-Token"))
								if strings.HasSuffix(r.URL.Path, "/prepare") {
									require.Equal(t, "sent", payload["client_prepare_state"])
									parentID, _ = payload["parent_message_id"].(string)
									_, err := uuid.Parse(parentID)
									require.NoError(t, err)
									traceID = r.Header.Get("X-Oai-Turn-Trace-Id")
									require.NotEmpty(t, traceID)
									bodyBytes, err := json.Marshal(map[string]string{"status": "ok", "conduit_token": tt.conduit})
									require.NoError(t, err)
									body = string(bodyBytes)
								} else {
									require.Equal(t, parentID, payload["parent_message_id"])
									require.Equal(t, traceID, r.Header.Get("X-Oai-Turn-Trace-Id"))
									require.Equal(t, tt.conduit, r.Header.Get("X-Conduit-Token"))
									if tt.conduit == "" {
										require.Equal(t, "none", payload["client_prepare_state"])
									} else {
										require.Equal(t, capture["client_prepare_state"], payload["client_prepare_state"])
									}
									if tt.model == "auto" {
										require.NotContains(t, payload, "thinking_effort")
									} else {
										require.Equal(t, tt.effort, payload["thinking_effort"])
										require.Equal(t, "tpp", payload["conversation_origin"])
										require.Equal(t, "fast", payload["service_tier"])
									}
									if tt.profile == "codex_browser" {
										require.Equal(t, capture["client_contextual_info"], payload["client_contextual_info"])
									}
									require.Empty(t, payload["local_function_names"], "gateway has no local.continue_in_work implementation")
									body = "data: {\"conversation_id\":\"mock-conversation\"}\n\ndata: {\"v\":{\"message\":{\"metadata\":{\"model_slug\":\"gpt-6\",\"default_model_slug\":\"auto\"},\"content\":{\"parts\":[{\"asset_pointer\":\"file-service://file_output\"}]}}}}\n\n"
								}
							case "/backend-api/files/download/file_output":
								body = `{"download_url":"https://mock.invalid/image"}`
							case "/image":
								body = string(protocolPNG(t))
							default:
								t.Errorf("unexpected path %s", r.URL.Path)
							}
							return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
						}
					})
					return client, nil
				})
				d.KeepConversation = true
				auth := Auth{AccessToken: "current-account-token", ProxyURL: "http://mock-proxy.invalid:8080", AccountID: "current-account", DeviceID: "current-device", SessionID: "current-session", UserAgent: "mock-user-agent", ClientProfile: tt.profile}
				mode := "web"
				if strings.HasSuffix(tt.model, "-wm") {
					mode = "work"
				}
				attachment := false // This test focuses on routing, not the mode's output default.
				request := GenerateRequest{Prompt: "draw", Model: tt.model, Mode: mode, RequireDownloadAttachment: &attachment, ThinkingEffort: tt.effort, N: 1}
				var result *GenerateResult
				var err error
				if edit {
					request.Images = []InputImage{{Data: []byte("input-image"), FileName: "input.png"}}
					result, err = d.Edit(context.Background(), auth, request)
				} else {
					result, err = d.Generate(context.Background(), auth, request)
				}
				require.NoError(t, err)
				require.Len(t, result.Data, 1)
				require.Equal(t, base64.StdEncoding.EncodeToString(protocolPNG(t)), result.Data[0].B64JSON)
				require.Contains(t, steps, "/backend-api/f/conversation/prepare")
				require.Contains(t, steps, "/backend-api/f/conversation")
				if edit {
					require.Contains(t, steps, "/upload")
				}
			})
		}
	}
}
