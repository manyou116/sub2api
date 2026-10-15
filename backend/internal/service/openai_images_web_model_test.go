package service

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIWebImagesOutboundModelAndBilling(t *testing.T) {
	for _, endpoint := range []string{openAIImagesGenerationsEndpoint, openAIImagesEditsEndpoint} {
		for _, tt := range []struct {
			name, globalMode, globalModel, accountModel, wantModel string
		}{
			{name: "global auto", globalMode: "auto", globalModel: "auto", wantModel: "auto"},
			{name: "global fixed auto", globalMode: "fixed", globalModel: "auto", wantModel: "auto"},
			{name: "account fixed auto", accountModel: "auto", wantModel: "auto"},
			{name: "explicit global model", globalMode: "fixed", globalModel: "gpt-5-6-thinking", wantModel: "gpt-5-6-thinking"},
			{name: "explicit account model", globalMode: "fixed", globalModel: "auto", accountModel: "gpt-5.6-luna-wm", wantModel: "gpt-5.6-luna-wm"},
		} {
			t.Run(endpoint+"/"+tt.name, func(t *testing.T) {
				account := webImageCapacityAccount(42)
				account.Credentials["chatgpt_account_id"] = "current-account"
				account.Credentials["user_agent"] = "current-account-agent"
				account.Extra["openai_device_id"] = "current-device"
				account.Extra["openai_session_id"] = "current-session"
				account.Extra["openai_web_images_client_profile"] = "codex_browser"
				if tt.accountModel != "" {
					account.Extra["openai_web_images"] = map[string]any{
						"enabled": true, "max_inflight": 1, "model_mode": "fixed", "model": tt.accountModel,
					}
				}
				repo := &webImgAccountRepo{accounts: map[int64]*Account{account.ID: account}}
				cfg := webImgTestCfg("memory")
				cfg.Gateway.OpenAIWebImages.DefaultModelMode = tt.globalMode
				cfg.Gateway.OpenAIWebImages.DefaultUpstreamModel = tt.globalModel
				web := NewOpenAIWebImagesService(cfg, nil, repo)
				web.Driver().KeepConversation = true
				calls := map[string]int{}
				web.Driver().ClientFactory = func(string) (*req.Client, error) {
					client := req.C()
					client.GetTransport().WrapRoundTripFunc(func(http.RoundTripper) req.HttpRoundTripFunc {
						return func(r *http.Request) (*http.Response, error) {
							calls[r.URL.Path]++
							if r.URL.Host == "chatgpt.com" {
								require.Equal(t, "current-account", r.Header.Get("ChatGPT-Account-Id"))
								require.Equal(t, "current-device", r.Header.Get("Oai-Did"))
								require.Equal(t, "current-session", r.Header.Get("Oai-Session-Id"))
								require.Equal(t, "current-account-agent", r.Header.Get("User-Agent"))
								require.Equal(t, "codex_webview", r.Header.Get("X-OpenAI-Web-Frontend"))
							}
							body := `{}`
							status := http.StatusOK
							switch r.URL.Path {
							case "/backend-api/conversation/init":
								body = `{"limits_progress":[{"feature_name":"image_gen","remaining":3}]}`
							case "/backend-api/sentinel/chat-requirements/prepare", "/backend-api/sentinel/chat-requirements/finalize":
								body = `{"token":"mock","proofofwork":{"required":false}}`
							case "/backend-api/files":
								body = `{"file_id":"file_input","upload_url":"https://mock.invalid/upload"}`
							case "/backend-api/f/conversation/prepare", "/backend-api/f/conversation":
								var payload struct {
									Model              string `json:"model"`
									ThinkingEffort     string `json:"thinking_effort"`
									ConversationOrigin string `json:"conversation_origin"`
									ServiceTier        string `json:"service_tier"`
									Messages           []struct {
										Content struct {
											ContentType string `json:"content_type"`
										} `json:"content"`
									} `json:"messages"`
								}
								require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
								if payload.Model != tt.wantModel {
									status, body = http.StatusNotFound, `{"detail":"Model not found"}`
									break
								}
								if r.URL.Path == "/backend-api/f/conversation/prepare" {
									body = `{"conduit_token":"mock-conduit"}`
								} else {
									if tt.wantModel == "auto" {
										require.Empty(t, payload.ThinkingEffort)
									} else {
										require.Equal(t, "extended", payload.ThinkingEffort)
									}
									if tt.wantModel == "gpt-5.6-luna-wm" {
										require.Equal(t, "tpp", payload.ConversationOrigin)
										require.Equal(t, "fast", payload.ServiceTier)
									}
									require.Len(t, payload.Messages, 1)
									if endpoint == openAIImagesEditsEndpoint {
										require.Equal(t, "multimodal_text", payload.Messages[0].Content.ContentType)
									}
									body = "data: {\"conversation_id\":\"mock-conversation\",\"asset_pointer\":\"file-service://file_00000000aaaaaaaaaaaaaaaaaaaaaaaa\"}\n\n"
								}
							case "/backend-api/files/download/file_00000000aaaaaaaaaaaaaaaaaaaaaaaa":
								body = `{"download_url":"https://mock.invalid/image"}`
							case "/image":
								body = "generated-image-bytes"
							case "/upload", "/backend-api/files/file_input/uploaded":
							default:
								t.Errorf("unexpected upstream path: %s", r.URL.Path)
							}
							return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
						}
					})
					return client, nil
				}
				parsed := &OpenAIImagesRequest{Endpoint: endpoint, Model: "gpt-image-2", Prompt: "draw", N: 1, Size: "1024x1024", SizeTier: "1K"}
				if parsed.IsEdits() {
					parsed.Uploads = []OpenAIImagesUpload{{FileName: "input.png", ContentType: "image/png", Data: []byte("input-image")}}
				}
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, endpoint, nil)
				gw := &OpenAIGatewayService{webImages: web}
				result, err := gw.forwardOpenAIImagesLegacyWeb(c.Request.Context(), c, account, parsed, "")
				require.NoError(t, err, recorder.Body.String())
				require.Equal(t, http.StatusOK, recorder.Code)
				require.Equal(t, "Z2VuZXJhdGVkLWltYWdlLWJ5dGVz", gjson.GetBytes(recorder.Body.Bytes(), "data.0.b64_json").String())
				require.Equal(t, "gpt-image-2", result.Model, "billing/log model must remain the requested image model")
				require.Equal(t, tt.wantModel, result.UpstreamModel, "record the model actually sent upstream")
				require.Equal(t, 1, result.ImageCount)
				require.Equal(t, "1K", result.ImageSize)
				require.Equal(t, 1, calls["/backend-api/conversation/init"], "probe uses the same account context as generation/edit")
				require.Equal(t, 1, calls["/backend-api/f/conversation/prepare"])
				require.Equal(t, 1, calls["/backend-api/f/conversation"])
				if parsed.IsEdits() {
					require.Equal(t, 1, calls["/upload"])
				}
			})
		}
	}
}

func TestOpenAIWebImagesAuthUsesSelectedAccountContext(t *testing.T) {
	a := webImageCapacityAccount(42)
	b := webImageCapacityAccount(43)
	first := openAIWebImagesAuth(a, "fresh-token", "http://proxy.invalid")
	require.Equal(t, "fresh-token", first.AccessToken)
	require.Equal(t, "http://proxy.invalid", first.ProxyURL)
	require.Empty(t, first.AccountID)
	require.Empty(t, first.ClientProfile, "Codex Browser context must be opt-in")
	require.NotEmpty(t, first.DeviceID)
	require.Equal(t, first.DeviceID, openAIWebImagesAuth(a, "refreshed-token", "").DeviceID)
	require.NotEqual(t, first.DeviceID, openAIWebImagesAuth(b, "other-token", "").DeviceID)
}
