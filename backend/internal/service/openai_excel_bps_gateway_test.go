//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type excelTestRepository struct {
	AccountRepository
	forkOpenAIConnectionRepository
	account *Account
	record  ForkOpenAIConnection
}

func (r *excelTestRepository) GetByID(context.Context, int64) (*Account, error) {
	return r.account, nil
}

func (r *excelTestRepository) ReadOpenAIConnection(_ context.Context, _ int64, kind string) (ForkOpenAIConnection, error) {
	if kind != excelConnectionKind {
		return ForkOpenAIConnection{}, nil
	}
	return r.record, nil
}

type excelTestHTTP struct {
	HTTPUpstream
	requests []*http.Request
	response func(*http.Request) *http.Response
}

func (u *excelTestHTTP) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.requests = append(u.requests, req)
	return u.response(req), nil
}

func excelTestGateway(t *testing.T, upstream HTTPUpstream) (*OpenAIGatewayService, *Account) {
	t.Helper()
	account := &Account{
		ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Extra:       map[string]any{openAIConnectionProtocolKey: excelConnectionKind},
		Credentials: map[string]any{"access_token": "primary-codex", "email": "user@example.test", "chatgpt_account_id": "workspace"},
	}
	cipher := &liveAttestationAES{key: [32]byte{1}}
	raw, err := json.Marshal(siwcGrant{
		AccessToken: "excel-access", RefreshToken: "excel-refresh", ClientID: openai.ExcelClientID,
		Email: "user@example.test", Workspace: "workspace", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	encrypted, err := cipher.Encrypt(string(raw))
	require.NoError(t, err)
	store := &excelTestRepository{account: account, record: ForkOpenAIConnection{HostID: "test", Ciphertext: encrypted, Revision: 1}}
	redisClient := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = redisClient.Close() })
	oauth := &OpenAIOAuthService{connections: &forkOpenAIConnections{accounts: store, repo: store, encryptor: cipher, redis: redisClient}}
	return &OpenAIGatewayService{httpUpstream: upstream, openAITokenProvider: &OpenAITokenProvider{openAIOAuthService: oauth}}, account
}

func TestExcelBPSForwardWireAndUsage(t *testing.T) {
	upstream := &excelTestHTTP{response: func(req *http.Request) *http.Response {
		data, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		require.Equal(t, basispoints.ResponsesURL, req.URL.String())
		require.Equal(t, "Bearer excel-access", req.Header.Get("Authorization"))
		require.Equal(t, "workspace", req.Header.Get("Chatgpt-Account-Id"))
		require.Empty(t, req.Header.Get("X-Codex-Turn-State"))
		require.True(t, gjson.GetBytes(data, "stream").Bool())
		require.False(t, gjson.GetBytes(data, "service_tier").Exists())
		header := http.Header{"Content-Type": {"text/event-stream"}}
		body := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_bps\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-5.6-sol\",\"output\":[],\"usage\":{\"input_tokens\":7,\"output_tokens\":2}}}\n\n"
		return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(body))}
	}}
	s, a := excelTestGateway(t, upstream)
	for _, path := range []string{"/v1/responses", "/v1/chat/completions"} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, path, nil)
		c.Request.Header.Set("X-Codex-Turn-State", "must-not-forward")
		var result *OpenAIForwardResult
		var err error
		if strings.HasSuffix(path, "/chat/completions") {
			result, err = s.ForwardAsChatCompletions(c.Request.Context(), c, a, []byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"hi"}]}`), "", "")
		} else {
			result, err = s.Forward(c.Request.Context(), c, a, []byte(`{"model":"gpt-5.6-sol","input":"hi","service_tier":"priority"}`))
		}
		require.NoError(t, err, rec.Body.String())
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, 7, result.Usage.InputTokens)
		require.Equal(t, 2, result.Usage.OutputTokens)
		require.Equal(t, excelBPSResponsesEndpoint, result.UpstreamEndpoint)
	}
	require.Len(t, upstream.requests, 2)
}

func TestExcelBPSImageWireAndNoReplay(t *testing.T) {
	upstream := &excelTestHTTP{response: func(req *http.Request) *http.Response {
		require.Equal(t, basispoints.ImagesGenerationsURL, req.URL.String())
		require.Equal(t, "Bearer excel-access", req.Header.Get("Authorization"))
		require.Equal(t, "Excel", req.Header.Get("X-Openai-Internal-Basispoints-Office-Host"))
		require.True(t, HTTPUpstreamRedirectsDisabled(req.Context()))
		return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader("{}"))}
	}}
	s, a := excelTestGateway(t, upstream)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	parsed := &OpenAIImagesRequest{Model: "gpt-image-2", Prompt: "A blue dot", N: 1}
	_, err := s.ForwardImages(c.Request.Context(), c, a, nil, parsed, "")
	require.Error(t, err)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Len(t, upstream.requests, 1)
}

func TestExcelBPSBrowserCallbackHostAndClient(t *testing.T) {
	code, id, err := parseOpenAIConnectionCallback(openai.ExcelRedirectURI+"?code=fixture&state=test", "test", "", excelConnectionKind)
	require.NoError(t, err)
	require.Equal(t, "fixture", code)
	require.Equal(t, openai.ExcelClientID, id)
	_, _, err = parseOpenAIConnectionCallback(openai.ExcelRedirectURI+"?code=fixture&state=test&client_id=wrong", "test", "", excelConnectionKind)
	require.Error(t, err)
}

func TestExcelBPSModelSelectionIsRequestLocal(t *testing.T) {
	a := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
		openAIConnectionProtocolKey: excelConnectionKind, excelBPSModelsKey: []any{"assigned"},
	}}
	require.True(t, chooseOpenAIConnectionForModel(a, "assigned").IsExcelBPSEnabled())
	require.True(t, chooseOpenAIConnectionForModel(a, "different").UsesOpenAICodexProtocol())
	require.True(t, a.IsExcelBPSEnabled())
}

func TestExcelBPSUploadAttachmentBeforeResponses(t *testing.T) {
	var imageBody bytes.Buffer
	require.NoError(t, png.Encode(&imageBody, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	url := "data:image/png;base64," + base64.StdEncoding.EncodeToString(imageBody.Bytes())
	upstream := &excelTestHTTP{response: func(req *http.Request) *http.Response {
		if req.URL.String() == basispoints.AttachmentsURL {
			require.Contains(t, req.Header.Get("Content-Type"), "multipart/form-data")
			require.True(t, HTTPUpstreamRedirectsDisabled(req.Context()))
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"openai_file_id":"file-bps-test"}`))}
		}
		require.Equal(t, basispoints.ResponsesURL, req.URL.String())
		body, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		require.Contains(t, string(body), "file-bps-test")
		require.NotContains(t, string(body), url)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(`data: {"type":"response.completed","response":{"id":"resp_bps","status":"completed","output":[]}}` + "\n\n"))}
	}}
	s, account := excelTestGateway(t, upstream)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	body := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"` + url + `"}]}]}`)
	_, err := s.Forward(c.Request.Context(), c, account, body)
	require.NoError(t, err, recorder.Body.String())
	require.Len(t, upstream.requests, 2)
}
