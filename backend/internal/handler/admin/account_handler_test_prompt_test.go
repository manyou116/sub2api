package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type promptTestAccountRepo struct {
	service.AccountRepository
	account *service.Account
	readIDs []int64
}

func (r *promptTestAccountRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	r.readIDs = append(r.readIDs, id)
	return r.account, nil
}

type promptTestUpstream struct {
	service.HTTPUpstream
	request     *http.Request
	body        []byte
	proxyURL    string
	accountID   int64
	concurrency int
	calls       int
}

func (u *promptTestUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	u.request, u.proxyURL, u.accountID, u.concurrency = req, proxyURL, accountID, concurrency
	u.calls++
	var err error
	u.body, err = io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"PROBE_\"}\n\n" +
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ALPHA_731\"}\n\n" +
			"data: {\"type\":\"response.completed\"}\n\n")),
	}, nil
}

func TestAccountHandlerTestOpenAIResponsesPrompt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, accountType := range []string{service.AccountTypeOAuth, service.AccountTypeAPIKey} {
		for _, tc := range []struct {
			name     string
			prompt   string
			omit     bool
			expected string
		}{
			{name: "missing", omit: true, expected: "hi"},
			{name: "empty", expected: "hi"},
			{name: "whitespace", prompt: " \t\r\n\u3000", expected: "hi"},
			{name: "alpha", prompt: "Only output PROBE_ALPHA_731", expected: "Only output PROBE_ALPHA_731"},
			{name: "beta", prompt: "Only output PROBE_BETA_924", expected: "Only output PROBE_BETA_924"},
			{name: "unicode_multiline_quotes", prompt: "  第一行：\"你好\"\n第二行：\\路径\t结束  ", expected: "  第一行：\"你好\"\n第二行：\\路径\t结束  "},
		} {
			t.Run(accountType+"/"+tc.name, func(t *testing.T) {
				proxyID := int64(7)
				account := &service.Account{
					ID: 42, Platform: service.PlatformOpenAI, Type: accountType,
					Concurrency: 3, ProxyID: &proxyID,
					Proxy: &service.Proxy{Protocol: "http", Host: "proxy.example.com", Port: 8080},
					Credentials: map[string]any{
						"access_token": "test-oauth-token", "api_key": "test-api-key",
						"base_url": "https://api.example.com", "chatgpt_account_id": "test-chatgpt-account",
						"model_mapping": map[string]any{"quality-probe": "gpt-6-astra"},
					},
					Extra: map[string]any{openai_compat.ExtraKeyResponsesSupported: true},
				}
				repo := &promptTestAccountRepo{account: account}
				upstream := &promptTestUpstream{}
				svc := service.NewAccountTestService(repo, nil, nil, nil, nil, upstream, &config.Config{}, nil)
				handler := &AccountHandler{accountTestService: svc}
				router := gin.New()
				router.POST("/api/v1/admin/accounts/:id/test", handler.Test)

				input := map[string]any{"model_id": "quality-probe"}
				if !tc.omit {
					input["prompt"] = tc.prompt
				}
				body, err := json.Marshal(input)
				require.NoError(t, err)
				req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/42/test", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)

				require.Equal(t, http.StatusOK, rec.Code)
				require.Equal(t, []int64{42}, repo.readIDs)
				require.Equal(t, 1, upstream.calls)
				require.Equal(t, account.ID, upstream.accountID)
				require.Equal(t, account.Concurrency, upstream.concurrency)
				require.Equal(t, account.Proxy.URL(), upstream.proxyURL)
				require.Equal(t, http.MethodPost, upstream.request.Method)
				require.Equal(t, "application/json", upstream.request.Header.Get("Content-Type"))
				expected := map[string]any{
					"model": "gpt-6-astra", "stream": true, "instructions": openai.DefaultInstructions,
					"input": []any{map[string]any{"role": "user", "content": []any{
						map[string]any{"type": "input_text", "text": tc.expected},
					}}},
				}
				if accountType == service.AccountTypeOAuth {
					expected["store"] = false
					require.Equal(t, "https://chatgpt.com/backend-api/codex/responses", upstream.request.URL.String())
					require.Equal(t, "Bearer test-oauth-token", upstream.request.Header.Get("Authorization"))
					require.Equal(t, "test-chatgpt-account", upstream.request.Header.Get("Chatgpt-Account-Id"))
				} else {
					require.Equal(t, "https://api.example.com/v1/responses", upstream.request.URL.String())
					require.Equal(t, "Bearer test-api-key", upstream.request.Header.Get("Authorization"))
				}
				var actual map[string]any
				require.NoError(t, json.Unmarshal(upstream.body, &actual))
				require.Equal(t, expected["input"], actual["input"])
				require.Equal(t, expected, actual)

				require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
				var events []service.TestEvent
				for _, line := range strings.Split(rec.Body.String(), "\n") {
					if data, ok := strings.CutPrefix(line, "data: "); ok {
						var event service.TestEvent
						require.NoError(t, json.Unmarshal([]byte(data), &event))
						events = append(events, event)
					}
				}
				require.Equal(t, []service.TestEvent{
					{Type: "test_start", Model: "gpt-6-astra"},
					{Type: "content", Text: "PROBE_"},
					{Type: "content", Text: "ALPHA_731"},
					{Type: "test_complete", Success: true},
				}, events)
			})
		}
	}
}
